package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
)

type Server struct {
	config  *Config
	handler *HTTPHandler
}

func NewServer(config *Config, handler *HTTPHandler) *Server {
	return &Server{config: config, handler: handler}
}

func (s *Server) Run() error {
	return s.RunContext(context.Background())
}

func (s *Server) RunContext(ctx context.Context) error {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(0, 0, 0, 0), Port: s.config.Port})
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	return s.serve(ctx, listener)
}

// A queued connection owns its socket until either a worker claims it or its
// queue deadline closes it. The CAS prevents a timer from closing an active job.
type queuedConnection struct {
	conn  *net.TCPConn
	state atomic.Uint32
	timer *time.Timer
}

func newQueuedConnection(conn *net.TCPConn, timeout time.Duration) *queuedConnection {
	job := &queuedConnection{conn: conn}
	job.timer = time.AfterFunc(timeout, job.expire)
	return job
}

func (job *queuedConnection) expire() {
	if job.state.CompareAndSwap(0, 2) {
		_ = job.conn.Close()
	}
}

func (job *queuedConnection) claim() bool {
	if !job.state.CompareAndSwap(0, 1) {
		return false
	}
	job.timer.Stop()
	return true
}

func (job *queuedConnection) discard() {
	job.expire()
	job.timer.Stop()
}

func (s *Server) queueConnection(jobs chan *queuedConnection, conn *net.TCPConn) bool {
	job := newQueuedConnection(conn, s.config.QueueTimeout)
	s.handler.stats.QueuedConnections.Add(1)
	select {
	case jobs <- job:
		return true
	default:
		s.handler.stats.QueuedConnections.Add(-1)
		s.handler.stats.RejectedConnections.Add(1)
		// Keep Accept moving so a full worker pool cannot fill the kernel backlog.
		job.discard()
		logrus.Debug("Worker queue full; rejected connection")
		return false
	}
}

func (s *Server) serve(parent context.Context, listener *net.TCPListener) error {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	var jobs chan *queuedConnection
	watcherDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-watcherDone:
		}
	}()
	defer func() {
		cancel()
		_ = listener.Close()
		if jobs != nil {
			close(jobs)
		}
		workers.Wait()
		close(watcherDone)
	}()

	if s.config.PoolSize > 0 {
		logrus.Infof("Starting in Worker Pool Mode (size: %d)", s.config.PoolSize)
		jobs = make(chan *queuedConnection, s.config.PoolSize)
		for i := 0; i < s.config.PoolSize; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for job := range jobs {
					s.handler.stats.QueuedConnections.Add(-1)
					if ctx.Err() != nil {
						job.discard()
						continue
					}
					if job.claim() {
						s.handleConnection(ctx, job.conn)
					}
				}
			}()
		}
	} else {
		logrus.Info("Starting in Default Mode (one goroutine per connection)")
	}

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if temporary, ok := err.(net.Error); ok && temporary.Temporary() {
				logrus.Warnf("Accept error: %v; retrying", err)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Millisecond):
					continue
				}
			}
			return fmt.Errorf("accept failed: %w", err)
		}
		if ctx.Err() != nil {
			_ = conn.Close()
			return nil
		}
		if jobs == nil {
			workers.Add(1)
			go func() {
				defer workers.Done()
				s.handleConnection(ctx, conn)
			}()
			continue
		}
		s.queueConnection(jobs, conn)
	}
}

func (s *Server) handleConnection(ctx context.Context, clientConn *net.TCPConn) {
	s.handler.stats.AddActiveConnections(1)
	defer func() {
		s.handler.stats.AddActiveConnections(^uint64(0))
		_ = clientConn.Close()
	}()

	originalDst, err := getOriginalDst(clientConn)
	if err != nil {
		logrus.Debugf("[server] Failed to get original destination: %v", err)
		return
	}
	_ = clientConn.SetKeepAlive(true)
	_ = clientConn.SetKeepAlivePeriod(3 * time.Minute)
	dialer := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 3 * time.Minute}
	upstream, err := dialer.DialContext(ctx, "tcp", originalDst.String())
	if err != nil {
		logrus.Debugf("[server] Failed to connect to %s: %v", originalDst, err)
		return
	}
	defer upstream.Close()
	s.relay(ctx, clientConn, upstream, originalDst)
}
