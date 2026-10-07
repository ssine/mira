package miraserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

var errSiteReplay = errors.New("site request lost its upstream connection; automatic replay is disabled")

type siteAttemptKey struct{}
type siteAttempt struct{ acquired atomic.Bool }

// siteTransport keeps the standard HTTP streaming/upgrade implementation and
// bounded idle pool, but permits only one connection assignment per request.
// Transport otherwise retries some failed requests on a reused connection.
type siteTransport struct {
	*http.Transport
	retired atomic.Bool
}

// A proxy removed from the bounded cache must not leave a new idle stream
// behind when an already-running response finishes.
func (tr *siteTransport) Retire() {
	tr.retired.Store(true)
	tr.CloseIdleConnections()
}

func newSiteTransport(dial func(context.Context, string, string) (net.Conn, error)) *siteTransport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		// Transport detaches dial cancellation from the request. Do not create
		// a replacement Node stream for an internally retried request either.
		if attempt, ok := ctx.Value(siteAttemptKey{}).(*siteAttempt); ok && attempt.acquired.Load() {
			return nil, errSiteReplay
		}
		return dial(ctx, network, address)
	}
	tr.MaxIdleConns = 4
	tr.MaxIdleConnsPerHost = 4
	// Admission is bounded by the shared channel stream budget, rather than
	// assigning independent fixed quotas to each site or serving Node.
	tr.MaxConnsPerHost = 0
	tr.IdleConnTimeout = 30 * time.Minute
	tr.ResponseHeaderTimeout = 0
	tr.DisableKeepAlives = false
	tr.DisableCompression = true
	// GotConn fences the exclusive HTTP/1 connection before request bytes can
	// be queued. Do not negotiate HTTP/2, whose shared connections and internal
	// retry machinery have a different ownership boundary.
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	tr.ForceAttemptHTTP2 = false
	return &siteTransport{Transport: tr}
}

func (tr *siteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tr.retired.Load() {
		return nil, net.ErrClosed
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	attempt := &siteAttempt{}
	ctx = context.WithValue(ctx, siteAttemptKey{}, attempt)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) {
			if attempt.acquired.Load() {
				cancel(errSiteReplay)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			if attempt.acquired.Swap(true) {
				// getConn can select an idle socket at the same time as context
				// cancellation. Close it synchronously, before roundTrip's writer
				// can send anything. HTTP/1 assigns it exclusively to this request.
				_ = info.Conn.Close()
				cancel(errSiteReplay)
			}
		},
	})
	resp, err := tr.Transport.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel(err)
		return nil, err
	}
	body := &siteResponseBody{ReadCloser: resp.Body, cancel: func(cause error) {
		cancel(cause)
		if tr.retired.Load() {
			tr.CloseIdleConnections()
		}
	}}
	if upgraded, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &siteUpgradeBody{siteResponseBody: body, Writer: upgraded}
	} else {
		resp.Body = body
	}
	return resp, nil
}

type siteResponseBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (body *siteResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.cancel(nil)
	return err
}

// Preserve the ReadWriteCloser required by ReverseProxy for HTTP upgrades.
type siteUpgradeBody struct {
	*siteResponseBody
	io.Writer
}
