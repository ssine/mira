package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

var portSiteName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,59}$`)
var portSiteRoute = regexp.MustCompile(`^/v1/sites/([a-z0-9-]{1,60})$`)

type portSite struct {
	ID                  string    `json:"siteId"`
	Name                string    `json:"name"`
	NodeID              string    `json:"nodeId"`
	Port                int       `json:"port"`
	Scheme              string    `json:"scheme"`
	Enabled             bool      `json:"enabled"`
	Revision            int64     `json:"revision"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
	URL                 string    `json:"url,omitempty"`
	Phase               string    `json:"phase,omitempty"`
	approved, supported bool
}

// The p- prefix cannot collide with released static preview IDs, which contain
// no hyphens. Both kinds use the existing wildcard ingress and TLS certificate.
func (s portSite) label() string { return "p-" + s.Name }

type siteProxy struct {
	revision  int64
	proxy     *httputil.ReverseProxy
	transport *http.Transport
	used      time.Time
}
type siteProxyStore struct {
	sync.Mutex
	items map[string]*siteProxy
}

const portSiteColumns = `s.site_id::text,s.name,s.node_id::text,s.port,s.scheme,s.enabled,s.revision,s.created_at,s.updated_at,
 n.approval_status='approved',COALESCE((n.capabilities->>'portSitesV1')::boolean,false)`

func scanPortSite(row pgx.Row) (*portSite, error) {
	s := &portSite{}
	err := row.Scan(&s.ID, &s.Name, &s.NodeID, &s.Port, &s.Scheme, &s.Enabled, &s.Revision, &s.CreatedAt, &s.UpdatedAt, &s.approved, &s.supported)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func (server *Server) getPortSite(ctx context.Context, key string) (*portSite, error) {
	return scanPortSite(server.pool.QueryRow(ctx, `SELECT `+portSiteColumns+` FROM mira_port_sites s JOIN codex_nodes n USING(node_id)
 WHERE (s.site_id::text=$1 OR s.name=$1) AND s.deleted_at IS NULL`, key))
}

func (server *Server) decoratePortSite(s *portSite, r *http.Request) *portSite {
	if origin := server.previewOrigin(r); origin != "" {
		u, _ := url.Parse(origin)
		u.Host = s.label() + "." + u.Host
		s.URL = u.String()
	}
	s.Phase = "available"
	if !s.Enabled {
		s.Phase = "disabled"
	} else if !s.approved {
		s.Phase = "revoked"
	} else if !s.supported {
		s.Phase = "upgrade_required"
	} else if !server.channel.IsConnected(s.NodeID) {
		s.Phase = "offline"
	}
	// Available describes the Mira transport. Backend application health is not
	// inferred or probed; the service itself controls its protocol and readiness.
	return s
}

func siteError(status int, message string) error {
	return &HTTPError{Status: status, Code: "site_unavailable", Message: message}
}

func (server *Server) checkPortSiteTarget(ctx context.Context, nodeID string) error {
	if !uuidPattern.MatchString(nodeID) {
		return siteError(400, "nodeId must be a Node UUID")
	}
	n, err := server.nodes.Get(ctx, nodeID, false)
	if err != nil {
		return err
	}
	if n == nil {
		return siteError(404, "target Node is unavailable")
	}
	if n.Capabilities["portSitesV1"] != true {
		return siteError(409, "upgrade the target Node to support portSitesV1")
	}
	return nil
}

func (server *Server) managePortSite(ctx context.Context, actor *foundation.Principal, action, key string, body map[string]any, r *http.Request) (any, error) {
	if action == "list" {
		limit := 50
		after, _ := body["after"].(string)
		if value, ok := body["limit"].(float64); ok {
			if value < 1 || value > 100 || value != float64(int(value)) {
				return nil, siteError(400, "limit must be 1–100")
			}
			limit = int(value)
		}
		if len(after) > 60 {
			return nil, siteError(400, "invalid cursor")
		}
		rows, err := server.pool.Query(ctx, `SELECT `+portSiteColumns+` FROM mira_port_sites s JOIN codex_nodes n USING(node_id)
 WHERE s.deleted_at IS NULL AND s.name>$1 ORDER BY s.name LIMIT $2`, after, limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []*portSite{}
		for rows.Next() {
			s, err := scanPortSite(rows)
			if err != nil {
				return nil, err
			}
			items = append(items, server.decoratePortSite(s, r))
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		more := len(items) > limit
		if more {
			items = items[:limit]
		}
		next := ""
		if more {
			next = items[len(items)-1].Name
		}
		return map[string]any{"data": items, "hasMore": more, "nextCursor": next}, nil
	}
	if action == "get" {
		s, err := server.getPortSite(ctx, key)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, siteError(404, "site not found")
		}
		return server.decoratePortSite(s, r), nil
	}
	allowed := map[string]bool{"expectedRevision": true}
	if action == "create" || action == "update" {
		allowed["nodeId"] = true
		allowed["port"] = true
		allowed["scheme"] = true
		allowed["enabled"] = true
	}
	if action == "create" {
		delete(allowed, "expectedRevision")
	}
	if action == "create" {
		allowed["name"] = true
	}
	for name := range body {
		if !allowed[name] {
			return nil, siteError(400, "unknown site field: "+name)
		}
	}
	if action != "create" && action != "update" && action != "delete" {
		return nil, siteError(400, "unknown site action")
	}
	var previous *portSite
	var err error
	s := &portSite{Scheme: "http", Enabled: true}
	if action != "create" {
		previous, err = server.getPortSite(ctx, key)
		if err != nil {
			return nil, err
		}
		if previous == nil {
			return nil, siteError(404, "site not found")
		}
		*s = *previous
	}
	if action == "create" {
		s.Name, _ = body["name"].(string)
		if !portSiteName.MatchString(s.Name) || strings.HasSuffix(s.Name, "-") {
			return nil, siteError(400, "name must be a lowercase DNS label starting with a letter, at most 60 characters")
		}
	}
	if value, present := body["nodeId"]; present {
		var ok bool
		s.NodeID, ok = value.(string)
		if !ok {
			return nil, siteError(400, "nodeId must be a string")
		}
	}
	if value, present := body["scheme"]; present {
		var ok bool
		s.Scheme, ok = value.(string)
		if !ok {
			return nil, siteError(400, "invalid scheme")
		}
	}
	if value, present := body["enabled"]; present {
		var ok bool
		s.Enabled, ok = value.(bool)
		if !ok {
			return nil, siteError(400, "enabled must be a boolean")
		}
	}
	if value, present := body["port"]; present {
		number, ok := value.(float64)
		if !ok || number < 1 || number > 65535 || number != math.Trunc(number) {
			return nil, siteError(400, "port must be an integer")
		}
		s.Port = int(number)
	}
	if s.Port < 1 || s.Port > 65535 || (s.Scheme != "http" && s.Scheme != "https") {
		return nil, siteError(400, "a port from 1–65535 and scheme http or https are required")
	}
	if action != "delete" && (action == "create" || previous.NodeID != s.NodeID || previous.Port != s.Port || previous.Scheme != s.Scheme) {
		if err = server.checkPortSiteTarget(ctx, s.NodeID); err != nil {
			return nil, err
		}
	}
	var expected int64
	if action != "create" {
		value, ok := body["expectedRevision"].(float64)
		if !ok || value < 1 || value > (1<<53)-1 || value != math.Trunc(value) {
			return nil, siteError(400, "expectedRevision is required")
		}
		expected = int64(value)
		if previous.Revision != expected {
			return nil, siteError(409, "site revision changed; refresh before updating")
		}
	}
	if action == "delete" {
		s.Enabled = false
	}
	server.siteLifecycle.Lock()
	defer server.siteLifecycle.Unlock()
	tx, err := server.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if action == "create" {
		// Named creation is idempotent. A different body cannot overwrite a URL.
		err = tx.QueryRow(ctx, `INSERT INTO mira_port_sites(site_id,name,node_id,port,scheme,enabled)
 VALUES(gen_random_uuid(),$1,$2::uuid,$3,$4,$5) ON CONFLICT(name) DO NOTHING RETURNING site_id::text`, s.Name, s.NodeID, s.Port, s.Scheme, s.Enabled).Scan(&s.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			var deleted bool
			err = tx.QueryRow(ctx, `SELECT site_id::text,node_id::text,port,scheme,enabled,deleted_at IS NOT NULL FROM mira_port_sites WHERE name=$1`, s.Name).Scan(&s.ID, &s.NodeID, &s.Port, &s.Scheme, &s.Enabled, &deleted)
			if err != nil {
				return nil, err
			}
			requestedNode, _ := body["nodeId"].(string)
			requestedPort, _ := body["port"].(float64)
			requestedScheme := "http"
			if v, ok := body["scheme"].(string); ok {
				requestedScheme = v
			}
			requestedEnabled := true
			if v, ok := body["enabled"].(bool); ok {
				requestedEnabled = v
			}
			if deleted || s.NodeID != requestedNode || s.Port != int(requestedPort) || s.Scheme != requestedScheme || s.Enabled != requestedEnabled {
				return nil, siteError(409, "site name already exists or has been retired")
			}
		}
	} else {
		query := `UPDATE mira_port_sites SET node_id=$2::uuid,port=$3,scheme=$4,enabled=$5,revision=revision+1,updated_at=now() WHERE site_id=$1::uuid AND revision=$6 AND deleted_at IS NULL RETURNING site_id::text`
		if action == "delete" {
			query = `UPDATE mira_port_sites SET node_id=$2::uuid,port=$3,scheme=$4,enabled=$5,deleted_at=now(),revision=revision+1,updated_at=now() WHERE site_id=$1::uuid AND revision=$6 AND deleted_at IS NULL RETURNING site_id::text`
		}
		err = tx.QueryRow(ctx, query, s.ID, s.NodeID, s.Port, s.Scheme, s.Enabled, expected).Scan(&s.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, siteError(409, "site revision changed; refresh before updating")
		}
	}
	if err != nil {
		return nil, err
	}
	if err = foundation.AppendAudit(ctx, tx, foundation.AuditEvent{Action: "site." + action, Principal: actor, TargetNodeID: s.NodeID, Request: r, Metadata: map[string]any{"siteId": s.ID, "port": s.Port, "enabled": s.Enabled}}, server.config.Foundation.TrustProxyHeaders); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	server.invalidateSiteProxy(s.ID)
	if action == "delete" || !s.Enabled {
		server.channel.CloseSiteStreams("", s.ID)
	}
	if action == "delete" {
		return map[string]any{"deleted": true, "siteId": s.ID}, nil
	}
	s, err = server.getPortSite(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	return server.decoratePortSite(s, r), nil
}

func (server *Server) routePortSites(ctx context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	match := portSiteRoute.FindStringSubmatch(r.URL.Path)
	if r.URL.Path != "/v1/sites" && match == nil {
		return false, nil
	}
	actor, err := server.authorize(ctx, w, r, "trusted", authOptions{CSRF: r.Method != "GET", ClientType: "cli"})
	if err != nil || actor == nil {
		return true, err
	}
	key := ""
	if match != nil {
		key = match[1]
	}
	action := ""
	switch r.Method {
	case "GET":
		if key == "" {
			action = "list"
		} else {
			action = "get"
		}
	case "POST":
		if key == "" {
			action = "create"
		}
	case "PATCH":
		if key != "" {
			action = "update"
		}
	case "DELETE":
		if key != "" {
			action = "delete"
		}
	}
	if action == "" {
		return true, siteError(405, "unsupported site method")
	}
	body := map[string]any{}
	if action == "list" {
		body["after"] = r.URL.Query().Get("after")
		if raw := r.URL.Query().Get("limit"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return true, siteError(400, "invalid limit")
			}
			body["limit"] = float64(value)
		}
	} else if action != "get" {
		if err = foundation.ReadJSON(r, &body, 16*1024); err != nil {
			return true, err
		}
	}
	if (action == "create") && server.previewOrigin(r) == "" {
		return true, siteError(409, "current Mira ingress has no configured site domain")
	}
	result, err := server.managePortSite(ctx, actor, action, key, body, r)
	if err != nil {
		return true, err
	}
	return true, writeJSON(w, 200, result)
}

func (server *Server) callSiteTool(ctx context.Context, actor *foundation.Principal, args map[string]any, options channel.InvokeContext) (any, error) {
	data, err := json.Marshal(args)
	if err != nil || len(data) > 16*1024 {
		return nil, siteError(400, "invalid site arguments")
	}
	body := map[string]any{}
	if err = json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	action, _ := body["action"].(string)
	key, _ := body["site"].(string)
	delete(body, "action")
	delete(body, "site")
	r := options.Request
	if r == nil {
		u, err := url.Parse(server.config.Foundation.CodexStoreEndpoint)
		if err != nil || u.Host == "" {
			u = &url.URL{Scheme: "http", Host: net.JoinHostPort(server.config.Foundation.ListenHost, strconv.Itoa(server.config.Foundation.ListenPort))}
		}
		r = &http.Request{URL: u, Host: u.Host, Header: http.Header{}}
	}
	if selector, ok := body["nodeId"].(string); ok {
		resolved, err := server.nodes.Resolve(ctx, selector, false)
		if err != nil {
			return nil, err
		}
		if resolved.Status != 200 {
			return nil, siteError(resolved.Status, "Node selector is unavailable or ambiguous")
		}
		raw, _ := json.Marshal(resolved.Body)
		var result struct {
			Node struct {
				ID string `json:"nodeId"`
			} `json:"node"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Node.ID == "" {
			return nil, fmt.Errorf("invalid Node resolution")
		}
		body["nodeId"] = result.Node.ID
	}
	if action == "create" && server.previewOrigin(r) == "" {
		return nil, siteError(409, "current Mira ingress has no configured site domain")
	}
	return server.managePortSite(ctx, actor, action, key, body, r)
}

func (server *Server) invalidateSiteProxy(id string) {
	server.siteProxies.Lock()
	entry := server.siteProxies.items[id]
	delete(server.siteProxies.items, id)
	server.siteProxies.Unlock()
	if entry != nil {
		entry.transport.CloseIdleConnections()
	}
}

func (server *Server) portSiteProxy(s *portSite) *httputil.ReverseProxy {
	server.siteProxies.Lock()
	defer server.siteProxies.Unlock()
	if entry := server.siteProxies.items[s.ID]; entry != nil && entry.revision == s.Revision {
		entry.used = time.Now()
		return entry.proxy
	}
	if server.siteProxies.items == nil {
		server.siteProxies.items = map[string]*siteProxy{}
	}
	if old := server.siteProxies.items[s.ID]; old != nil {
		old.transport.CloseIdleConnections()
	}
	if len(server.siteProxies.items) >= 64 {
		var key string
		var oldest time.Time
		for id, entry := range server.siteProxies.items {
			if key == "" || entry.used.Before(oldest) {
				key = id
				oldest = entry.used
			}
		}
		server.siteProxies.items[key].transport.CloseIdleConnections()
		delete(server.siteProxies.items, key)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		server.siteLifecycle.RLock()
		defer server.siteLifecycle.RUnlock()
		current, err := server.getPortSite(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if current == nil || !current.Enabled || !current.approved || current.Revision != s.Revision {
			return nil, net.ErrClosed
		}
		return server.channel.DialSite(ctx, s.ID, s.NodeID, s.Port)
	}
	tr.MaxIdleConns = 4
	tr.MaxIdleConnsPerHost = 4
	tr.MaxConnsPerHost = 32
	tr.IdleConnTimeout = 30 * time.Second
	tr.ResponseHeaderTimeout = 0
	// A fresh upstream stream per request prevents net/http from replaying an
	// idempotent request after a reused transport loses its Node session. The
	// browser/client connection to Mira may still use keep-alive.
	tr.DisableKeepAlives = true
	tr.DisableCompression = true
	tr.ForceAttemptHTTP2 = false
	proxy := &httputil.ReverseProxy{Transport: tr, FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.Out.URL.Scheme = s.Scheme
			p.Out.URL.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port))
			p.Out.Host = p.In.Host
			p.SetXForwarded()
			if server.config.Foundation.SecureCookies {
				p.Out.Header.Set("X-Forwarded-Proto", "https")
			}
			if server.config.Foundation.TrustProxyHeaders {
				p.Out.Header.Set("X-Forwarded-For", foundation.RequestAddress(p.In, true))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Node service is unavailable", http.StatusBadGateway)
		}}
	server.siteProxies.items[s.ID] = &siteProxy{revision: s.Revision, proxy: proxy, transport: tr, used: time.Now()}
	return proxy
}

func (server *Server) servePortSite(w http.ResponseWriter, r *http.Request, label string) {
	if server.pool == nil || len(label) < 3 {
		http.NotFound(w, r)
		return
	}
	s, err := server.getPortSite(r.Context(), strings.TrimPrefix(label, "p-"))
	if err != nil {
		http.Error(w, "site lookup unavailable", 503)
		return
	}
	if s == nil || s.label() != label || !s.Enabled {
		http.NotFound(w, r)
		return
	}
	if !s.approved || !s.supported || !server.channel.IsConnected(s.NodeID) {
		http.Error(w, "Node is unavailable", 503)
		return
	}
	server.portSiteProxy(s).ServeHTTP(w, r)
}
