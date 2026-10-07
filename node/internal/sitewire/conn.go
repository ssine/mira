// Package sitewire carries an opaque, half-closeable TCP stream over a dedicated
// Mira transport. It never interprets HTTP bodies or replays application bytes.
package sitewire

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/transport"
)

const Protocol = "mira-site-v1"
const FrameBytes = 64 * 1024
const dataFrame byte = 1
const finFrame byte = 2

// Ready acknowledges one local dial. This frame precedes all stream records.
type Ready struct {
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

type record struct {
	data []byte
	err  error
}

type Conn struct {
	raw                         transport.Conn
	in                          chan record
	done                        chan struct{}
	once                        sync.Once
	readMu                      sync.Mutex
	pending                     []byte
	eof                         bool
	writeMu                     sync.Mutex
	writeClosed                 bool
	deadlineMu                  sync.Mutex
	readDeadline, writeDeadline time.Time
	readChanged                 chan struct{}
	writeChanged                chan struct{}
}

func New(raw transport.Conn) *Conn {
	raw.SetReadLimit(FrameBytes)
	c := &Conn{raw: raw, in: make(chan record, 1), done: make(chan struct{}), readChanged: make(chan struct{}), writeChanged: make(chan struct{})}
	go c.receive()
	return c
}

func (c *Conn) receive() {
	finished := false
	for {
		kind, payload, err := c.raw.ReadMessage()
		if err != nil {
			c.deliver(record{err: io.ErrUnexpectedEOF})
			c.Close()
			return
		}
		if kind != websocket.BinaryMessage || len(payload) < 1 || len(payload) > FrameBytes {
			c.deliver(record{err: errors.New("invalid site stream record")})
			c.Close()
			return
		}
		switch payload[0] {
		case dataFrame:
			if finished || len(payload) == 1 {
				c.deliver(record{err: errors.New("invalid site data")})
				c.Close()
				return
			}
			if !c.deliver(record{data: payload[1:]}) {
				return
			}
		case finFrame:
			if finished || len(payload) != 1 {
				c.deliver(record{err: errors.New("invalid site FIN")})
				c.Close()
				return
			}
			finished = true
			if !c.deliver(record{err: io.EOF}) {
				return
			}
		default:
			c.deliver(record{err: errors.New("unknown site record")})
			c.Close()
			return
		}
	}
}

func (c *Conn) deliver(r record) bool {
	select {
	case c.in <- r:
		return true
	case <-c.done:
		return false
	}
}

func (c *Conn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(c.pending) > 0 {
			n := copy(p, c.pending)
			c.pending = c.pending[n:]
			return n, nil
		}
		if c.eof {
			return 0, io.EOF
		}
		c.deadlineMu.Lock()
		deadline, changed := c.readDeadline, c.readChanged
		c.deadlineMu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		var r record
		select {
		case r = <-c.in:
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-c.done:
			// Deliver a FIN or bytes already queued before a transport failure.
			select {
			case r = <-c.in:
			default:
				if timer != nil {
					timer.Stop()
				}
				return 0, net.ErrClosed
			}
		}
		if timer != nil {
			timer.Stop()
		}
		if r.err != nil {
			if r.err == io.EOF {
				c.eof = true
			}
			return 0, r.err
		}
		c.pending = r.data
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed {
		return 0, net.ErrClosed
	}
	n := 0
	for len(p) > 0 {
		select {
		case <-c.done:
			return n, net.ErrClosed
		default:
		}
		size := min(len(p), FrameBytes-1)
		frame := make([]byte, size+1)
		frame[0] = dataFrame
		copy(frame[1:], p[:size])
		if err := c.writeFrame(frame); err != nil {
			return n, err
		}
		p = p[size:]
		n += size
	}
	return n, nil
}

func (c *Conn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed {
		return nil
	}
	c.writeClosed = true
	return c.writeFrame([]byte{finFrame})
}

// The transport writer is serialized by writeMu. Deadline updates wake this
// waiter, and closing the transport interrupts a blocked write in either mode.
// A timed-out write poisons the connection; application bytes are never retried.
func (c *Conn) writeFrame(frame []byte) error {
	c.deadlineMu.Lock()
	expired := !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline)
	c.deadlineMu.Unlock()
	if expired {
		return os.ErrDeadlineExceeded
	}
	result := make(chan error, 1)
	go func() { result <- c.raw.WriteMessage(websocket.BinaryMessage, frame) }()
	for {
		c.deadlineMu.Lock()
		deadline, changed := c.writeDeadline, c.writeChanged
		c.deadlineMu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(max(0, time.Until(deadline)))
			timeout = timer.C
		}
		select {
		case err := <-result:
			if timer != nil {
				timer.Stop()
			}
			return err
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-timeout:
			c.Close()
			return os.ErrDeadlineExceeded
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return net.ErrClosed
		}
	}
}
func (c *Conn) Close() error          { c.once.Do(func() { close(c.done); _ = c.raw.Close() }); return nil }
func (c *Conn) Done() <-chan struct{} { return c.done }
func (c *Conn) LocalAddr() net.Addr   { return c.raw.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr  { return c.raw.RemoteAddr() }
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = t
	close(c.readChanged)
	c.readChanged = make(chan struct{})
	c.deadlineMu.Unlock()
	return nil
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	c.writeDeadline = t
	close(c.writeChanged)
	c.writeChanged = make(chan struct{})
	c.deadlineMu.Unlock()
	return nil
}
func (c *Conn) SetDeadline(t time.Time) error { _ = c.SetReadDeadline(t); return c.SetWriteDeadline(t) }

// Bridge preserves TCP half-closes. Cancellation closes both sockets, including
// writers blocked by backpressure. There is no application inactivity timeout.
func Bridge(ctx context.Context, stream *Conn, local *net.TCPConn) {
	defer stream.Close()
	defer local.Close()
	results := make(chan error, 2)
	go func() {
		_, err := io.Copy(stream, local)
		if err == nil {
			err = stream.CloseWrite()
		}
		results <- err
	}()
	go func() {
		_, err := io.Copy(local, stream)
		if err == nil {
			err = local.CloseWrite()
		}
		results <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-stream.Done():
			return
		}
	}
}
