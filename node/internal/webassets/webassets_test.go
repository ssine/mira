package webassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestServeAllAssets(t *testing.T) {
	for route, spec := range specs {
		t.Run(route, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, route+"?cache-bust=1", nil)
			response := httptest.NewRecorder()
			if !Serve(response, request) {
				t.Fatal("Serve did not handle known asset")
			}
			result := response.Result()
			defer result.Body.Close()
			if result.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusOK)
			}
			if got := result.Header.Get("Content-Type"); got != spec.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, spec.contentType)
			}
			if got := result.Header.Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("Cache-Control = %q, want no-cache", got)
			}
			if got := result.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Fatalf("unexpected Content-Security-Policy: %q", got)
			}
			if result.Header.Get("X-Content-Type-Options") != "nosniff" || result.Header.Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("security headers are missing")
			}
			body, err := io.ReadAll(result.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, assets[route].payload) {
				t.Fatal("response body differs from embedded asset")
			}
			digest := sha256.Sum256(body)
			wantETag := `"` + hex.EncodeToString(digest[:]) + `"`
			if got := result.Header.Get("ETag"); got != wantETag {
				t.Fatalf("ETag = %q, want %q", got, wantETag)
			}
			if got := result.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
				t.Fatalf("Content-Length = %q, want %d", got, len(body))
			}
		})
	}
}

func TestServeHeadAndConditionalRequests(t *testing.T) {
	entry := assets["/app.js"]

	head := httptest.NewRecorder()
	if !Serve(head, httptest.NewRequest(http.MethodHead, "/app.js", nil)) {
		t.Fatal("HEAD was not handled")
	}
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD response = status %d, %d body bytes", head.Code, head.Body.Len())
	}
	if got := head.Header().Get("Content-Length"); got != strconv.Itoa(len(entry.payload)) {
		t.Fatalf("HEAD Content-Length = %q", got)
	}

	for _, value := range []string{entry.etag, "W/" + entry.etag, `"other", W/` + entry.etag, "*"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/app.js", nil)
		request.Header.Set("If-None-Match", value)
		if !Serve(response, request) {
			t.Fatalf("conditional request with %q was not handled", value)
		}
		if response.Code != http.StatusNotModified || response.Body.Len() != 0 {
			t.Fatalf("If-None-Match %q response = status %d, %d body bytes", value, response.Code, response.Body.Len())
		}
		if response.Header().Get("Content-Length") != "" {
			t.Fatalf("304 response includes Content-Length for %q", value)
		}
	}
}

func TestServiceWorkerVersionAndScope(t *testing.T) {
	response := httptest.NewRecorder()
	if !Serve(response, httptest.NewRequest(http.MethodGet, "/service-worker.js", nil)) {
		t.Fatal("service worker was not handled")
	}
	if got := response.Header().Get("Service-Worker-Allowed"); got != "/" {
		t.Fatalf("Service-Worker-Allowed = %q, want /", got)
	}
	body := response.Body.String()
	if strings.Contains(body, "__MIRA_OFFLINE_VERSION__") {
		t.Fatal("service worker still contains the offline version placeholder")
	}
	if !strings.Contains(body, `const offlineAssets = ["/offline.html", "/offline.css", "/offline.js", "/icons/mira.svg"]`) {
		t.Fatal("service worker lost its public-only offline fallback")
	}
}

func TestUnhandledRequestsDelegate(t *testing.T) {
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/missing", nil),
		httptest.NewRequest(http.MethodPost, "/app.js", nil),
		httptest.NewRequest(http.MethodGet, "/app%2Ejs", nil),
	} {
		response := httptest.NewRecorder()
		if Serve(response, request) {
			t.Fatalf("Serve unexpectedly handled %s %s", request.Method, request.URL.EscapedPath())
		}
		if len(response.Header()) != 0 || response.Body.Len() != 0 {
			t.Fatal("unhandled request mutated the response")
		}
	}

	wrapped := Wrap(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusTeapot)
	}))
	response := httptest.NewRecorder()
	wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api", nil))
	if response.Code != http.StatusTeapot {
		t.Fatalf("wrapped fallback status = %d, want %d", response.Code, http.StatusTeapot)
	}

	notFound := httptest.NewRecorder()
	NewHandler().ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("standalone handler status = %d, want %d", notFound.Code, http.StatusNotFound)
	}
}
