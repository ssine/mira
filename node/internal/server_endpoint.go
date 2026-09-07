package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	serverDiscoveryPath      = "/.well-known/mira"
	serverDiscoveryBodyLimit = 64 * 1024
	serverHealthBodyLimit    = 4 * 1024
	serverProbeTimeout       = 3 * time.Second
	serverEndpointLimit      = 16
)

type serverDiscoveryDocument struct {
	Version   int                       `json:"version"`
	Endpoints []serverDiscoveryEndpoint `json:"endpoints"`
}

type serverDiscoveryEndpoint struct {
	URL      string `json:"url"`
	Priority int    `json:"priority"`
	Scope    string `json:"scope"`
}

// serverEndpointSelector keeps the configured URL as the durable identity
// anchor while allowing a Server-controlled well-known document to advertise
// a lower-latency ingress. Selection is process-local derived state: credentials
// and configuration are never rebound to a discovered URL.
type serverEndpointSelector struct {
	canonical string
	http      *http.Client

	mu          sync.Mutex
	initialized bool
	endpoints   []string
	current     int
}

func newServerEndpointSelector(canonical string, client *http.Client) *serverEndpointSelector {
	return &serverEndpointSelector{canonical: strings.TrimRight(canonical, "/"), http: client}
}

func (selector *serverEndpointSelector) endpoint(ctx context.Context) string {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	if !selector.initialized {
		selector.initializeLocked(ctx)
	}
	return selector.endpoints[selector.current]
}

func (selector *serverEndpointSelector) initializeLocked(ctx context.Context) {
	selector.initialized = true
	selector.endpoints = []string{selector.canonical}
	discovered, ok := discoverServerEndpoints(ctx, selector.canonical, selector.http)
	if !ok {
		return
	}
	selector.endpoints = discovered
	for index, endpoint := range discovered {
		if endpoint == selector.canonical || probeServerEndpoint(ctx, endpoint, selector.http) {
			selector.current = index
			break
		}
	}
	if selected := selector.endpoints[selector.current]; selected != selector.canonical {
		Log("selected discovered Mira Server endpoint", map[string]any{
			"serverUrl": selector.canonical,
			"endpoint":  selected,
		})
	}
}

// fail advances only when the caller failed on the currently selected
// endpoint. The next operation reuses the new selection; after every candidate
// has failed, the selector cycles so a recovered ingress can be retried.
func (selector *serverEndpointSelector) fail(endpoint string) string {
	selector.mu.Lock()
	defer selector.mu.Unlock()
	if len(selector.endpoints) < 2 || selector.endpoints[selector.current] != endpoint {
		return selector.endpoints[selector.current]
	}
	selector.current = (selector.current + 1) % len(selector.endpoints)
	return selector.endpoints[selector.current]
}

func discoverServerEndpoints(ctx context.Context, canonical string, client *http.Client) ([]string, bool) {
	requestContext, cancel := context.WithTimeout(ctx, serverProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, canonical+serverDiscoveryPath, nil)
	if err != nil {
		return nil, false
	}
	request.Header.Set("Accept", "application/json")
	response, err := endpointProbeClient(client).Do(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, serverDiscoveryBodyLimit+1))
	if err != nil || len(content) > serverDiscoveryBodyLimit {
		return nil, false
	}
	var document serverDiscoveryDocument
	if json.Unmarshal(content, &document) != nil || document.Version != 1 || len(document.Endpoints) == 0 || len(document.Endpoints) > serverEndpointLimit {
		return nil, false
	}
	sort.SliceStable(document.Endpoints, func(left, right int) bool {
		return document.Endpoints[left].Priority < document.Endpoints[right].Priority
	})
	canonicalURL, err := url.Parse(canonical)
	if err != nil {
		return nil, false
	}
	endpoints := make([]string, 0, len(document.Endpoints)+1)
	seen := make(map[string]bool, len(document.Endpoints)+1)
	for _, advertised := range document.Endpoints {
		endpoint, ok := validDiscoveredServerEndpoint(canonicalURL, advertised.URL)
		if !ok || seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		endpoints = append(endpoints, endpoint)
	}
	if !seen[canonical] {
		endpoints = append(endpoints, canonical)
	}
	if len(endpoints) == 0 {
		return nil, false
	}
	return endpoints, true
}

func validDiscoveredServerEndpoint(canonical *url.URL, raw string) (string, bool) {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return "", false
	}
	parsed, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	// A well-known document fetched over authenticated HTTPS cannot downgrade a
	// Node credential or WebSocket authentication token to plaintext HTTP.
	if canonical.Scheme == "https" && parsed.Scheme != "https" {
		return "", false
	}
	return strings.TrimRight(parsed.String(), "/"), true
}

func probeServerEndpoint(ctx context.Context, endpoint string, client *http.Client) bool {
	requestContext, cancel := context.WithTimeout(ctx, serverProbeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint+"/healthz", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Accept", "application/json")
	response, err := endpointProbeClient(client).Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, serverHealthBodyLimit+1))
	if err != nil || len(content) > serverHealthBodyLimit {
		return false
	}
	var health struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(content, &health) == nil && health.Status == "ok"
}

func endpointProbeClient(client *http.Client) *http.Client {
	probe := *client
	probe.Timeout = serverProbeTimeout
	probe.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &probe
}

func serverWebSocketURL(server, route string) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported control server scheme: %s", parsed.Scheme)
	}
	parsed.Path = route
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
