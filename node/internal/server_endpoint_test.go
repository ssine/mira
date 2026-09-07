package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestServerEndpointDiscoverySelectsHealthyPriority(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/healthz" {
			t.Errorf("direct endpoint received %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Error("health probe exposed authorization")
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok"})
	}))
	defer direct.Close()

	var canonical *httptest.Server
	canonical = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case serverDiscoveryPath:
			if request.Header.Get("Authorization") != "" {
				t.Error("well-known request exposed authorization")
			}
			_ = json.NewEncoder(response).Encode(serverDiscoveryDocument{Version: 1, Endpoints: []serverDiscoveryEndpoint{
				{URL: canonical.URL, Priority: 20, Scope: "global"},
				{URL: direct.URL, Priority: 10, Scope: "direct"},
			}})
		case "/healthz":
			_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(response, request)
		}
	}))
	defer canonical.Close()

	selector := newServerEndpointSelector(canonical.URL, canonical.Client())
	if selected := selector.endpoint(context.Background()); selected != direct.URL {
		t.Fatalf("selected %q, want direct endpoint %q", selected, direct.URL)
	}
	if fallback := selector.fail(direct.URL); fallback != canonical.URL {
		t.Fatalf("fallback %q, want canonical endpoint %q", fallback, canonical.URL)
	}
}

func TestControlRequestsUseDiscoveredEndpointAndFailOver(t *testing.T) {
	var directAvailable atomic.Bool
	directAvailable.Store(true)
	direct := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok"})
			return
		}
		if !directAvailable.Load() {
			http.Error(response, "direct unavailable", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"endpoint": "direct"})
	}))
	defer direct.Close()
	var canonical *httptest.Server
	canonical = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == serverDiscoveryPath {
			_ = json.NewEncoder(response).Encode(serverDiscoveryDocument{Version: 1, Endpoints: []serverDiscoveryEndpoint{
				{URL: direct.URL, Priority: 10, Scope: "direct"},
				{URL: canonical.URL, Priority: 20, Scope: "global"},
			}})
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"endpoint": "canonical"})
	}))
	defer canonical.Close()

	client := newControlClient(config{ServerURL: canonical.URL}, nil)
	defer client.close()
	var result map[string]string
	if err := client.requestJSON(context.Background(), http.MethodGet, "/probe", "test-token", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result["endpoint"] != "direct" {
		t.Fatalf("first request used %q, want direct", result["endpoint"])
	}
	directAvailable.Store(false)
	if err := client.requestJSON(context.Background(), http.MethodGet, "/probe", "test-token", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result["endpoint"] != "canonical" {
		t.Fatalf("failed-over request used %q, want canonical", result["endpoint"])
	}
}

func TestServerEndpointDiscoveryFallsBackWhenDirectProbeFails(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, "not healthy", http.StatusServiceUnavailable)
	}))
	defer direct.Close()
	var canonical *httptest.Server
	canonical = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == serverDiscoveryPath {
			_ = json.NewEncoder(response).Encode(serverDiscoveryDocument{Version: 1, Endpoints: []serverDiscoveryEndpoint{
				{URL: direct.URL, Priority: 10, Scope: "direct"},
				{URL: canonical.URL, Priority: 20, Scope: "global"},
			}})
			return
		}
		_ = json.NewEncoder(response).Encode(map[string]string{"status": "ok"})
	}))
	defer canonical.Close()

	selector := newServerEndpointSelector(canonical.URL, canonical.Client())
	if selected := selector.endpoint(context.Background()); selected != canonical.URL {
		t.Fatalf("selected %q, want canonical endpoint %q", selected, canonical.URL)
	}
}

func TestServerEndpointDiscoveryRejectsHTTPSDowngrade(t *testing.T) {
	canonical, _ := urlForTest(t, "https://mira.example.test")
	if endpoint, ok := validDiscoveredServerEndpoint(canonical, "http://direct.example.test:24443"); ok || endpoint != "" {
		t.Fatalf("accepted HTTPS downgrade: %q", endpoint)
	}
	if endpoint, ok := validDiscoveredServerEndpoint(canonical, "https://direct.example.test:24443/"); !ok || endpoint != "https://direct.example.test:24443" {
		t.Fatalf("rejected valid direct endpoint: %q, %v", endpoint, ok)
	}
}

func urlForTest(t *testing.T, raw string) (*url.URL, string) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed, raw
}
