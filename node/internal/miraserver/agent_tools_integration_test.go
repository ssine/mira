package miraserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ssine/mira/node/internal/agenttools"
	serverchannel "github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

func TestAgentToolsHTTPCompatibilityAndAuthorization(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	auth := foundation.NewAuthService(pool, foundation.AuthOptions{})
	if _, err := auth.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	registry := nodes.New(pool, nodes.Options{})
	broker, err := serverchannel.New(serverchannel.Options{Database: pool, Nodes: registry, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	server := &Server{pool: pool, auth: auth, nodes: registry, channel: broker,
		config: Config{Logger: log.Default(), Foundation: foundation.Config{MaxBodyBytes: foundation.DefaultMaxBodyBytes}}}
	nodeID, _ := randomUUID()
	credentialID, _ := randomUUID()
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash, _ := foundation.NodeSecretHash(secret)
	token := "mira_node_" + credentialID + "_" + secret
	if _, err := pool.Exec(ctx, `INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,approved_at)
	 VALUES($1::uuid,$1::text,'tools-test','linux','amd64','linux','test','{}','[]','approved',NOW())`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO mira_node_credentials(credential_id,node_id,secret_hash) VALUES($1::uuid,$2::uuid,$3)`, credentialID, nodeID, hash); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, bearer, cookie, csrf string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		request.Header.Set("Cookie", cookie)
		request.Header.Set("X-Mira-CSRF", csrf)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var result map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return response.Code, result
	}
	for _, path := range []string{"/v1/agent-tools", "/v1/dynamic-tools"} {
		if status, _ := call(http.MethodGet, path, "", "", "", ""); status != 401 {
			t.Fatalf("unauthenticated %s returned %d", path, status)
		}
	}
	status, catalog := call(http.MethodGet, "/v1/agent-tools", "", token, "", "")
	if status != 200 || catalog["namespace"] != agenttools.Namespace || len(catalog["tools"].([]any)) != 5 {
		t.Fatalf("catalog: %d %#v", status, catalog)
	}
	_, legacy := call(http.MethodGet, "/v1/dynamic-tools", "", token, "", "")
	wrapped := legacy["dynamicTools"].([]any)[0].(map[string]any)["tools"].([]any)
	for index, raw := range wrapped {
		tool := raw.(map[string]any)
		if tool["type"] != "function" {
			t.Fatal("legacy Codex wrapper changed")
		}
		delete(tool, "type")
		if !reflect.DeepEqual(tool, catalog["tools"].([]any)[index]) {
			t.Fatal("catalogs disagree")
		}
	}
	body := `{"tool":"status","arguments":{"action":"list"}}`
	status, result := call(http.MethodPost, "/v1/agent-tools/call", body, token, "", "")
	if status != 200 || result["isError"] != false {
		t.Fatalf("call: %d %#v", status, result)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)), &decoded); err != nil {
		t.Fatal(err)
	}
	_, legacy = call(http.MethodPost, "/v1/dynamic-tools/call", body, token, "", "")
	// Authentication timestamps can advance between calls; compare identities.
	newNodes, oldNodes := decoded["nodes"].([]any), legacy["result"].(map[string]any)["nodes"].([]any)
	if len(newNodes) != 1 || len(oldNodes) != 1 || newNodes[0].(map[string]any)["nodeId"] != oldNodes[0].(map[string]any)["nodeId"] {
		t.Fatal("legacy and shared tools resolved different Nodes")
	}
	for _, invalid := range []string{`{}`, `{"tool":"status","arguments":{"action":"typo"}}`, `{"tool":"file","arguments":{"action":"read"}}`} {
		if status, _ := call(http.MethodPost, "/v1/agent-tools/call", invalid, token, "", ""); status != 400 {
			t.Fatalf("invalid call returned %d", status)
		}
	}
	if _, err := foundation.SetAdminPassword(ctx, pool, "admin", "agent-tools-test-password"); err != nil {
		t.Fatal(err)
	}
	login, err := auth.Login(ctx, httptest.NewRequest(http.MethodPost, "/", nil), "admin", "agent-tools-test-password")
	if err != nil || login == nil {
		t.Fatalf("login failed: %v", err)
	}
	cookie := strings.SplitN(login.Cookie, ";", 2)[0]
	if status, _ := call(http.MethodPost, "/v1/agent-tools/call", body, "", cookie, ""); status != 403 {
		t.Fatalf("browser mutation without CSRF returned %d", status)
	}
	if status, _ := call(http.MethodPost, "/v1/agent-tools/call", body, "", cookie, login.CSRFToken); status != 200 {
		t.Fatalf("authorized browser call returned %d", status)
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_node_credentials SET revoked_at=NOW() WHERE credential_id=$1::uuid`, credentialID); err != nil {
		t.Fatal(err)
	}
	if status, _ := call(http.MethodPost, "/v1/agent-tools/call", body, token, "", ""); status != 403 {
		t.Fatalf("revoked Node returned %d", status)
	}
}
