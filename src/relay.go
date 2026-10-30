package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const relayBufferSize = 8 * 1024

var relayBufferPool = sync.Pool{New: func() any { return new([relayBufferSize]byte) }}

func copyRelay(dst io.Writer, src io.Reader) (int64, error) {
	buffer := relayBufferPool.Get().(*[relayBufferSize]byte)
	defer relayBufferPool.Put(buffer)
	return io.CopyBuffer(dst, src, buffer[:])
}

type activityConn struct {
	net.Conn
	touch func()
}

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *activityConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	} else {
		_ = conn.Close()
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func (s *Server) relay(ctx context.Context, client, upstream net.Conn, target *net.TCPAddr) {
	started := time.Now()
	var lastActivity atomic.Int64
	touch := func() {
		now := time.Since(started).Nanoseconds()
		for previous := lastActivity.Load(); previous < now; previous = lastActivity.Load() {
			if lastActivity.CompareAndSwap(previous, now) {
				return
			}
		}
	}
	touch()
	clientIO := &activityConn{Conn: client, touch: touch}
	upstreamIO := &activityConn{Conn: upstream, touch: touch}
	done := make(chan error, 2)
	go func() {
		err := s.handler.ModifyAndForward(upstreamIO, clientIO, target.String(), target.IP.String(), target.Port)
		closeWrite(upstream)
		done <- err
	}()
	go func() {
		_, err := copyRelay(clientIO, upstreamIO)
		closeWrite(client)
		done <- err
	}()

	closeBoth := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	defer closeBoth()
	idleLimit := s.config.IdleTimeout
	timer := time.NewTimer(idleLimit)
	defer timer.Stop()
	timerC := timer.C
	contextDone := ctx.Done()
	completed := 0
	for completed < 2 {
		select {
		case err := <-done:
			completed++
			if completed == 1 {
				if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
					closeBoth()
					timerC = nil
				} else {
					// Preserve half-close semantics while the response is active.
					// Only a silent drain is cut short; there is no lifetime cap.
					idleLimit = s.config.HalfCloseTimeout
					touch()
					resetTimer(timer, idleLimit)
					timerC = timer.C
				}
			}
		case <-timerC:
			idle := time.Since(started) - time.Duration(lastActivity.Load())
			if idle >= idleLimit {
				closeBoth()
				timerC = nil
			} else {
				resetTimer(timer, idleLimit-idle)
			}
		case <-contextDone:
			closeBoth()
			contextDone = nil
			timerC = nil
		}
	}
}
