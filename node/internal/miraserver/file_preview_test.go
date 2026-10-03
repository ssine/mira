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
