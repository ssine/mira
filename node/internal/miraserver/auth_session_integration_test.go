package miraserver

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestAdminSessionRenewal(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	const password = "session-renewal-test-password"
	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			if _, err := foundation.SetAdminPassword(ctx, pool, "admin", password); err != nil {
				t.Fatal(err)
			}
			auth := foundation.NewAuthService(pool, foundation.AuthOptions{SecureCookies: secure})
			server := &Server{pool: pool, auth: auth, config: Config{Logger: log.Default()}}
			var cookie *http.Cookie
			var csrf string
			call := func(method, path, body string, withCSRF bool) (*httptest.ResponseRecorder, map[string]any) {
				t.Helper()
				request := httptest.NewRequest(method, name+"://mira.test"+path, strings.NewReader(body))
				if cookie != nil {
					request.AddCookie(cookie)
				}
				if withCSRF {
					request.Header.Set("X-Mira-CSRF", csrf)
				}
				response := httptest.NewRecorder()
				if err := server.route(ctx, response, request); err != nil {
					server.writeRouteError(response, err)
				}
				var result map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				return response, result
			}
			checkCookie := func(response *httptest.ResponseRecorder) *http.Cookie {
				t.Helper()
				cookies := response.Result().Cookies()
				if len(cookies) != 1 {
					t.Fatalf("got %d cookies, want one", len(cookies))
				}
				got := cookies[0]
				wantName := "mira_session"
				if secure {
					wantName = "__Host-mira_session"
				}
				if got.Name != wantName || got.MaxAge != 30*24*60*60 || got.Secure != secure ||
					!got.HttpOnly || got.SameSite != http.SameSiteStrictMode || got.Path != "/" || got.Domain != "" {
					t.Fatal("session cookie lifetime or security attributes changed")
				}
				return got
			}
			login := func() {
				t.Helper()
				response, result := call(http.MethodPost, "/v1/admin/login", `{"username":"admin","password":"`+password+`"}`, false)
				if response.Code != 200 {
					t.Fatalf("login returned %d", response.Code)
				}
				cookie = checkCookie(response)
				csrf = result["csrfToken"].(string)
				expires, err := time.Parse(time.RFC3339Nano, result["expiresAt"].(string))
				if err != nil || time.Until(expires) < 30*24*time.Hour-time.Minute || time.Until(expires) > 30*24*time.Hour+time.Minute {
					t.Fatal("login did not issue a 30-day session")
				}
			}
			setExpiry := func(expires time.Time) {
				t.Helper()
				if _, err := pool.Exec(ctx, `UPDATE mira_admin_sessions SET expires_at=$2 WHERE token_hash=$1`, foundation.TokenHash(cookie.Value), expires); err != nil {
					t.Fatal(err)
				}
			}
			storedExpiry := func() time.Time {
				t.Helper()
				var expires time.Time
				if err := pool.QueryRow(ctx, `SELECT expires_at FROM mira_admin_sessions WHERE token_hash=$1`, foundation.TokenHash(cookie.Value)).Scan(&expires); err != nil {
					t.Fatal(err)
				}
				return expires
			}
			login()
			// An unexpired legacy 12-hour session renews on ordinary API use,
			// not just on the explicit session endpoint. Reuse its old cookie.
			for _, path := range []string{"/v1/dynamic-tools", "/v1/admin/session"} {
				setExpiry(time.Now().Add(time.Hour))
				response, result := call(http.MethodGet, path, "", false)
				if response.Code != 200 || time.Until(storedExpiry()) < 30*24*time.Hour-time.Minute {
					t.Fatalf("%s did not renew the stored session", path)
				}
				if checkCookie(response).Value != cookie.Value {
					t.Fatal("renewal rotated the token and could invalidate another tab")
				}
				if path == "/v1/admin/session" {
					expires, err := time.Parse(time.RFC3339Nano, result["expiresAt"].(string))
					if err != nil || !expires.Equal(storedExpiry()) || result["csrfToken"] != csrf {
						t.Fatal("session response does not match renewed expiry or stable CSRF")
					}
				}
			}
			// A server restart preserves the login and its renewal behavior.
			server.auth = foundation.NewAuthService(pool, foundation.AuthOptions{SecureCookies: secure})
			if _, err := server.auth.Initialize(ctx); err != nil {
				t.Fatal(err)
			}
			if response, _ := call(http.MethodGet, "/v1/admin/session", "", false); response.Code != 200 {
				t.Fatal("server restart invalidated an active session")
			}
			setExpiry(time.Now().Add(time.Hour))
			before := storedExpiry()
			for _, request := range []struct{ method, path, code string }{
				{http.MethodPost, "/v1/admin/logout", "invalid_csrf"},
				{http.MethodGet, "/v1/nodes/00000000-0000-4000-8000-000000000001/ssh/keys", "permission_denied"},
			} {
				response, result := call(request.method, request.path, "", false)
				if response.Code != 403 || result["code"] != request.code || len(response.Result().Cookies()) != 0 || !storedExpiry().Equal(before) {
					t.Fatal("a rejected request renewed or cleared the session")
				}
			}
			response, _ := call(http.MethodPost, "/v1/admin/logout", "{}", true)
			cleared := response.Result().Cookies()
			if response.Code != 200 || len(cleared) != 1 || cleared[0].MaxAge != -1 || cleared[0].Value != "" {
				t.Fatal("logout must only send the clearing cookie")
			}
			response, result := call(http.MethodGet, "/v1/admin/session", "", false)
			if response.Code != 401 || result["code"] != "authentication_required" || len(response.Result().Cookies()) != 0 {
				t.Fatal("revoked session was accepted or renewed")
			}
			login()
			setExpiry(time.Now().Add(-time.Minute))
			response, _ = call(http.MethodGet, "/v1/admin/session", "", false)
			if response.Code != 401 || len(response.Result().Cookies()) != 0 || storedExpiry().After(time.Now()) {
				t.Fatal("expired session was revived")
			}
			login()
			if _, err := foundation.SetAdminPassword(ctx, pool, "admin", password); err != nil {
				t.Fatal(err)
			}
			response, _ = call(http.MethodGet, "/v1/admin/session", "", false)
			if response.Code != 401 || len(response.Result().Cookies()) != 0 {
				t.Fatal("password reset did not revoke the renewed session")
			}
		})
	}
}
