// Package rtspproxy provides a minimal TCP pass-through proxy. It listens on a
// port and forwards every accepted connection bidirectionally to a fixed
// upstream target ("host:port"), which is how ONVIF clients reach the real
// camera RTSP stream through the advertised proxy port.
package rtspproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	dialTimeout    = 5 * time.Second
	idleTimeout    = 2 * time.Minute
	maxConnections = 128
)

// Run listens on all interfaces at listenPort and forwards each connection to
// target via two io.Copy goroutines; when either direction closes, both sides
// are closed. Cancelling ctx closes the listener and every in-flight
// connection. It returns the net.Listen error or, on shutdown, ctx.Err().
func Run(ctx context.Context, listenPort int, target string) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp4", ":"+strconv.Itoa(listenPort))
	if err != nil {
		return err
	}
	return Serve(ctx, ln, target)
}

// Serve forwards connections accepted from an already-bound listener. It lets
// callers reserve and validate a port before switching configuration. Serve
// owns ln and closes it when ctx is cancelled.
func Serve(ctx context.Context, ln net.Listener, target string) error {
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()

	var (
		mu     sync.Mutex
		conns  = make(map[net.Conn]struct{})
		wg     sync.WaitGroup
		closed bool
	)

	closeAll := func() {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		closed = true
		_ = ln.Close()
		for c := range conns {
			_ = c.Close()
		}
	}
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			c.Close()
			return false
		}
		conns[c] = struct{}{}
		return true
	}
	untrack := func(c net.Conn) {
		mu.Lock()
		delete(conns, c)
		mu.Unlock()
	}

	// Close the listener and all tracked connections when ctx is cancelled.
	stop := make(chan struct{})
	go func() {
		select {
		case <-serveCtx.Done():
		case <-stop:
			return
		}
		closeAll()
	}()

	slots := make(chan struct{}, maxConnections)
	var serveErr error
	for {
		client, err := ln.Accept()
		if err != nil {
			if serveCtx.Err() != nil {
				serveErr = serveCtx.Err()
				break
			}
			// Back off briefly on temporary accept errors, then retry.
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				select {
				case <-time.After(10 * time.Millisecond):
				case <-serveCtx.Done():
					serveErr = serveCtx.Err()
					break
				}
				if serveErr != nil {
					break
				}
				continue
			}
			serveErr = err
			break
		}

		select {
		case slots <- struct{}{}:
		default:
			_ = client.Close()
			continue
		}
		wg.Add(1)
		go func(client net.Conn) {
			defer wg.Done()
			defer func() { <-slots }()
			rawClient := client
			if !track(rawClient) {
				return
			}
			defer func() {
				untrack(rawClient)
				_ = rawClient.Close()
			}()

			dialer := net.Dialer{Timeout: dialTimeout}
			upstream, err := dialer.DialContext(serveCtx, "tcp", target)
			if err != nil {
				return
			}
			rawUpstream := upstream
			if !track(rawUpstream) {
				return
			}
			defer func() {
				untrack(rawUpstream)
				_ = rawUpstream.Close()
			}()

			clientPipe, upstreamPipe := withIdleTimeout(rawClient, rawUpstream, idleTimeout)

			// Bidirectional copy; closing on either side unblocks the other.
			var once sync.Once
			closeBoth := func() {
				once.Do(func() {
					_ = rawClient.Close()
					_ = rawUpstream.Close()
				})
			}

			var pipeWg sync.WaitGroup
			pipeWg.Add(2)
			go func() {
				defer pipeWg.Done()
				io.Copy(upstreamPipe, clientPipe)
				closeBoth()
			}()
			go func() {
				defer pipeWg.Done()
				io.Copy(clientPipe, upstreamPipe)
				closeBoth()
			}()
			pipeWg.Wait()
		}(client)
	}

	close(stop)
	cancelServe()
	closeAll()
	wg.Wait()
	if serveErr != nil {
		return serveErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("rtsp proxy stopped")
}

// activityDeadline shares one idle deadline across both halves of a proxied
// connection. Activity in either direction refreshes both sockets, which is
// important for one-way RTP-over-RTSP streams.
type activityDeadline struct {
	conns   []net.Conn
	timeout time.Duration
}

func (d *activityDeadline) touch() {
	deadline := time.Now().Add(d.timeout)
	for _, c := range d.conns {
		_ = c.SetDeadline(deadline)
	}
}

type activityConn struct {
	net.Conn
	deadline *activityDeadline
}

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.deadline.touch()
	}
	return n, err
}

func (c *activityConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.deadline.touch()
	}
	return n, err
}

func withIdleTimeout(a, b net.Conn, timeout time.Duration) (net.Conn, net.Conn) {
	deadline := &activityDeadline{conns: []net.Conn{a, b}, timeout: timeout}
	deadline.touch()
	return &activityConn{Conn: a, deadline: deadline},
		&activityConn{Conn: b, deadline: deadline}
}
