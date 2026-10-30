package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUACacheBudgetsAndDisabled(t *testing.T) {
	for _, c := range []*UACache{nil, NewUACache(0, 1024), NewUACache(10, 0)} {
		c.Add("Linux", "Android")
		if _, ok := c.Get("Linux"); ok {
			t.Fatal("disabled cache returned a hit")
		}
	}
	c := NewUACache(1000, 512)
	for i := 0; i < 100; i++ {
		c.Add(fmt.Sprint(i), "Android")
	}
	if _, ok := c.Get("0"); ok {
		t.Fatal("old entry survived budget eviction")
	}
	if _, ok := c.Get("99"); !ok {
		t.Fatal("new entry not cached")
	}
	if n, size := c.Snapshot(); n == 0 || size > 512 {
		t.Fatalf("entries=%d bytes=%d", n, size)
	}
	c.Add("99", "unchanged")
	if value, _ := c.Get("99"); value != "unchanged" {
		t.Fatal("update failed")
	}
	n, size := c.Snapshot()
	c.Add(strings.Repeat("x", 2049), "Android")
	c.Add("large replacement", strings.Repeat("x", 2049))
	if n2, size2 := c.Snapshot(); n != n2 || size != size2 {
		t.Fatal("oversized UA was retained")
	}
	c = NewUACache(2, 4096)
	c.Add("a", "a")
	c.Add("b", "b")
	c.Get("a")
	c.Add("c", "c")
	if _, ok := c.Get("b"); ok {
		t.Fatal("LRU order not respected")
	}
}

func TestUACacheConcurrentAccess(t *testing.T) {
	c := NewUACache(20, 2048)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				key := fmt.Sprintf("Linux/%d/%d", i, j%30)
				c.Add(key, "Android")
				c.Get(key)
				if n, size := c.Snapshot(); n > 20 || size > 2048 {
					t.Errorf("over budget: %d %d", n, size)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestFirewallManagerOnlyWhenNeeded(t *testing.T) {
	if configuredFirewallManager(&Config{}) != nil {
		t.Fatal("disabled firewall allocated manager")
	}
	for _, cfg := range []*Config{{EnableFirewallUABypass: true}, {FirewallUAWhitelist: []string{"test"}}} {
		if configuredFirewallManager(cfg) == nil {
			t.Fatal("enabled feature has no manager")
		}
	}
}

func boundedReader(wire string, bufferSize int) (*bufio.Reader, *headerBudgetReader) {
	budget := &headerBudgetReader{r: strings.NewReader(wire)}
	return bufio.NewReaderSize(budget, bufferSize), budget
}

func TestHeaderByteLimitIncludingPrefetchedData(t *testing.T) {
	const limit = 1024
	base := "GET / HTTP/1.1\r\nHost: example.test\r\nX-Test: \r\n\r\n"
	for _, bufferSize := range []int{128, 4096} {
		for _, size := range []int{limit - 1, limit, limit + 1, 128 * 1024} {
			wire := strings.Replace(base, "X-Test: ", "X-Test: "+strings.Repeat("x", size-len(base)), 1)
			r, budget := boundedReader(wire, bufferSize)
			_, _ = r.Peek(7)
			_, err := readBoundedRequest(r, budget, limit)
			if size <= limit && err != nil {
				t.Fatalf("buffer=%d size=%d: %v", bufferSize, size, err)
			}
			if size > limit && !errors.Is(err, errHeaderTooLarge) {
				t.Fatalf("buffer=%d size=%d accepted/error=%v", bufferSize, size, err)
			}
		}
	}
}

func TestHeaderFieldLimitAndLongRequestLine(t *testing.T) {
	for _, wire := range []string{
		"GET / HTTP/1.1\r\nHost: example.test\r\n" + strings.Repeat("X-A: 1\r\n", 256) + "\r\n",
		"GET /" + strings.Repeat("a", defaultMaxHeaderBytes) + " HTTP/1.1\r\nHost: example.test\r\n\r\n",
	} {
		r, budget := boundedReader(wire, 4096)
		if _, err := readBoundedRequest(r, budget, defaultMaxHeaderBytes); !errors.Is(err, errHeaderTooLarge) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestHeaderBudgetDoesNotLimitBodiesOrLosePipeline(t *testing.T) {
	body := strings.Repeat("body-", 30000)
	first := fmt.Sprintf("POST /one HTTP/1.1\r\nHost: example.test\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	second := "GET /two HTTP/1.1\r\nHost: example.test\r\nUser-Agent: Linux\r\n\r\n"
	r, budget := boundedReader(first+second, 4096)
	for _, path := range []string{"/one", "/two"} {
		_, _ = r.Peek(7)
		req, err := readBoundedRequest(r, budget, 1024)
		if err != nil || req.URL.Path != path {
			t.Fatalf("path=%s req=%v error=%v", path, req, err)
		}
		got, err := io.ReadAll(req.Body)
		if err != nil || (path == "/one" && string(got) != body) {
			t.Fatal("body altered or truncated", err)
		}
		_ = req.Body.Close()
	}
}

func TestChunkedBodyAndTrailerRemainBounded(t *testing.T) {
	for _, huge := range []bool{false, true} {
		value := "complete"
		if huge {
			value = strings.Repeat("x", 8192)
		}
		body := strings.Repeat("b", 10000)
		wire := "POST / HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\nTrailer: X-Result\r\n\r\n" + fmt.Sprintf("%x\r\n%s\r\n0\r\nX-Result: %s\r\n\r\n", len(body), body, value)
		r, budget := boundedReader(wire, 4096)
		req, err := readBoundedRequest(r, budget, 1024)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(req.Body)
		if huge {
			if err == nil {
				t.Fatal("oversized trailer accepted")
			}
		} else if err != nil || string(got) != body || req.Trailer.Get("X-Result") != value {
			t.Fatal("chunked data/trailer not preserved", err)
		}
	}
}

func TestNonHTTPDoesNotAllocateHTTPWriter(t *testing.T) {
	s := testServer(t)
	var writers atomic.Int64
	s.handler.bufioWriterPool.New = func() any { writers.Add(1); return bufio.NewWriterSize(nil, 1024) }
	client, upstream, done, cancel := startRelay(t, s)
	payload := []byte("BINARY! payload")
	_, _ = client.Write(payload)
	readExact(t, upstream, payload)
	cancel()
	waitRelay(t, done)
	if writers.Load() != 0 {
		t.Fatal("non-HTTP connection borrowed HTTP writer")
	}
}

func TestOversizedHTTPClosesRelay(t *testing.T) {
	s := testServer(t)
	s.config.MaxHeaderBytes = 1024
	client, upstream, done, _ := startRelay(t, s)
	_, _ = client.Write([]byte("GET / HTTP/1.1\r\nHost: example.test\r\nX: " + strings.Repeat("x", 2048) + "\r\n\r\n"))
	waitRelay(t, done)
	assertClosed(t, upstream)
}

func TestDisabledCacheStillRewritesAndStreamsBody(t *testing.T) {
	s := testServer(t)
	s.handler.cache = nil
	s.config.MaxHeaderBytes = 1024
	client, upstream, done, cancel := startRelay(t, s)
	body := bytes.Repeat([]byte("0123456789"), 20000)
	wrote := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintf(client, "POST / HTTP/1.1\r\nHost: example.test\r\nUser-Agent: Linux\r\nContent-Length: %d\r\n\r\n", len(body))
		if err == nil {
			_, err = client.Write(body)
		}
		wrote <- err
	}()
	req, err := http.ReadRequest(bufio.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || !bytes.Equal(got, body) || req.UserAgent() != "Android" {
		t.Fatal("streamed body or UA mismatch", err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	cancel()
	waitRelay(t, done)
}

type failingWriteConn struct{ net.Conn }

func (failingWriteConn) Write([]byte) (int, error) {
	return 0, errors.New("test upstream write failure")
}

func TestFailedForwardDoesNotDrainUnfinishedUpload(t *testing.T) {
	s := testServer(t)
	client, source := net.Pipe()
	defer client.Close()
	defer source.Close()
	_ = client.SetDeadline(time.Now().Add(testHangGuard))
	done := make(chan error, 1)
	go func() { done <- s.handler.ModifyAndForward(failingWriteConn{}, source, "test:80", "127.0.0.1", 80) }()
	go func() {
		// The declared body is deliberately never completed.
		_, _ = fmt.Fprint(client, "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 1000000\r\n\r\n"+strings.Repeat("b", 8192))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("upstream failure was lost")
		}
	case <-time.After(time.Second):
		t.Fatal("failed write tried to drain an unfinished body")
	}
}

// A small actual TCP loopback transfer compares buffers on the target CPU.
// It does not claim to measure WAN/Wi-Fi throughput or total router capacity.
func BenchmarkRelayLoopback(b *testing.B) {
	for _, pooled := range []bool{false, true} {
		b.Run(fmt.Sprintf("pooled8K_%t", pooled), func(b *testing.B) {
			b.SetBytes(4 * 1024 * 1024)
			for i := 0; i < b.N; i++ {
				listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					b.Fatal(err)
				}
				client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
				if err != nil {
					listener.Close()
					b.Fatal(err)
				}
				server, err := listener.AcceptTCP()
				listener.Close()
				if err != nil {
					client.Close()
					b.Fatal(err)
				}
				_ = client.SetDeadline(time.Now().Add(10 * time.Second))
				_ = server.SetDeadline(time.Now().Add(10 * time.Second))
				sent := make(chan error, 1)
				go func() {
					chunk := make([]byte, 32768)
					for j := 0; j < 128; j++ {
						if _, err := server.Write(chunk); err != nil {
							sent <- err
							return
						}
					}
					_ = server.CloseWrite()
					sent <- nil
				}()
				var last atomic.Int64
				src := &activityConn{Conn: client, touch: func() { last.Store(time.Now().UnixNano()) }}
				// Hide Discard.ReadFrom so both variants exercise their copy buffer.
				dst := struct{ io.Writer }{io.Discard}
				var n int64
				if pooled {
					n, err = copyRelay(dst, src)
				} else {
					n, err = io.Copy(dst, src)
				}
				client.Close()
				server.Close()
				if sendErr := <-sent; sendErr != nil {
					b.Fatal(sendErr)
				}
				if err != nil || n != 4*1024*1024 {
					b.Fatalf("n=%d error=%v", n, err)
				}
			}
		})
	}
}
