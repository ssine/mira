package miraserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/resourcewire"
)

type previewSession struct {
	Epoch        any `json:"-"`
	ID           string
	Owner        string
	NodeID       string
	Resource     resourcewire.Request
	ControlURL   string
	Entry        string
	GrantHash    string
	CookieHash   string
	Expires      time.Time
	GrantExpires time.Time
}
type previewStore struct {
	sync.Mutex
	sessions map[string]*previewSession
}

func previewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func resourceFromQuery(r *http.Request) (string, resourcewire.Request, error) {
	q := r.URL.Query()
	p := resourcewire.Request{Path: q.Get("path"), Archive: q.Get("archive") == "true", ArchiveVersion: q.Get("archiveVersion"), ArchiveEntry: q.Get("archiveEntry"), ExecutionContext: q.Get("executionContext")}
	if v := q.Get("entryId"); v != "" {
		id, e := strconv.Atoi(v)
		if e != nil || id < 0 {
			return "", p, fmt.Errorf("invalid entry ID")
		}
		p.EntryID = &id
	}
	if v := q.Get("userSessionId"); v != "" {
		id, e := strconv.ParseUint(v, 10, 32)
		if e != nil {
			return "", p, e
		}
		sid := uint32(id)
		p.UserSessionID = &sid
	}
	return q.Get("nodeId"), p, nil
}
func resourceParams(p resourcewire.Request, action string) map[string]any {
	b, _ := json.Marshal(p)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	delete(m, "method")
	delete(m, "headers")
	delete(m, "root")
	m["action"] = action
	return m
}
func pinResourceIdentity(p resourcewire.Request, stat map[string]any) resourcewire.Request {
	if p.ExecutionContext == "" {
		if resolved, _ := stat["executionContext"].(string); resolved == "user" || resolved == "system" {
			p.ExecutionContext = resolved
		}
	}
	if p.UserSessionID == nil && p.ExecutionContext == "user" {
		if number, ok := stat["userSessionId"].(float64); ok && number >= 0 && number <= 4294967295 {
			sid := uint32(number)
			p.UserSessionID = &sid
		}
	}
	return p
}
func (server *Server) checkResource(r *http.Request, actor *foundation.Principal, nodeID string, p resourcewire.Request) (map[string]any, error) {
	n, e := server.nodes.Get(r.Context(), nodeID, false)
	if e != nil {
		return nil, e
	}
	if n == nil {
		return nil, &foundation.HTTPError{Status: 404, Message: "Node unavailable"}
	}
	if n.Capabilities["filePreviewV1"] != true {
		return nil, &foundation.HTTPError{Status: 409, Message: "请先升级此 Node，以支持文件预览和流式读取", Code: "preview_unavailable"}
	}
	result, e := server.channel.Capabilities().Invoke(r.Context(), actor, nodeID, "file", resourceParams(p, "stat"), channel.InvokeContext{Request: r})
	if e != nil {
		return nil, e
	}
	stat, _ := result.(map[string]any)
	return stat, nil
}
func (server *Server) fileRoutes(w http.ResponseWriter, r *http.Request) error {
	actor, e := server.authorize(r.Context(), w, r, "admin", authOptions{CSRF: r.Method != "GET" && r.Method != "HEAD"})
	if e != nil || actor == nil {
		return e
	}
	switch r.URL.Path {
	case "/v1/files/meta", "/v1/files/content":
		if r.Method != "GET" && r.Method != "HEAD" {
			return foundation.WriteErrorJSON(w, 405, "read-only resource", "method_not_allowed")
		}
		nodeID, p, e := resourceFromQuery(r)
		if e != nil {
			return e
		}
		if r.URL.Path == "/v1/files/meta" {
			stat, err := server.checkResource(r, actor, nodeID, p)
			if err != nil {
				return err
			}
			if r.URL.Query().Get("action") != "list" {
				return writeJSON(w, 200, stat)
			}
			p = pinResourceIdentity(p, stat)
			params := resourceParams(p, "stat")
			if r.URL.Query().Get("action") == "list" {
				params["action"] = "list"
				params["pageSize"] = 250
				cursor, err := strconv.Atoi(r.URL.Query().Get("cursor"))
				if r.URL.Query().Get("cursor") == "" {
					cursor = 0
					err = nil
				}
				if err != nil {
					return err
				}
				params["cursor"] = cursor
			}
			result, e := server.channel.Capabilities().Invoke(r.Context(), actor, nodeID, "file", params, channel.InvokeContext{Request: r})
			if e != nil {
				return e
			}
			return writeJSON(w, 200, result)
		}
		stat, e := server.checkResource(r, actor, nodeID, p)
		if e != nil {
			return e
		}
		p = pinResourceIdentity(p, stat)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'self'")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.URL.Query().Get("download") == "true" {
			name := p.Path
			if p.Archive {
				name = p.ArchiveEntry
			}
			w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(path.Base(strings.ReplaceAll(name, "\\", "/"))))
		}
		err := server.streamResource(w, r, nodeID, p, actor.SessionID, "")
		if err != nil {
			if tracked, ok := w.(*trackingResponseWriter); ok && tracked.written {
				panic(http.ErrAbortHandler)
			}
		}
		return err
	case "/v1/file-previews":
		if r.Method != "POST" {
			return foundation.WriteErrorJSON(w, 405, "POST required", "method_not_allowed")
		}
		domain := server.config.Foundation.PreviewDomain
		if domain == "" {
			return foundation.WriteErrorJSON(w, 409, "尚未配置网站预览域名；请设置 MIRA_NODE_PREVIEW_DOMAIN 并准备通配符 DNS 与证书", "preview_domain_unconfigured")
		}
		var body struct {
			NodeID   string               `json:"nodeId"`
			Root     string               `json:"root"`
			Entry    string               `json:"entry"`
			Resource resourcewire.Request `json:"resource"`
		}
		if e = foundation.ReadJSON(r, &body, 64*1024); e != nil {
			return e
		}
		p := body.Resource
		if p.Archive {
			p.ArchiveEntry = body.Root
			p.EntryID = nil
		} else {
			p.Path = body.Root
		}
		stat, e := server.checkResource(r, actor, body.NodeID, p)
		if e != nil {
			return e
		}
		if stat["type"] != "directory" {
			return foundation.WriteErrorJSON(w, 400, "站点根必须是目录", "invalid_root")
		}
		entry := strings.TrimPrefix(body.Entry, "/")
		if !safeSitePath(entry) || entry == "" {
			return foundation.WriteErrorJSON(w, 400, "invalid site entry", "invalid_entry")
		}
		if p.Archive {
			p.ArchiveVersion, _ = stat["archiveVersion"].(string)
		}
		p = pinResourceIdentity(p, stat)
		p.Root = body.Root
		if p.Archive {
			p.ArchiveEntry = path.Join(body.Root, entry)
		} else {
			p.Path = joinNative(body.Root, entry)
		}
		entryStat, e := server.checkResource(r, actor, body.NodeID, p)
		if e != nil {
			return e
		}
		if entryStat["type"] != "file" {
			return foundation.WriteErrorJSON(w, 400, "入口不是文件", "invalid_entry")
		}
		grant := previewToken()
		id := strings.ToLower(previewToken()[:20])
		id = strings.ReplaceAll(strings.ReplaceAll(id, "_", "a"), "-", "b")
		now := time.Now()
		session := &previewSession{ID: id, Epoch: server.channel.NodeConnectionEpoch(body.NodeID), Owner: actor.SessionID, NodeID: body.NodeID, Resource: p, Entry: "/" + escapeSitePath(entry), GrantHash: foundation.TokenHash(grant), Expires: now.Add(30 * time.Minute), GrantExpires: now.Add(time.Minute)}
		controlQuery := url.Values{"nodeId": {body.NodeID}, "path": {p.Path}, "view": {"site"}, "noauto": {"1"}, "previewId": {id}}
		if p.Archive {
			controlQuery.Set("archive", "true")
			controlQuery.Set("archiveEntry", p.ArchiveEntry)
		}
		if p.ExecutionContext != "" {
			controlQuery.Set("executionContext", p.ExecutionContext)
		}
		if p.UserSessionID != nil {
			controlQuery.Set("userSessionId", strconv.FormatUint(uint64(*p.UserSessionID), 10))
		}
		session.ControlURL = requestOrigin(r, server.config.Foundation.SecureCookies) + "/files.html?" + controlQuery.Encode()

		server.previews.Lock()
		if server.previews.sessions == nil {
			server.previews.sessions = map[string]*previewSession{}
		}
		for id, s := range server.previews.sessions {
			if now.After(s.Expires) {
				delete(server.previews.sessions, id)
			}
		}
		if len(server.previews.sessions) >= 128 {
			server.previews.Unlock()
			return foundation.WriteErrorJSON(w, 429, "预览会话数量达到上限，请关闭旧预览", "preview_busy")
		}
		server.previews.sessions[id] = session
		server.previews.Unlock()
		scheme := "https"
		if !server.config.Foundation.SecureCookies {
			scheme = "http"
		}
		return writeJSON(w, 201, map[string]any{"id": id, "url": scheme + "://" + id + "." + domain + previewPort(r.Host, scheme) + "/__mira_preview/bootstrap#" + grant, "expiresAt": session.Expires})
	default:
		id := strings.TrimPrefix(r.URL.Path, "/v1/file-previews/")
		server.previews.Lock()
		defer server.previews.Unlock()
		s := server.previews.sessions[id]
		if s == nil || s.Owner != actor.SessionID {
			return foundation.WriteErrorJSON(w, 404, "preview missing", "preview_missing")
		}
		if time.Now().After(s.Expires) && r.Method != "DELETE" {
			delete(server.previews.sessions, id)
			return foundation.WriteErrorJSON(w, 410, "preview expired", "preview_expired")
		}
		if r.Method == "GET" {
			return writeJSON(w, 200, map[string]any{"root": s.Resource.Root, "entry": s.Entry, "expiresAt": s.Expires})
		}
		if r.Method == "DELETE" {
			delete(server.previews.sessions, id)
			return writeJSON(w, 200, map[string]any{"closed": true})
		}
		if r.Method == "POST" {
			s.Expires = time.Now().Add(30 * time.Minute)
			return writeJSON(w, 200, map[string]any{"expiresAt": s.Expires})
		}
		return foundation.WriteErrorJSON(w, 405, "method not allowed", "method_not_allowed")
	}
}
func safeSitePath(p string) bool {
	return !strings.ContainsAny(p, "\\\x00:") && p != ".." && !strings.HasPrefix(p, "../") && p == path.Clean(p) && !strings.HasPrefix(p, "/")
}
func escapeSitePath(p string) string {
	parts := strings.Split(p, "/")
	for i, v := range parts {
		parts[i] = url.PathEscape(v)
	}
	return strings.Join(parts, "/")
}
func joinNative(root, relative string) string {
	sep := "/"
	if strings.Contains(root, "\\") {
		sep = "\\"
	}
	return strings.TrimRight(root, "/\\") + sep + strings.ReplaceAll(relative, "/", sep)
}
func (server *Server) previewHost(host string) (string, bool) {
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + server.config.Foundation.PreviewDomain
	if server.config.Foundation.PreviewDomain == "" || (!strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".")) {
		return "", false
	}
	id := strings.TrimSuffix(host, suffix)
	if strings.Contains(id, ".") || id == host {
		return "", true
	}
	return id, true
}
func (server *Server) servePreviewHost(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), display-capture=()")
	server.previews.Lock()
	s := server.previews.sessions[id]
	if s != nil {
		copy := *s
		s = &copy
	}
	server.previews.Unlock()
	if s == nil || time.Now().After(s.Expires) {
		http.Error(w, "预览已过期或已关闭，请从 Mira 对话重新打开。", 410)
		return
	}
	var live bool
	e := server.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM mira_admin_sessions WHERE session_id=$1::uuid AND revoked_at IS NULL AND expires_at>NOW())`, s.Owner).Scan(&live)
	if e != nil || !live || s.Epoch != server.channel.NodeConnectionEpoch(s.NodeID) || !server.channel.IsConnected(s.NodeID) {
		server.previews.Lock()
		delete(server.previews.sessions, id)
		server.previews.Unlock()
		http.Error(w, "预览授权失效或 Node 离线。", 403)
		return
	}
	if r.URL.Path == "/__mira_preview/bootstrap" && r.Method == "GET" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		fmt.Fprint(w, previewBootstrap)
		return
	}
	if r.URL.Path == "/__mira_preview/claim" && r.Method == "POST" {
		if r.Header.Get("Origin") != requestOrigin(r, server.config.Foundation.SecureCookies) {
			http.Error(w, "invalid origin", 403)
			return
		}
		var body struct {
			Grant string `json:"grant"`
		}
		if foundation.ReadJSON(r, &body, 1024) != nil {
			http.Error(w, "invalid grant", 403)
			return
		}
		server.previews.Lock()
		current := server.previews.sessions[id]
		if current == nil || current.GrantHash == "" || time.Now().After(current.GrantExpires) || foundation.TokenHash(body.Grant) != current.GrantHash {
			server.previews.Unlock()
			http.Error(w, "引导授权已失效，请从对话重新打开。", 403)
			return
		}
		cookie := previewToken()
		current.CookieHash = foundation.TokenHash(cookie)
		current.GrantHash = ""
		server.previews.Unlock()
		http.SetCookie(w, &http.Cookie{Name: previewCookieName(server.config.Foundation.SecureCookies), Value: cookie, Path: "/", HttpOnly: true, Secure: server.config.Foundation.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
		_ = writeJSON(w, 200, map[string]any{"entry": s.Entry})
		return
	}
	cookie, e := r.Cookie(previewCookieName(server.config.Foundation.SecureCookies))
	if e != nil || foundation.TokenHash(cookie.Value) != s.CookieHash {
		http.Error(w, "请通过 Mira 对话打开此预览。", 403)
		return
	}
	if r.URL.Path == "/__mira_preview/manage" && r.Method == "GET" {
		http.Redirect(w, r, s.ControlURL, http.StatusSeeOther)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "static preview is read-only", 405)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__mira_preview/") {
		http.NotFound(w, r)
		return
	}
	relative := strings.TrimPrefix(r.URL.Path, "/")
	if relative == "" || strings.HasSuffix(relative, "/") {
		relative += "index.html"
	}
	if !safeSitePath(relative) {
		http.Error(w, "invalid resource path", 400)
		return
	}
	p := s.Resource
	p.DiagnosticURL = "/__mira_preview/manage"
	p.EntryID = nil
	if p.Archive {
		p.ArchiveEntry = path.Join(p.Root, relative)
	} else {
		p.Path = joinNative(p.Root, relative)
	}
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-same-origin allow-forms allow-downloads; default-src 'self' data: blob: https:; script-src 'self' 'unsafe-inline' https:; style-src 'self' 'unsafe-inline' https:; connect-src 'self'; worker-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
	tracked := &trackingResponseWriter{ResponseWriter: w}
	if e = server.streamResource(tracked, r, s.NodeID, p, s.Owner, s.ID); e != nil {
		if tracked.written {
			panic(http.ErrAbortHandler)
		}
		http.Error(w, "站点资源暂时不可用，请刷新或返回 Mira 调整根目录。", 502)
	}

}
func requestOrigin(r *http.Request, secure bool) string {
	scheme := "http"
	if secure {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

const previewBootstrap = `<!doctype html><meta charset="utf-8"><title>Mira 网站预览</title><p id="status">正在打开网站…</p><script>
(async()=>{const grant=location.hash.slice(1);history.replaceState(null,'',location.pathname);try{const r=await fetch('/__mira_preview/claim',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({grant})});if(!r.ok)throw new Error(await r.text());const b=await r.json();location.replace(b.entry)}catch(e){document.getElementById('status').textContent=e.message}})();
</script>`

func previewPort(host, scheme string) string {
	_, port, e := net.SplitHostPort(host)
	if e != nil || scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		return ""
	}
	return ":" + port
}

func previewCookieName(secure bool) string {
	if secure {
		return "__Host-mira_preview"
	}
	return "mira_preview"
}

// Revocation also stops in-flight HTTP bodies, not only subsequent requests.
func (server *Server) streamResource(w http.ResponseWriter, r *http.Request, nodeID string, p resourcewire.Request, owner, id string) error {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				live := false
				err := server.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_admin_sessions WHERE session_id=$1::uuid AND revoked_at IS NULL AND expires_at>NOW())`, owner).Scan(&live)
				if id != "" {
					server.previews.Lock()
					s := server.previews.sessions[id]
					live = live && s != nil && s.Owner == owner && time.Now().Before(s.Expires) && s.Epoch == server.channel.NodeConnectionEpoch(s.NodeID)
					server.previews.Unlock()
				}
				if err != nil || !live {
					cancel()
					return
				}
			}
		}
	}()
	return server.channel.StreamFile(ctx, w, r.WithContext(ctx), nodeID, p)
}
