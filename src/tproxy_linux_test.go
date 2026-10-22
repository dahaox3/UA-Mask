//go:build linux

package main

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func socketFlags(t *testing.T, conn *net.TCPConn) int {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		flags, socketErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if socketErr != nil {
		t.Fatal(socketErr)
	}
	return flags
}

func TestOriginalDstPreservesNonblockingSocketAndReadDeadlines(t *testing.T) {
	_, conn := tcpPair(t)
	before := socketFlags(t, conn)
	if before&unix.O_NONBLOCK == 0 {
		t.Fatal("test socket did not begin in nonblocking mode")
	}
	// The host may lack conntrack/SO_ORIGINAL_DST. Success and error paths
	// must both preserve the socket flags and Go's network poller.
	_, _ = getOriginalDst(conn)
	after := socketFlags(t, conn)
	if before != after {
		t.Fatalf("socket flags changed: before=%#x after=%#x", before, after)
	}
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		var data [1]byte
		_, err := conn.Read(data[:])
		finished <- err
	}()
	select {
	case err := <-finished:
		if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
			t.Fatalf("read did not respect its deadline: %v", err)
		}
	case <-time.After(testHangGuard):
		t.Fatal("reading original destination disabled the socket read deadline")
	}
}
