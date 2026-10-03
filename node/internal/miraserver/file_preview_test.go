package miraserver

import (
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/resourcewire"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResourceIdentityPinsWindowsSessionOnly(t *testing.T) {
	for _, identity := range []string{"node", "user", "system"} {
		p := pinResourceIdentity(resourcewire.Request{}, map[string]any{"executionContext": identity, "userSessionId": float64(7)})
		if identity == "node" && p.ExecutionContext != "" || identity != "node" && p.ExecutionContext != identity {
			t.Fatal("incorrect execution identity", p)
		}
		if identity == "user" && (p.UserSessionID == nil || *p.UserSessionID != 7) || identity != "user" && p.UserSessionID != nil {
			t.Fatal("incorrect Windows session", p)
		}
	}
}

func TestPreviewHostNeverRoutesAdminOrWebAssets(t *testing.T) {
	s := &Server{config: Config{Foundation: foundation.Config{PreviewDomain: "preview.example.test"}}}
	for _, host := range []string{"id.preview.example.test", "id.preview.example.test:443", "a.b.preview.example.test", "preview.example.test"} {
		for _, path := range []string{"/", "/app.js", "/v1/admin/session", "/v1/nodes/a/connect"} {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
			s.ServeHTTP(w, r)
			if w.Code != 410 {
				t.Fatal(host, path, w.Code)
			}
		}
	}
	if _, ok := s.previewHost("mira.example.test"); ok {
		t.Fatal("console host captured")
	}
}
func TestSitePaths(t *testing.T) {
	for _, v := range []string{"../secret", "assets/../../secret", "/absolute", "a\\b", "a\x00b", "a/./b", "a//b", "C:/Users"} {
		if safeSitePath(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"index.html", "assets/a.js", "子目录/页面.html"} {
		if !safeSitePath(v) {
			t.Fatal(v)
		}
	}
	if joinNative(`C:\work\site`, "pages/index.html") != `C:\work\site\pages\index.html` {
		t.Fatal("native path")
	}
}

func TestExpiredSiteCannotServeAssets(t *testing.T) {
	s := &Server{config: Config{Foundation: foundation.Config{PreviewDomain: "preview.example.test"}}, previews: previewStore{sessions: map[string]*previewSession{"expired": {ID: "expired", Expires: time.Now().Add(-time.Second)}}}}
	r := httptest.NewRequest("GET", "https://expired.preview.example.test/index.html", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 410 {
		t.Fatal(w.Code)
	}
}

func TestPreviewIngressSelectionDoesNotInheritConsolePort(t *testing.T) {
	s := &Server{config: Config{Foundation: foundation.Config{SecureCookies: true, PreviewDomain: "legacy.example.test", PreviewIngresses: []foundation.PreviewIngress{
		{ConsoleOrigin: "https://console.example.test", PreviewOrigin: "https://preview.example.test:9443"},
		{ConsoleOrigin: "https://direct.example.test:24443", PreviewOrigin: "https://preview.direct.example.test:34443"},
	}}}}
	for _, test := range []struct{ host, want string }{
		{"console.example.test", "https://preview.example.test:9443"},
		{"CONSOLE.example.test:443", "https://preview.example.test:9443"},
		{"direct.example.test:24443", "https://preview.direct.example.test:34443"},
		{"direct.example.test", ""},
		{"unknown.example.test:24443", ""},
	} {
		r := httptest.NewRequest("POST", "https://"+test.host+"/v1/file-previews", nil)
		r.Header.Set("X-Forwarded-Host", "console.example.test")
		if got := s.previewOrigin(r); got != test.want {
			t.Fatalf("%s: %s, want %s", test.host, got, test.want)
		}
	}
	s.config.Foundation.PreviewIngresses = nil
	r := httptest.NewRequest("POST", "https://console.example.test:8443/v1/file-previews", nil)
	if got := s.previewOrigin(r); got != "https://legacy.example.test:8443" {
		t.Fatal("legacy configuration changed", got)
	}
}

func TestAllPreviewSuffixesReserveAdminRoutes(t *testing.T) {
	s := &Server{config: Config{Foundation: foundation.Config{PreviewIngresses: []foundation.PreviewIngress{
		{PreviewOrigin: "https://preview.example.test"},
		{PreviewOrigin: "https://direct.preview.example.test:24443"},
	}}}}
	for _, host := range []string{"id.preview.example.test", "id.direct.preview.example.test:24443", "direct.preview.example.test", "a.b.direct.preview.example.test"} {
		for _, route := range []string{"/app.js", "/v1/admin/session", "/v1/nodes/a/connect"} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "https://"+host+route, nil))
			if w.Code != 410 {
				t.Fatal(host, route, w.Code)
			}
		}
	}
	if id, ok := s.previewHost("id.direct.preview.example.test:24443"); !ok || id != "id" {
		t.Fatal("most specific preview suffix did not win", id, ok)
	}
}

func TestPreviewSessionCannotMoveToAnotherIngress(t *testing.T) {
	s := &Server{config: Config{Foundation: foundation.Config{SecureCookies: true, PreviewIngresses: []foundation.PreviewIngress{
		{PreviewOrigin: "https://preview.example.test"},
		{PreviewOrigin: "https://preview.direct.example.test:24443"},
	}}}, previews: previewStore{sessions: map[string]*previewSession{"active": {ID: "active", Origin: "https://active.preview.example.test", Expires: time.Now().Add(time.Minute)}}}}
	for _, host := range []string{"active.preview.direct.example.test:24443", "active.preview.example.test:24443"} {
		for _, route := range []string{"/__mira_preview/bootstrap", "/__mira_preview/claim", "/index.html"} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("POST", "https://"+host+route, nil))
			if w.Code != 410 {
				t.Fatal("session accepted on another origin", host, route, w.Code)
			}
		}
	}
	if s.previews.sessions["active"] == nil {
		t.Fatal("wrong ingress invalidated the owner's session")
	}
}
