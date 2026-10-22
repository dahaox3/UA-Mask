package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"testing"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const testHangGuard = 5 * time.Second

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.AcceptTCP()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	_ = client.SetDeadline(time.Now().Add(testHangGuard))
	_ = server.SetDeadline(time.Now().Add(testHangGuard))
	return client, server
}

func testServer(t *testing.T) *Server {
	t.Helper()
	config := &Config{
		UserAgent: "Android", EnableRegex: true, EnablePartialReplace: true,
		UARegexp:   regexp.MustCompile(`(?i)(iPhone|iPad|Macintosh|Linux)`),
		BufferSize: 1024, PoolSize: 2,
		FirstPacketTimeout: time.Second, IdleTimeout: 2 * time.Second,
		HalfCloseTimeout: 500 * time.Millisecond, QueueTimeout: time.Second,
	}
	cache, err := lru.New[string, string](32)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(config, NewHTTPHandler(config, NewStats(), cache, nil))
}

func startRelay(t *testing.T, server *Server) (*net.TCPConn, *net.TCPConn, <-chan struct{}, context.CancelFunc) {
	t.Helper()
	client, clientProxy := tcpPair(t)
	upstream, upstreamProxy := tcpPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.relay(ctx, clientProxy, upstreamProxy, upstream.RemoteAddr().(*net.TCPAddr))
	}()
	return client, upstream, done, cancel
}

func waitRelay(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testHangGuard):
		t.Fatal("relay did not release both directions")
	}
}

func readExact(t *testing.T, conn net.Conn, expected []byte) {
	t.Helper()
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(conn, actual); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("payload changed: %q != %q", actual, expected)
	}
}

func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	var buf [1]byte
	_, err := conn.Read(buf[:])
	if err == nil {
		t.Fatal("expected closed connection")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("connection remained open until the test hang guard")
	}
}

func TestFirstPacketTimeoutReleasesSilentAndPartialClients(t *testing.T) {
	for _, initial := range []string{"", "GET"} {
		t.Run("prefix="+initial, func(t *testing.T) {
			server := testServer(t)
			server.config.FirstPacketTimeout = 20 * time.Millisecond
			server.config.HalfCloseTimeout = 20 * time.Second
			client, upstream, done, _ := startRelay(t, server)
			if initial != "" {
				_, _ = client.Write([]byte(initial))
			}
			waitRelay(t, done)
			assertClosed(t, client)
			assertClosed(t, upstream)
		})
	}
}

func TestIncompleteHTTPHeadersAreBounded(t *testing.T) {
	server := testServer(t)
	server.config.FirstPacketTimeout = 20 * time.Millisecond
	client, upstream, done, _ := startRelay(t, server)
	_, _ = client.Write([]byte("GET / HTTP/1.1\r\nHost: example.test\r\n"))
	waitRelay(t, done)
	assertClosed(t, upstream)
}

func TestMalformedHTTPClosesBothDirectionsImmediately(t *testing.T) {
	server := testServer(t)
	server.config.HalfCloseTimeout = 20 * time.Second
	client, upstream, done, _ := startRelay(t, server)
	_, _ = client.Write([]byte("GET / HTTP/1.1\r\nMalformed-Header\r\n\r\n"))
	waitRelay(t, done)
	assertClosed(t, upstream)
}

func TestIdleNonHTTPConnectionIsReclaimed(t *testing.T) {
	server := testServer(t)
	server.config.IdleTimeout = 40 * time.Millisecond
	client, upstream, done, _ := startRelay(t, server)
	payload := []byte{0x16, 3, 1, 0, 3, 0, 255, 42}
	_, _ = client.Write(payload)
	readExact(t, upstream, payload)
	waitRelay(t, done)
	assertClosed(t, client)
}

func TestOneWayTrafficKeepsConnectionAliveBeyondIdleLimit(t *testing.T) {
	server := testServer(t)
	server.config.IdleTimeout = 250 * time.Millisecond
	client, upstream, done, cancel := startRelay(t, server)
	payload := []byte("BINARY! initial")
	_, _ = client.Write(payload)
	readExact(t, upstream, payload)
	// More than one complete idle period passes, but only server -> client
	// traffic continues. A per-direction deadline would incorrectly drop it.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 8; i++ {
		<-ticker.C
		if _, err := upstream.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		readExact(t, client, []byte{byte(i)})
	}
	cancel()
	waitRelay(t, done)
}

func TestHalfCloseDrainsResponseAndReclaimsSilentPeer(t *testing.T) {
	server := testServer(t)
	server.config.HalfCloseTimeout = 250 * time.Millisecond
	client, upstream, done, _ := startRelay(t, server)
	payload := []byte("BINARY! request")
	_, _ = client.Write(payload)
	_ = client.CloseWrite()
	actual, err := io.ReadAll(upstream)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("half-close request: %q %v", actual, err)
	}
	_, _ = upstream.Write([]byte("response"))
	readExact(t, client, []byte("response"))
	waitRelay(t, done)
	assertClosed(t, upstream)
}

func TestActiveHalfClosedResponseHasNoFixedLifetimeLimit(t *testing.T) {
	server := testServer(t)
	server.config.HalfCloseTimeout = 250 * time.Millisecond
	client, upstream, done, _ := startRelay(t, server)
	_, _ = client.Write([]byte("BINARY! request"))
	_ = client.CloseWrite()
	if _, err := io.ReadAll(upstream); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 8; i++ {
		<-ticker.C
		_, err := upstream.Write([]byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		readExact(t, client, []byte{byte(i)})
	}
	_ = upstream.CloseWrite()
	waitRelay(t, done)
}

func TestCancellationUnblocksBothCopyLoops(t *testing.T) {
	server := testServer(t)
	client, upstream, done, cancel := startRelay(t, server)
	cancel()
	waitRelay(t, done)
	assertClosed(t, client)
	assertClosed(t, upstream)
}

func TestHTTPRewritePreservesConfiguredWindowsExclusion(t *testing.T) {
	server := testServer(t)
	client, upstream, done, cancel := startRelay(t, server)
	defer cancel()
	upstreamReader := bufio.NewReader(upstream)
	clientReader := bufio.NewReader(client)
	for _, ua := range []struct{ in, out string }{{"Probe Linux", "Probe Android"}, {"Probe Windows", "Probe Windows"}} {
		request := "GET / HTTP/1.1\r\nHost: example.test\r\nUser-Agent: " + ua.in + "\r\n\r\n"
		_, _ = client.Write([]byte(request))
		parsed, err := http.ReadRequest(upstreamReader)
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.UserAgent(); got != ua.out {
			t.Fatalf("UA = %q, want %q", got, ua.out)
		}
		parsed.Body.Close()
		_, _ = upstream.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
		response, err := http.ReadResponse(clientReader, nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	cancel()
	waitRelay(t, done)
}

func TestQueuedConnectionExpiresAndCannotBeClaimed(t *testing.T) {
	client, queued := tcpPair(t)
	job := newQueuedConnection(queued, 20*time.Millisecond)
	defer job.discard()
	assertClosed(t, client)
	if job.claim() {
		t.Fatal("expired connection was claimed by a worker")
	}
}

func TestClaimedConnectionSurvivesQueueExpiry(t *testing.T) {
	client, queued := tcpPair(t)
	job := newQueuedConnection(queued, time.Hour)
	if !job.claim() {
		t.Fatal("worker failed to claim connection")
	}
	job.expire()
	_, _ = client.Write([]byte("ok"))
	readExact(t, queued, []byte("ok"))
}

func TestFullWorkerQueueRejectsWithoutBlockingAccept(t *testing.T) {
	server := testServer(t)
	jobs := make(chan *queuedConnection, 1)
	firstClient, first := tcpPair(t)
	if !server.queueConnection(jobs, first) {
		t.Fatal("first connection was not queued")
	}
	secondClient, second := tcpPair(t)
	returned := make(chan bool, 1)
	go func() { returned <- server.queueConnection(jobs, second) }()
	select {
	case accepted := <-returned:
		if accepted {
			t.Fatal("full queue accepted another connection")
		}
	case <-time.After(testHangGuard):
		t.Fatal("full queue blocked the accept loop")
	}
	assertClosed(t, secondClient)
	(<-jobs).discard()
	assertClosed(t, firstClient)
}

func TestServerCancellationStopsListenerAndWorkers(t *testing.T) {
	server := testServer(t)
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx, listener) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testHangGuard):
		t.Fatal("server did not stop")
	}
}
