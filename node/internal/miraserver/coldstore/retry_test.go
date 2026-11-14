package coldstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Dropped connections and provider 5xx responses are retried; client errors
// such as AccessDenied are not.
func TestObjectRequestsRetryTransientFailures(t *testing.T) {
	var puts, gets atomic.Int32
	stored := []byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			switch puts.Add(1) {
			case 1: // connection dropped mid-request
				hijacker, _ := w.(http.Hijacker)
				connection, _, _ := hijacker.Hijack()
				connection.Close()
				return
			case 2:
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			stored = body
		case http.MethodGet:
			if gets.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if r.URL.Path == "/bucket/denied" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
				return
			}
			_, _ = w.Write(stored)
		}
	}))
	defer server.Close()
	store, err := NewS3(ObjectConfig{Endpoint: server.URL, Region: "r", Bucket: "bucket", AccessKeyID: "id", SecretAccessKey: "secret", PathStyle: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "object", []byte("payload")); err != nil || puts.Load() != 3 {
		t.Fatalf("put: %v after %d attempts", err, puts.Load())
	}
	if body, err := store.Get(ctx, "object"); err != nil || string(body) != "payload" || gets.Load() != 2 {
		t.Fatalf("get: %q %v after %d attempts", body, err, gets.Load())
	}
	before := gets.Load()
	if _, err := store.Get(ctx, "denied"); err == nil || gets.Load() != before+1 {
		t.Fatalf("access denied retried or accepted: %v (%d attempts)", err, gets.Load()-before)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Put(canceled, "object", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled put: %v", err)
	}
}
