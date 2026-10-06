package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Authorize authenticates every HTTP request and returns an identity key. A
// session ID routes traffic and never replaces the caller's credential.
type Authorize func(*http.Request) (string, int)
type Server struct {
	mu        sync.Mutex
	sessions  map[string]*serverConn
	authorize Authorize
	done      chan struct{}
	closed    bool
	bytes     int
	changed   chan struct{}
}

func NewServer(authorize Authorize) *Server {
	s := &Server{sessions: map[string]*serverConn{}, authorize: authorize, done: make(chan struct{}), changed: make(chan struct{})}
	go s.reap()
	return s
}
func (s *Server) signal() { close(s.changed); s.changed = make(chan struct{}) }
func (s *Server) Upgrade(w http.ResponseWriter, r *http.Request, protocol string) (Conn, error) {
	if r.Method != "POST" || r.URL.Query().Get("transport") != "https" {
		http.Error(w, "HTTPS POST handshake required", 405)
		return nil, fmt.Errorf("HTTPS POST handshake required")
	}
	owner, status := s.authorize(r)
	if status != 0 {
		http.Error(w, "unauthorized", status)
		return nil, fmt.Errorf("unauthorized")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw[:])
	s.mu.Lock()
	count := 0
	for _, c := range s.sessions {
		if c.owner == owner {
			count++
		}
	}
	if s.closed || len(s.sessions) >= 512 || count >= 128 {
		s.mu.Unlock()
		http.Error(w, "transport capacity exhausted", 429)
		return nil, fmt.Errorf("capacity exhausted")
	}
	c := &serverConn{server: s, id: id, owner: owner, protocol: protocol, limit: MaxFrame, in: make(chan frame, 1), done: make(chan struct{}), changed: make(chan struct{}), sendGate: make(chan struct{}, 1), receiveGate: make(chan struct{}, 1), active: time.Now(), digests: make(map[uint64][32]byte)}
	s.sessions[id] = c
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	if err := json.NewEncoder(w).Encode(map[string]any{"id": id, "protocol": protocol, "maxFrameBytes": MaxFrame, "pollSeconds": int(PollTime / time.Second)}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, Prefix), "/")
	if len(parts) != 2 || len(parts[0]) != 32 {
		http.NotFound(w, r)
		return
	}
	owner, status := s.authorize(r)
	if status != 0 {
		http.Error(w, "unauthorized", status)
		return
	}
	s.mu.Lock()
	c := s.sessions[parts[0]]
	s.mu.Unlock()
	if c == nil {
		http.Error(w, "session expired", 410)
		return
	}
	if c.owner != owner {
		http.Error(w, "wrong session owner", 403)
		return
	}
	c.mu.Lock()
	c.active = time.Now()
	c.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case r.Method == "GET" && parts[1] == "receive":
		c.receiveHTTP(w, r)
	case r.Method == "POST" && parts[1] == "send":
		c.sendHTTP(w, r)
	case r.Method == "POST" && parts[1] == "close":
		c.forceClose()
		w.WriteHeader(204)
	default:
		http.Error(w, "method not allowed", 405)
	}
}
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	var list []*serverConn
	for _, c := range s.sessions {
		list = append(list, c)
	}
	s.mu.Unlock()
	for _, c := range list {
		c.forceClose()
	}
}
func (s *Server) reap() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			var list []*serverConn
			for _, c := range s.sessions {
				list = append(list, c)
			}
			s.mu.Unlock()
			for _, c := range list {
				c.mu.Lock()
				expired := time.Since(c.active) > LeaseTime
				c.mu.Unlock()
				if expired {
					c.forceClose()
				}
			}
		}
	}
}

type serverConn struct {
	server                       *Server
	id, owner, protocol          string
	mu                           sync.Mutex
	in                           chan frame
	done, changed                chan struct{}
	sendGate, receiveGate        chan struct{}
	out                          []frame
	bytes                        int
	limit                        int64
	readDeadline, writeDeadline  time.Time
	next, offered, ack, accepted uint64
	digests                      map[uint64][32]byte
	closed                       bool
	active                       time.Time
}

func (c *serverConn) signal() { close(c.changed); c.changed = make(chan struct{}) }
func wait(ctx context.Context, closed, changed <-chan struct{}, deadline time.Time) error {
	var timeout <-chan time.Time
	var timer *time.Timer
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return io.EOF
	case <-changed:
		return nil
	case <-timeout:
		return context.DeadlineExceeded
	}
}
func (c *serverConn) WriteMessage(kind int, data []byte) error {
	if (kind != 1 && kind != 2 && kind != 8) || len(data) > MaxFrame {
		return fmt.Errorf("invalid frame")
	}
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return io.EOF
		}
		deadline := c.writeDeadline
		if len(c.out) < 64 && (c.bytes+len(data) <= 1024*1024 || len(c.out) == 0) {
			c.server.mu.Lock()
			if c.server.bytes+len(data) <= 64*1024*1024 {
				c.server.bytes += len(data)
				c.server.mu.Unlock()
				c.next++
				c.out = append(c.out, frame{kind, c.next, bytes.Clone(data)})
				c.bytes += len(data)
				c.signal()
				c.mu.Unlock()
				return nil
			}
			changed := c.server.changed
			c.server.mu.Unlock()
			c.mu.Unlock()
			if err := wait(context.Background(), c.done, changed, deadline); err != nil {
				return err
			}
			continue
		}
		changed := c.changed
		c.mu.Unlock()
		if err := wait(context.Background(), c.done, changed, deadline); err != nil {
			return err
		}
	}
}
func (c *serverConn) receiveHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case c.receiveGate <- struct{}{}:
		defer func() { <-c.receiveGate }()
	default:
		http.Error(w, "receive already active", 429)
		return
	}
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, "invalid cursor", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), PollTime)
	defer cancel()
	for {
		c.mu.Lock()
		if c.closed && len(c.out) == 0 {
			c.mu.Unlock()
			http.Error(w, "session expired", 410)
			return
		}
		if after < c.ack || after > c.offered {
			c.mu.Unlock()
			http.Error(w, "invalid cursor", 409)
			return
		}
		freed := 0
		for len(c.out) > 0 && c.out[0].Seq <= after {
			freed += len(c.out[0].Data)
			c.out[0] = frame{}
			c.out = c.out[1:]
		}
		c.bytes -= freed
		c.ack = after
		if freed > 0 {
			c.server.mu.Lock()
			c.server.bytes -= freed
			c.server.signal()
			c.server.mu.Unlock()
			c.signal()
		}
		if len(c.out) > 0 {
			var batch []frame
			size := 0
			for _, f := range c.out {
				if len(batch) >= 64 || (len(batch) > 0 && size+len(f.Data) > 1024*1024) {
					break
				}
				batch = append(batch, f)
				size += len(f.Data)
			}
			c.offered = batch[len(batch)-1].Seq
			data := encode(batch)
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data)
			return
		}
		changed := c.changed
		c.mu.Unlock()
		if err := wait(ctx, c.done, changed, time.Time{}); err != nil {
			if r.Context().Err() == nil {
				w.WriteHeader(204)
			}
			return
		}
	}
}
func (c *serverConn) sendHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case c.sendGate <- struct{}{}:
		defer func() { <-c.sendGate }()
	case <-r.Context().Done():
		return
	default:
		http.Error(w, "send already active", 429)
		return
	case <-c.done:
		http.Error(w, "session expired", 410)
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		http.Error(w, "session expired", 410)
		return
	}
	limit := c.limit
	c.mu.Unlock()
	body, err := io.ReadAll(io.LimitReader(r.Body, min(MaxBatch, limit*64+64*13)+1))
	if err != nil || int64(len(body)) > min(int64(MaxBatch), limit*64+64*13) {
		http.Error(w, "invalid batch", 413)
		return
	}
	frames, err := decode(body, limit)
	if err != nil || len(frames) == 0 {
		http.Error(w, "invalid frames", 400)
		return
	}
	// Validate the whole batch before exposing any new frame to the application.
	c.mu.Lock()
	next := c.accepted + 1
	for i, f := range frames {
		if (i > 0 && f.Seq != frames[i-1].Seq+1) || f.Seq > next {
			c.mu.Unlock()
			http.Error(w, "out of order", 409)
			return
		}
		digest := sha256.Sum256(encode([]frame{f}))
		if f.Seq < next {
			if known, ok := c.digests[f.Seq]; !ok || known != digest {
				c.mu.Unlock()
				http.Error(w, "conflicting retransmission", 409)
				return
			}
		} else {
			next++
		}
	}
	c.mu.Unlock()
	for _, f := range frames {
		c.mu.Lock()
		accepted := c.accepted
		c.mu.Unlock()
		if f.Seq <= accepted {
			continue
		}
		if f.Seq != accepted+1 {
			http.Error(w, "out of order", 409)
			return
		}
		select {
		case c.in <- f:
			c.mu.Lock()
			c.accepted = f.Seq
			c.digests[f.Seq] = sha256.Sum256(encode([]frame{f}))
			if f.Seq > 64 {
				delete(c.digests, f.Seq-64)
			}
			c.mu.Unlock()
		case <-r.Context().Done():
			return
		case <-c.done:
			http.Error(w, "session expired", 410)
			return
		}
	}
	w.WriteHeader(204)
}
func (c *serverConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	deadline := c.readDeadline
	c.mu.Unlock()
	var timeout <-chan time.Time
	var timer *time.Timer
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case f := <-c.in:
		if f.Kind == 8 {
			return 0, nil, io.EOF
		}
		return f.Kind, f.Data, nil
	case <-c.done:
		select {
		case f := <-c.in:
			if f.Kind != 8 {
				return f.Kind, f.Data, nil
			}
		default:
		}
		return 0, nil, io.EOF
	case <-timeout:
		return 0, nil, context.DeadlineExceeded
	}
}
func (c *serverConn) ReadJSON(v any) error {
	_, p, e := c.ReadMessage()
	if e != nil {
		return e
	}
	return json.Unmarshal(p, v)
}
func (c *serverConn) WriteJSON(v any) error {
	p, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return c.WriteMessage(1, p)
}
func (c *serverConn) NextReader() (int, io.Reader, error) {
	k, p, e := c.ReadMessage()
	return k, bytes.NewReader(p), e
}
func (c *serverConn) WriteControl(k int, p []byte, d time.Time) error {
	if k == 8 {
		c.SetWriteDeadline(d)
		return c.WriteMessage(k, p)
	}
	return nil
}
func (c *serverConn) SetReadLimit(n int64) { c.mu.Lock(); c.limit = n; c.mu.Unlock() }
func (c *serverConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.readDeadline = d
	c.mu.Unlock()
	return nil
}
func (c *serverConn) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	c.writeDeadline = d
	c.mu.Unlock()
	return nil
}
func (c *serverConn) Subprotocol() string  { return c.protocol }
func (c *serverConn) LocalAddr() net.Addr  { return httpAddr("https-server") }
func (c *serverConn) RemoteAddr() net.Addr { return httpAddr(c.owner) }

// Close ends application access but retains already-written outbound frames
// until the client reads the close frame. This preserves SSH EOF and file tails.
func (c *serverConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	if len(c.out) == 0 || c.out[len(c.out)-1].Kind != 8 {
		c.next++
		c.out = append(c.out, frame{8, c.next, nil})
	}
	c.signal()
	return nil
}
func (c *serverConn) forceClose() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	freed := c.bytes
	c.out = nil
	c.bytes = 0
	c.mu.Unlock()
	c.server.mu.Lock()
	delete(c.server.sessions, c.id)
	c.server.bytes -= freed
	c.server.signal()
	c.server.mu.Unlock()
}
