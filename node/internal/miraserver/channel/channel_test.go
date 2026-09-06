package channel

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

const (
	testNodeA = "123e4567-e89b-12d3-a456-426614174000"
	testNodeB = "223e4567-e89b-12d3-a456-426614174000"
	testCredA = "323e4567-e89b-12d3-a456-426614174000"
	testCredB = "423e4567-e89b-12d3-a456-426614174000"
)

type fakeDB struct {
	mu                sync.Mutex
	queries           []string
	insertThreadStart bool
}

func (db *fakeDB) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	db.mu.Lock()
	db.queries = append(db.queries, query)
	db.mu.Unlock()
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (db *fakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}
func (db *fakeDB) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	db.mu.Lock()
	db.queries = append(db.queries, query)
	insert := db.insertThreadStart
	db.mu.Unlock()
	if insert && strings.Contains(query, "INSERT INTO mira_appserver_thread_start_requests") {
		return staticRow{values: []any{"523e4567-e89b-12d3-a456-426614174000"}}
	}
	return staticRow{err: pgx.ErrNoRows}
}

type staticRow struct {
	values []any
	err    error
}

func (row staticRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return errors.New("scan count mismatch")
	}
	for index, destination := range destinations {
		reflect.ValueOf(destination).Elem().Set(reflect.ValueOf(row.values[index]))
	}
	return nil
}

type fakeRegistry struct {
	values   map[string]nodes.Node
	statuses chan any
}

func (registry *fakeRegistry) Get(_ context.Context, nodeID string, _ bool) (*nodes.Node, error) {
	value, ok := registry.values[nodeID]
	if !ok {
		return nil, nil
	}
	return &value, nil
}
func (registry *fakeRegistry) List(context.Context, bool) ([]nodes.Node, error) {
	result := make([]nodes.Node, 0, len(registry.values))
	for _, value := range registry.values {
		result = append(result, value)
	}
	return result, nil
}
func (registry *fakeRegistry) Resolve(_ context.Context, selector string, _ bool) (nodes.Result, error) {
	value, ok := registry.values[selector]
	if !ok {
		return nodes.Result{Status: 404, Body: map[string]any{"error": "not found", "code": "not_found"}}, nil
	}
	return nodes.Result{Status: 200, Body: map[string]any{"node": value}}, nil
}
func (registry *fakeRegistry) SetChannelStatus(_ context.Context, _ string, status any) error {
	if registry.statuses != nil {
		select {
		case registry.statuses <- status:
		default:
		}
	}
	return nil
}

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, _ *http.Request, _ string) (*foundation.Principal, error) {
	return nil, nil
}
func (fakeAuth) AuthenticateNodeToken(_ context.Context, token, clientType string) (*foundation.Principal, error) {
	switch token {
	case "token-a":
		return &foundation.Principal{Kind: "node", NodeID: testNodeA, CredentialID: testCredA, SubjectID: testCredA, ClientType: clientType}, nil
	case "token-b":
		return &foundation.Principal{Kind: "node", NodeID: testNodeB, CredentialID: testCredB, SubjectID: testCredB, ClientType: clientType}, nil
	default:
		return nil, nil
	}
}
func (fakeAuth) Permits(principal *foundation.Principal, actorType string) bool {
	return principal != nil && !principal.Revoked && (actorType == "trusted" || actorType == principal.Kind)
}

func TestProtocolTokenNeverUsesQueryCredential(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://mira.test/connect?token=token-a", nil)
	request.Header.Set("Sec-WebSocket-Protocol", "mira-node-v1")
	if _, ok := ProtocolToken(request); ok {
		t.Fatal("query credential was accepted")
	}
	request.Header.Set("Sec-WebSocket-Protocol", "mira-node-v1, auth."+base64.RawURLEncoding.EncodeToString([]byte("token-a")))
	if token, ok := ProtocolToken(request); !ok || token != "token-a" {
		t.Fatalf("ProtocolToken() = %q, %v", token, ok)
	}
}

func TestNodeUpgradeRejectsQueryCredentialBeforeAuth(t *testing.T) {
	channel := newTestChannel(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/nodes/"+testNodeA+"/connect?credential=token-a", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", "mira-node-v1")
	response := httptest.NewRecorder()
	channel.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestBrowserWebSocketOriginMustMatchPublicRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://mira.example.test/v1/nodes/"+testNodeA+"/app-server", nil)
	request.Host = "mira.example.test"
	request.Header.Set("Origin", "http://attacker.example.test")
	if sameBrowserOrigin(request, false) {
		t.Fatal("accepted a cross-origin browser websocket")
	}
	request.Header.Set("Origin", "http://mira.example.test")
	if !sameBrowserOrigin(request, false) {
		t.Fatal("rejected the direct same-origin browser websocket")
	}
	request.Header.Set("Origin", "https://mira.example.test")
	request.Header.Set("X-Forwarded-Proto", "https")
	if !sameBrowserOrigin(request, true) {
		t.Fatal("rejected the proxied same-origin browser websocket")
	}
	request.Header.Del("Origin")
	if sameBrowserOrigin(request, true) {
		t.Fatal("accepted a cookie websocket without Origin")
	}
}

func TestHandlesOnlyOwnedWebSocketPaths(t *testing.T) {
	channel := newTestChannel(t)
	for _, path := range []string{
		"/v1/nodes/" + testNodeA + "/connect",
		"/v1/nodes/" + testNodeA + "/app-server",
		"/v1/ssh/sessions/623e4567-e89b-12d3-a456-426614174000/source",
	} {
		if !channel.Handles(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Fatalf("Handles(%q) = false", path)
		}
	}
	for _, path := range []string{"/healthz", "/v1/nodes/" + testNodeA, "/v1/ssh/sessions/not-a-uuid/source"} {
		if channel.Handles(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Fatalf("Handles(%q) = true", path)
		}
	}
}

func TestCanonicalJSONMatchesJavaScriptOrdering(t *testing.T) {
	value := map[string]any{"b": float64(1), "a": map[string]any{"\ue000": float64(1), "𝄞": float64(2), "html": "<tag>"}}
	canonical, err := canonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"a":{"html":"<tag>","𝄞":2,"":1},"b":1}`
	if canonical != expected {
		t.Fatalf("canonical JSON = %s, want %s", canonical, expected)
	}
	digest, err := requestDigest(value)
	if err != nil || digest != "b0f188cb37dd4b31169242fc61fb141ad06f226798f77929f7a79868e787934b" {
		t.Fatalf("JavaScript-compatible digest = %q, %v", digest, err)
	}
	if _, err := decodeObject([]byte(`{} {}`)); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
}

func TestDeveloperAndDynamicToolInjection(t *testing.T) {
	target := &nodes.Node{Platform: "linux", ReportedAppServer: map[string]any{"miraCliPath": "/opt/mira/mira"}}
	old := "before\n\n" + miraCLIInstructionsBegin + "\nold\n" + miraCLIInstructionsEnd + "\nafter"
	merged, ok := mergeDeveloperInstructions(old, miraCLIInstructions(target))
	if !ok || strings.Count(merged, miraCLIInstructionsBegin) != 1 || !strings.Contains(merged, "before\n\nafter") || !strings.Contains(merged, "/opt/mira/mira") {
		t.Fatalf("unexpected merged instructions: %q", merged)
	}
	tools := mergeDynamicTools([]any{map[string]any{"name": DynamicToolNamespace}, map[string]any{"name": "kept"}})
	if len(tools) != 2 || stringValue(tools[0].(map[string]any)["name"]) != "kept" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
}

func TestCapabilityValidationBounds(t *testing.T) {
	valid := map[string]any{"action": "resize", "sessionId": "s", "rows": json.Number("24"), "cols": json.Number("80")}
	if _, err := validateCapabilityParams("pty", valid); err != nil {
		t.Fatal(err)
	}
	if _, err := validateCapabilityParams("pty", map[string]any{"action": "resize", "rows": 24}); err == nil {
		t.Fatal("resize without cols accepted")
	}
	if _, err := validateCapabilityParams("file", map[string]any{"action": "read", "path": "a\x00b"}); err == nil {
		t.Fatal("NUL path accepted")
	}
}

func TestValidSSHKey(t *testing.T) {
	raw := make([]byte, 51)
	binary.BigEndian.PutUint32(raw[:4], 11)
	copy(raw[4:15], "ssh-ed25519")
	binary.BigEndian.PutUint32(raw[15:19], 32)
	for index := 19; index < len(raw); index++ {
		raw[index] = byte(index)
	}
	key := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(raw)
	if !ValidSSHKey(key) {
		t.Fatal("valid canonical key rejected")
	}
	if ValidSSHKey(key + " comment") {
		t.Fatal("comment-bearing key accepted")
	}
}

func TestInvokeTimeoutAndDisconnectCleanup(t *testing.T) {
	channel := newTestChannel(t)
	server := httptest.NewServer(channel)
	defer server.Close()
	defer channel.Close()
	node := dial(t, server.URL, "/v1/nodes/"+testNodeA+"/connect", "mira-node-v1", "token-a")
	defer node.Close()
	waitConnected(t, channel, testNodeA)
	result := make(chan error, 1)
	go func() {
		_, err := channel.Invoke(context.Background(), testNodeA, "status", map[string]any{}, 30*time.Millisecond)
		result <- err
	}()
	if _, _, err := node.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; errorCode(err) != "capability_timeout" {
		t.Fatalf("timeout error = %#v", err)
	}
	channel.mu.Lock()
	count := len(channel.pending)
	channel.mu.Unlock()
	if count != 0 {
		t.Fatalf("pending after timeout = %d", count)
	}

	go func() {
		_, err := channel.Invoke(context.Background(), testNodeA, "status", map[string]any{}, time.Second)
		result <- err
	}()
	if _, _, err := node.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	_ = node.Close()
	if err := <-result; errorCode(err) != "node_offline" {
		t.Fatalf("disconnect error = %#v", err)
	}
}

func TestAppServerProxyInjectsPolicyAndTools(t *testing.T) {
	channel := newTestChannel(t)
	server := httptest.NewServer(channel)
	defer server.Close()
	defer channel.Close()
	node := dial(t, server.URL, "/v1/nodes/"+testNodeA+"/connect", "mira-node-v1", "token-a")
	defer node.Close()
	waitConnected(t, channel, testNodeA)
	client := dial(t, server.URL, "/v1/nodes/"+testNodeA+"/app-server?storeId=personal", "mira-client-v1", "token-b")
	defer client.Close()
	_, openPayload, err := node.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	open, _ := decodeObject(openPayload)
	if open["type"] != "appserver.open" {
		t.Fatalf("open = %#v", open)
	}
	if err := client.WriteJSON(map[string]any{"id": 1, "method": "thread/start", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	_, forwardedPayload, err := node.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	forwarded, _ := decodeObject(forwardedPayload)
	appPayload, _ := forwarded["payload"].(string)
	rpc, err := decodeObject([]byte(appPayload))
	if err != nil {
		t.Fatal(err)
	}
	params := rpc["params"].(map[string]any)
	if params["approvalPolicy"] != "never" || params["sandbox"] != "danger-full-access" {
		t.Fatalf("policy not injected: %#v", params)
	}
	tools, ok := params["dynamicTools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not injected: %#v", params["dynamicTools"])
	}
}

func TestSSHSourceTargetBinaryRelay(t *testing.T) {
	channel := newTestChannel(t)
	server := httptest.NewServer(channel)
	defer server.Close()
	defer channel.Close()
	sessionID := "623e4567-e89b-12d3-a456-426614174000"
	session := &sshSession{id: sessionID, sourceNodeID: testNodeA, targetNodeID: testNodeB,
		sourceCredentialID: testCredA, targetCredentialID: testCredB,
		principal: &foundation.Principal{Kind: "node", NodeID: testNodeA, CredentialID: testCredA},
		claimed:   map[string]bool{}, sockets: map[string]*socket{}, ready: make(chan struct{}), done: make(chan struct{})}
	session.timer = time.AfterFunc(time.Minute, func() {})
	channel.ssh.mu.Lock()
	channel.ssh.sessions[sessionID] = session
	channel.ssh.mu.Unlock()
	source := dial(t, server.URL, "/v1/ssh/sessions/"+sessionID+"/source", "mira-ssh-v1", "token-a")
	defer source.Close()
	target := dial(t, server.URL, "/v1/ssh/sessions/"+sessionID+"/target", "mira-ssh-v1", "token-b")
	defer target.Close()
	payload := []byte{0, 1, 2, 3, 255}
	if err := source.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	kind, received, err := target.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.BinaryMessage || !reflect.DeepEqual(received, payload) {
		t.Fatalf("relay = %d %v", kind, received)
	}
	if err := target.WriteMessage(websocket.TextMessage, []byte("not binary")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for channel.ssh.SessionCount(testNodeA) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if channel.ssh.SessionCount(testNodeA) != 0 {
		t.Fatal("text frame did not close SSH session")
	}
}

func TestThreadStartReservationUsesDurableLedger(t *testing.T) {
	db := &fakeDB{insertThreadStart: true}
	channel := newTestChannelWithDB(t, db)
	proxy := &proxy{storeID: "personal", actorKey: "node:" + testCredA, targetNodeID: testNodeA,
		idempotentThreadStarts: map[string]string{}, boundThreadIDs: map[string]bool{}, ephemeralThreadIDs: map[string]bool{}}
	message := map[string]any{"id": json.Number("7"), "method": "thread/start", "params": map[string]any{
		"miraRequestId": "523e4567-e89b-12d3-a456-426614174000", "cwd": "/work",
	}}
	handled, err := channel.reserveThreadStart(context.Background(), proxy, message)
	if err != nil || handled {
		t.Fatalf("reserve = %v, %v", handled, err)
	}
	if _, exists := message["params"].(map[string]any)["miraRequestId"]; exists {
		t.Fatal("Mira-only request ID forwarded")
	}
	if len(proxy.idempotentThreadStarts) != 1 || len(channel.threadStarts) != 1 {
		t.Fatal("reservation was not tracked")
	}
	digest, err := requestDigest(message["params"])
	if err != nil || digest != "8d7a52b127ad55322882587715fc1eac7c6087d4d6f7b1be6a7bb329f1572516" {
		t.Fatalf("released thread/start digest = %q, %v", digest, err)
	}
}

func newTestChannel(t *testing.T) *Channel { return newTestChannelWithDB(t, &fakeDB{}) }
func newTestChannelWithDB(t *testing.T, db *fakeDB) *Channel {
	t.Helper()
	registry := &fakeRegistry{values: map[string]nodes.Node{
		testNodeA: {NodeID: testNodeA, Platform: "linux", Capabilities: map[string]any{"appServer": true, "files": true}, DesiredAppServer: map[string]any{}, ReportedAppServer: map[string]any{}, MachineStatus: map[string]any{}},
		testNodeB: {NodeID: testNodeB, Platform: "linux", Capabilities: map[string]any{"appServer": true}, DesiredAppServer: map[string]any{}, ReportedAppServer: map[string]any{}, MachineStatus: map[string]any{}},
	}}
	value, err := New(Options{Database: db, Nodes: registry, Auth: fakeAuth{}, Audit: func(context.Context, foundation.AuditEvent) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func dial(t *testing.T, serverURL, path, protocol, token string) *websocket.Conn {
	t.Helper()
	parsed, _ := url.Parse(serverURL)
	scheme := "ws"
	if parsed.Scheme == "https" {
		scheme = "wss"
	}
	endpoint := scheme + "://" + parsed.Host + path
	protocols := []string{protocol}
	if token != "" {
		protocols = append(protocols, "auth."+base64.RawURLEncoding.EncodeToString([]byte(token)))
	}
	dialer := websocket.Dialer{Subprotocols: protocols}
	connection, response, err := dialer.Dial(endpoint, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial %s: %v (status %d)", path, err, response.StatusCode)
		}
		t.Fatalf("dial %s: %v", path, err)
	}
	if connection.Subprotocol() != protocol {
		t.Fatalf("subprotocol = %q", connection.Subprotocol())
	}
	return connection
}

func waitConnected(t *testing.T, channel *Channel, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !channel.IsConnected(nodeID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !channel.IsConnected(nodeID) {
		t.Fatal("Node did not connect")
	}
}
