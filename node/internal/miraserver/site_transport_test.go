package miraserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func siteTestTransport(t *testing.T) (*siteTransport, *atomic.Int32) {
	t.Helper()
	dials := &atomic.Int32{}
	tr := newSiteTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return tr, dials
}

func siteTestRead(t *testing.T, tr *siteTransport, req *http.Request) string {
	t.Helper()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestSiteTransportReusesHTTPAndHTTPSConnections(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 1 {
					t.Errorf("unexpected shared HTTP protocol: %s", r.Proto)
				}
				body, _ := io.ReadAll(r.Body)
				w.Write(body)
			}))
			if secure {
				server.EnableHTTP2 = true
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			tr, dials := siteTestTransport(t)
			if secure {
				tr.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			}
			for _, method := range []string{"GET", "HEAD", "POST", "PUT", "GET", "POST"} {
				req, _ := http.NewRequest(method, server.URL, strings.NewReader("payload"))
				body := siteTestRead(t, tr, req)
				if method != "HEAD" && body != "payload" {
					t.Fatalf("body changed: %q", body)
				}
			}
			if dials.Load() != 1 {
				t.Fatalf("sequential requests opened %d connections", dials.Load())
			}
			tr.CloseIdleConnections()
			req, _ := http.NewRequest("GET", server.URL, nil)
			siteTestRead(t, tr, req)
			if dials.Load() != 2 {
				t.Fatalf("closed pool reused an old connection: %d", dials.Load())
			}
		})
	}
}

func TestSiteTransportDoesNotReplayLostResponses(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST"} {
		t.Run(method, func(t *testing.T) {
			var lost atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/lost" {
					lost.Add(1)
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
				io.WriteString(w, "ready")
			}))
			defer server.Close()
			tr, dials := siteTestTransport(t)
			req, _ := http.NewRequest("GET", server.URL, nil)
			siteTestRead(t, tr, req) // Mark the connection reusable: stock Transport retries.
			req, _ = http.NewRequest(method, server.URL+"/lost", strings.NewReader("body"))
			req.Header.Set("Idempotency-Key", "upstream-owned")
			resp, err := tr.RoundTrip(req)
			if resp != nil || !errors.Is(err, errSiteReplay) {
				t.Fatalf("expected an explicit replay failure, response=%v error=%v", resp, err)
			}
			if lost.Load() != 1 || dials.Load() != 1 {
				t.Fatalf("failed request was retried: requests=%d dials=%d", lost.Load(), dials.Load())
			}
			req, _ = http.NewRequest("GET", server.URL, nil)
			if siteTestRead(t, tr, req) != "ready" || dials.Load() != 2 {
				t.Fatal("a later independent request did not recover")
			}
		})
	}
}

type siteFailWriteConn struct {
	net.Conn
	failed *atomic.Int32
}

func (conn *siteFailWriteConn) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(" /lost ")) {
		conn.failed.Add(1)
		conn.Conn.Close()
		return 0, net.ErrClosed
	}
	return conn.Conn.Write(p)
}

func TestSiteTransportDoesNotRetryZeroByteWrites(t *testing.T) {
	// Transport retries nothingWrittenError even for non-idempotent methods
	// with GetBody. Exercise loss before the first byte, not just a lost reply.
	var failed, dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ready")
	}))
	defer server.Close()
	tr := newSiteTransport(func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &siteFailWriteConn{Conn: conn, failed: &failed}, nil
	})
	defer tr.CloseIdleConnections()
	for _, method := range []string{"GET", "POST"} {
		req, _ := http.NewRequest("GET", server.URL, nil)
		siteTestRead(t, tr, req)
		before := dials.Load()
		req, _ = http.NewRequest(method, server.URL+"/lost", strings.NewReader("body"))
		resp, err := tr.RoundTrip(req)
		if resp != nil || !errors.Is(err, errSiteReplay) || dials.Load() != before {
			t.Fatalf("zero-byte failure retried: response=%v error=%v dials=%d", resp, err, dials.Load())
		}
	}
	if failed.Load() != 2 {
		t.Fatalf("expected one write attempt per request, got %d", failed.Load())
	}
}

func TestSiteTransportRetryCannotUseAnotherIdleConnection(t *testing.T) {
	// Give an attempted retry another ready pooled socket. Cancellation alone
	// is insufficient: getConn can select that socket concurrently with Done.
	for iteration := 0; iteration < 30; iteration++ {
		var warm, lost atomic.Int32
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/warm" {
				if warm.Add(1) == 2 {
					close(release)
				}
				<-release
				io.WriteString(w, "ready")
				return
			}
			lost.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}))
		tr, _ := siteTestTransport(t)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Go(func() {
				req, _ := http.NewRequest("GET", server.URL+"/warm", nil)
				siteTestRead(t, tr, req)
			})
		}
		wg.Wait()
		req, _ := http.NewRequest("GET", server.URL+"/lost", nil)
		resp, err := tr.RoundTrip(req)
		if resp != nil || !errors.Is(err, errSiteReplay) || lost.Load() != 1 {
			t.Fatalf("retry used another idle connection: response=%v error=%v requests=%d", resp, err, lost.Load())
		}
		tr.CloseIdleConnections()
		server.Close()
	}
}

func TestSiteTransportIdleExpiryAndCancellation(t *testing.T) {
	ended := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			ended <- struct{}{}
			return
		}
		io.WriteString(w, "ready")
	}))
	defer server.Close()
	tr, dials := siteTestTransport(t)
	tr.IdleConnTimeout = 20 * time.Millisecond
	req, _ := http.NewRequest("GET", server.URL, nil)
	siteTestRead(t, tr, req)
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ = http.NewRequestWithContext(ctx, "GET", server.URL+"/stream", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatal("expired idle connection was retained")
	}
	cancel()
	io.ReadAll(resp.Body)
	resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not close the upstream stream")
	}
}
