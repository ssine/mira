package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func harness(t *testing.T, fault string) (Conn, *serverConn, *Server) {
	t.Helper()
	registry := NewServer(func(r *http.Request) (string, int) {
		if r.Header.Get("Authorization") != "Bearer test-only" {
			return "", 401
		}
		return "test-owner", 0
	})
	t.Cleanup(registry.Close)
	opened := make(chan *serverConn, 1)
	var dropped atomic.Bool
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connect" {
			if r.Method == "GET" {
				http.Error(w, "Upgrade blocked", 426)
				return
			}
			c, err := registry.Upgrade(w, r, "test-v1")
			if err == nil {
				opened <- c.(*serverConn)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/"+fault) && dropped.CompareAndSwap(false, true) {
			record := httptest.NewRecorder()
			registry.ServeHTTP(record, r)
			if fault == "receive" {
				for k, v := range record.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(record.Code)
				_, _ = w.Write(record.Body.Bytes()[:record.Body.Len()/2])
			}
			raw, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = raw.Close()
			}
			return
		}
		registry.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)
	dialer := *websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // isolated test server
	dialer.Subprotocols = []string{"test-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte("test-only"))}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, _, err := Dial(ctx, &dialer, "wss"+strings.TrimPrefix(host.URL, "https")+"/connect", nil, "auto")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, <-opened, registry
}

func TestHTTPSAutoFallbackBinaryAndLostResponses(t *testing.T) {
	for _, fault := range []string{"", "send", "receive"} {
		t.Run(fault, func(t *testing.T) {
			client, server, _ := harness(t, fault)
			payload := bytes.Repeat([]byte{0, 0xff, 0x80, 2, 3}, 13000)
			var calls atomic.Int32
			done := make(chan error, 1)
			go func() {
				kind, data, err := server.ReadMessage()
				if err == nil {
					calls.Add(1)
					err = server.WriteMessage(kind, data)
				}
				done <- err
			}()
			if err := client.WriteMessage(2, payload); err != nil {
				t.Fatal(err)
			}
			kind, data, err := client.ReadMessage()
			if err != nil || kind != 2 || !bytes.Equal(data, payload) {
				t.Fatalf("binary delivery kind=%d bytes=%d err=%v", kind, len(data), err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("native side effect ran %d times", calls.Load())
			}
			// A second operation proves the acknowledged cursor freed retained frames.
			if err := server.WriteMessage(1, []byte("next")); err != nil {
				t.Fatal(err)
			}
			if _, data, err := client.ReadMessage(); err != nil || string(data) != "next" {
				t.Fatalf("next delivery %q %v", data, err)
			}
		})
	}
}

func sendBatch(c *serverConn, frames []frame) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/send", bytes.NewReader(encode(frames)))
	c.sendHTTP(w, r)
	return w
}
func TestHTTPSRejectsConflictingRetryAndMalformedBatchAtomically(t *testing.T) {
	_, c, _ := harness(t, "")
	if w := sendBatch(c, []frame{{1, 1, []byte("first")}}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if _, p, e := c.ReadMessage(); e != nil || string(p) != "first" {
		t.Fatal(e)
	}
	if w := sendBatch(c, []frame{{1, 1, []byte("first")}}); w.Code != 204 {
		t.Fatal("identical retry", w.Code)
	}
	if w := sendBatch(c, []frame{{1, 1, []byte("changed")}}); w.Code != 409 {
		t.Fatal("conflicting retry", w.Code)
	}
	if w := sendBatch(c, []frame{{1, 2, []byte("never execute")}, {1, 4, nil}}); w.Code != 409 {
		t.Fatal("gap", w.Code)
	}
	select {
	case <-c.in:
		t.Fatal("malformed batch partially reached application")
	default:
	}
	if w := sendBatch(c, []frame{{1, 2, []byte("second")}}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if _, p, e := c.ReadMessage(); e != nil || string(p) != "second" {
		t.Fatal(e)
	}
}
func TestHTTPSExpiredSessionCannotReplay(t *testing.T) {
	client, c, _ := harness(t, "")
	_ = c.Close()
	err := client.WriteMessage(1, []byte("must fail"))
	if e, ok := err.(*HTTPError); !ok || e.Status != 410 {
		t.Fatalf("expired session: %v", err)
	}
}
func TestHTTPSBackpressureAndShutdown(t *testing.T) {
	_, c, registry := harness(t, "")
	payload := make([]byte, 64*1024)
	for i := 0; i < 16; i++ {
		if err := c.WriteMessage(2, payload); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- c.WriteMessage(2, payload) }()
	select {
	case err := <-done:
		t.Fatalf("unbounded queue %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	registry.Close()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer survived shutdown")
	}
	if registry.bytes != 0 {
		t.Fatalf("retained bytes after close: %d", registry.bytes)
	}
}
func TestHTTPSAuthenticationAndReadLimit(t *testing.T) {
	client, c, registry := harness(t, "")
	c.SetReadLimit(4)
	if err := client.WriteMessage(1, []byte("oversized")); err == nil {
		t.Fatal("read limit ignored")
	}
	req := httptest.NewRequest("POST", Prefix+c.id+"/close", nil)
	w := httptest.NewRecorder()
	registry.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("session ID acted as credential", w.Code)
	}
}
func TestHTTPSFallbackCannotBypassAuthentication(t *testing.T) {
	var posts atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts.Add(1)
		}
		http.Error(w, "denied", 403)
	}))
	defer host.Close()
	_, _, err := Dial(context.Background(), websocket.DefaultDialer, "ws"+strings.TrimPrefix(host.URL, "http"), nil, "auto")
	if err == nil || posts.Load() != 1 {
		t.Fatalf("fallback retained authentication err=%v posts=%d", err, posts.Load())
	}
}

func TestHTTPSClosePreservesAcceptedTails(t *testing.T) {
	client, c, _ := harness(t, "")
	if err := c.WriteMessage(2, []byte("final SSH bytes")); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, p, err := client.ReadMessage(); err != nil || string(p) != "final SSH bytes" {
		t.Fatalf("lost tail %q %v", p, err)
	}
	if _, _, err := client.ReadMessage(); err != io.EOF {
		t.Fatalf("expected close after tail: %v", err)
	}
	_, input, _ := harness(t, "")
	if w := sendBatch(input, []frame{{1, 1, []byte("file done")}}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	input.forceClose()
	if _, p, err := input.ReadMessage(); err != nil || string(p) != "file done" {
		t.Fatalf("accepted input lost on close: %q %v", p, err)
	}
}

func TestCustomProxyCarriesWebSocketAndHTTPSFallback(t *testing.T) {
	registry := NewServer(func(r *http.Request) (string, int) { return "proxy-test", 0 })
	defer registry.Close()
	opened := make(chan *serverConn, 1)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connect" {
			if r.Method == "GET" {
				http.Error(w, "proxy denies Upgrade", 403)
				return
			}
			c, err := registry.Upgrade(w, r, "proxy-v1")
			if err == nil {
				opened <- c.(*serverConn)
			}
			return
		}
		registry.ServeHTTP(w, r)
	}))
	defer host.Close()
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != strings.TrimPrefix(host.URL, "https://") {
			http.Error(w, "unexpected proxy target", 400)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "connect failed", 502)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		tunnels.Add(1)
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer upstream.Close(); defer client.Close(); _, _ = io.Copy(upstream, client) }()
		go func() { defer upstream.Close(); defer client.Close(); _, _ = io.Copy(client, upstream) }()
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	dialer := *websocket.DefaultDialer
	dialer.Proxy = http.ProxyURL(proxyURL)
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // isolated TLS fixture
	dialer.Subprotocols = []string{"proxy-v1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := Dial(ctx, &dialer, "wss"+strings.TrimPrefix(host.URL, "https")+"/connect", nil, "auto")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	c := <-opened
	if err := c.WriteMessage(2, []byte("proxied binary")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := client.ReadMessage(); err != nil || string(data) != "proxied binary" {
		t.Fatal(string(data), err)
	}
	if tunnels.Load() < 2 {
		t.Fatalf("both transports must use explicit proxy: %d tunnels", tunnels.Load())
	}
}
