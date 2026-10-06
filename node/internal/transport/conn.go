// Package transport carries bounded Mira frames over WebSocket or ordinary HTTP.
// It does not replay operations or persist conversation history.
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const MaxFrame = 16 * 1024 * 1024
const MaxBatch = MaxFrame + 64*13
const PollTime = 20 * time.Second
const LeaseTime = 60 * time.Second
const Prefix = "/v1/transports/"

// Conn deliberately keeps frame semantics, including binary frames and deadlines.
type Conn interface {
	ReadMessage() (int, []byte, error)
	WriteMessage(int, []byte) error
	ReadJSON(any) error
	WriteJSON(any) error
	NextReader() (int, io.Reader, error)
	WriteControl(int, []byte, time.Time) error
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	Subprotocol() string
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	Close() error
}

type frame struct {
	Kind int
	Seq  uint64
	Data []byte
}

func encode(frames []frame) []byte {
	size := 0
	for _, f := range frames {
		size += 13 + len(f.Data)
	}
	out := make([]byte, 0, size)
	for _, f := range frames {
		header := make([]byte, 13)
		header[0] = byte(f.Kind)
		binary.BigEndian.PutUint64(header[1:9], f.Seq)
		binary.BigEndian.PutUint32(header[9:], uint32(len(f.Data)))
		out = append(out, header...)
		out = append(out, f.Data...)
	}
	return out
}
func decode(data []byte, limit int64) ([]frame, error) {
	var result []frame
	for len(data) > 0 {
		if len(data) < 13 || len(result) >= 64 {
			return nil, fmt.Errorf("invalid frame batch")
		}
		kind := int(data[0])
		seq := binary.BigEndian.Uint64(data[1:9])
		n := int(binary.BigEndian.Uint32(data[9:13]))
		data = data[13:]
		if (kind != 1 && kind != 2 && kind != 8) || seq == 0 || n > int(limit) || n > len(data) {
			return nil, fmt.Errorf("invalid frame")
		}
		result = append(result, frame{kind, seq, data[:n]})
		data = data[n:]
	}
	return result, nil
}

// Dial never retries a handshake or native operation. Only accepted HTTP frames
// may be retransmitted with the identical sequence inside the same live session.
func Dial(ctx context.Context, dialer *websocket.Dialer, endpoint string, header http.Header, mode string) (Conn, *http.Response, error) {
	if mode == "" {
		mode = os.Getenv("MIRA_NODE_TRANSPORT")
	}
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "websocket" && mode != "https" {
		return nil, nil, fmt.Errorf("transport must be auto, websocket or https")
	}
	if mode != "https" {
		probe := *dialer
		if probe.Proxy == nil {
			probe.Proxy = http.ProxyFromEnvironment
		}
		if mode == "auto" && (probe.HandshakeTimeout == 0 || probe.HandshakeTimeout > 3*time.Second) {
			probe.HandshakeTimeout = 3 * time.Second
		}
		conn, response, err := probe.DialContext(ctx, endpoint, header)
		if err == nil {
			return conn, response, nil
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if mode == "websocket" || ctx.Err() != nil {
			return nil, response, err
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return nil, nil, fmt.Errorf("invalid transport URL")
	}
	query := u.Query()
	query.Set("transport", "https")
	u.RawQuery = query.Encode()
	h := header.Clone()
	if h == nil {
		h = make(http.Header)
	}
	for _, p := range dialer.Subprotocols {
		if strings.HasPrefix(p, "auth.") {
			raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(p, "auth."))
			if e != nil {
				return nil, nil, e
			}
			h.Set("Authorization", "Bearer "+string(raw))
		} else {
			h.Set("X-Mira-Protocol", p)
		}
	}
	network := http.DefaultTransport.(*http.Transport).Clone()
	if dialer.Proxy != nil {
		network.Proxy = dialer.Proxy
	}
	if dialer.TLSClientConfig != nil {
		network.TLSClientConfig = dialer.TLSClientConfig.Clone()
		network.TLSClientConfig.NextProtos = nil
	}
	network.TLSClientConfig = ensureTLS(network.TLSClientConfig)
	client := &http.Client{Transport: network, Timeout: PollTime + 10*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, "POST", u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header = h
	response, err := client.Do(request)
	if err != nil {
		return nil, response, err
	}
	defer response.Body.Close()
	if response.StatusCode != 201 {
		return nil, response, fmt.Errorf("HTTPS transport rejected (HTTP %d)", response.StatusCode)
	}
	var handshake struct {
		ID       string `json:"id"`
		Protocol string `json:"protocol"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&handshake); err != nil {
		return nil, response, err
	}
	id, idErr := hex.DecodeString(handshake.ID)
	if idErr != nil || len(id) != 16 || handshake.Protocol != h.Get("X-Mira-Protocol") {
		return nil, response, fmt.Errorf("invalid HTTPS handshake")
	}
	u.Path = Prefix + handshake.ID
	u.RawQuery = ""
	u.Fragment = ""
	cctx, cancel := context.WithCancel(ctx)
	return &clientConn{http: client, url: u.String(), headers: h, ctx: cctx, cancel: cancel, limit: MaxFrame, protocol: handshake.Protocol}, response, nil
}
func ensureTLS(config *tls.Config) *tls.Config {
	if config == nil {
		config = &tls.Config{}
	}
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	return config
}

type clientConn struct {
	http                        *http.Client
	url                         string
	headers                     http.Header
	ctx                         context.Context
	cancel                      context.CancelFunc
	protocol                    string
	readMu, writeMu, mu         sync.Mutex
	limit                       int64
	readDeadline, writeDeadline time.Time
	received, sent              uint64
	pending                     []frame
	closed                      bool
	closeDone                   chan struct{}
	closeErr                    error
}

func (c *clientConn) request(ctx context.Context, method, route string, data []byte) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.url+route, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	r.Header = c.headers.Clone()
	r.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBatch+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBatch {
		return nil, fmt.Errorf("oversized HTTP frame batch")
	}
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return nil, &HTTPError{resp.StatusCode}
	}
	return body, nil
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTPS session returned HTTP %d", e.Status) }
func retryable(err error) bool {
	if e, ok := err.(*HTTPError); ok {
		return e.Status == 408 || e.Status == 429 || e.Status >= 500
	}
	return err != nil
}
func (c *clientConn) exchange(ctx context.Context, method, route string, data []byte) ([]byte, error) {
	until := time.Now().Add(LeaseTime - 5*time.Second)
	for {
		body, err := c.request(ctx, method, route, data)
		if err == nil {
			return body, nil
		}
		if ctx.Err() != nil || !retryable(err) || time.Now().After(until) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func (c *clientConn) operationContext(read bool) (context.Context, context.CancelFunc, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.writeDeadline
	if read {
		d = c.readDeadline
	}
	if !d.IsZero() {
		ctx, cancel := context.WithDeadline(c.ctx, d)
		return ctx, cancel, c.limit
	}
	return c.ctx, func() {}, c.limit
}
func (c *clientConn) ReadMessage() (int, []byte, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	ctx, done, limit := c.operationContext(true)
	defer done()
	for len(c.pending) == 0 {
		body, err := c.exchange(ctx, "GET", fmt.Sprintf("/receive?after=%d", c.received), nil)
		if err != nil {
			return 0, nil, err
		}
		frames, err := decode(body, limit)
		if err != nil {
			return 0, nil, err
		}
		for i, f := range frames {
			if f.Seq != c.received+uint64(i)+1 {
				return 0, nil, fmt.Errorf("out-of-order HTTPS frame")
			}
		}
		c.pending = frames
	}
	f := c.pending[0]
	c.pending = c.pending[1:]
	c.received = f.Seq
	if f.Kind == 8 {
		return 0, nil, io.EOF
	}
	return f.Kind, f.Data, nil
}
func (c *clientConn) WriteMessage(kind int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if (kind != 1 && kind != 2 && kind != 8) || len(data) > MaxFrame {
		return fmt.Errorf("invalid frame")
	}
	ctx, done, _ := c.operationContext(false)
	defer done()
	batch := encode([]frame{{kind, c.sent + 1, data}})
	if _, err := c.exchange(ctx, "POST", "/send", batch); err != nil {
		return err
	}
	c.sent++
	return nil
}
func (c *clientConn) ReadJSON(v any) error {
	_, p, e := c.ReadMessage()
	if e != nil {
		return e
	}
	return json.Unmarshal(p, v)
}
func (c *clientConn) WriteJSON(v any) error {
	p, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return c.WriteMessage(1, p)
}
func (c *clientConn) NextReader() (int, io.Reader, error) {
	k, p, e := c.ReadMessage()
	return k, bytes.NewReader(p), e
}
func (c *clientConn) WriteControl(k int, p []byte, d time.Time) error {
	if k == 8 {
		c.SetWriteDeadline(d)
		return c.WriteMessage(k, p)
	}
	return nil
}
func (c *clientConn) SetReadLimit(n int64) { c.mu.Lock(); c.limit = n; c.mu.Unlock() }
func (c *clientConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.readDeadline = d
	c.mu.Unlock()
	return nil
}
func (c *clientConn) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	c.writeDeadline = d
	c.mu.Unlock()
	return nil
}
func (c *clientConn) Subprotocol() string  { return c.protocol }
func (c *clientConn) LocalAddr() net.Addr  { return httpAddr("https-client") }
func (c *clientConn) RemoteAddr() net.Addr { return httpAddr(c.url) }
func (c *clientConn) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		return c.closeErr
	}
	c.closed = true
	c.closeDone = make(chan struct{})
	c.mu.Unlock()
	c.cancel()
	ctx, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	_, err := c.request(ctx, "POST", "/close", nil)
	c.http.CloseIdleConnections()
	c.mu.Lock()
	c.closeErr = err
	close(c.closeDone)
	c.mu.Unlock()
	return err
}

type httpAddr string

func (a httpAddr) Network() string { return "https" }
func (a httpAddr) String() string  { return string(a) }
