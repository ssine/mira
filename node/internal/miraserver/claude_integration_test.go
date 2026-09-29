package miraserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/clauderuntime"
	serverchannel "github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type claudeFixture struct {
	server                                   *Server
	endpoint, nodeID, token, cookie, csrf    string
	t                                        *testing.T
	failMirror, dropNextMirror, replayedLost atomic.Bool
	lostOperation                            atomic.Value
}

func newClaudeFixture(t *testing.T) *claudeFixture {
	t.Helper()
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
	s := &Server{pool: pool, auth: auth, nodes: registry, channel: broker, config: Config{Logger: log.Default(), Foundation: foundation.Config{MaxBodyBytes: foundation.DefaultMaxBodyBytes}}}
	nodeID, _ := randomUUID()
	credentialID, _ := randomUUID()
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash, _ := foundation.NodeSecretHash(secret)
	token := "mira_node_" + credentialID + "_" + secret
	if _, err = pool.Exec(ctx, `INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,approved_at,last_seen_at) VALUES($1::uuid,$1::text,'claude-test','linux','amd64','linux','test','{"claudeRuntimeV1":true,"claudeAccountsV1":true,"claudeSessionCacheV1":true,"files":true}','[]','approved',NOW(),NOW())`, nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO mira_node_credentials(credential_id,node_id,secret_hash) VALUES($1,$2,$3)`, credentialID, nodeID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err = foundation.SetAdminPassword(ctx, pool, "admin", "claude-fixture-password"); err != nil {
		t.Fatal(err)
	}
	login, err := auth.Login(ctx, httptest.NewRequest("POST", "/", nil), "admin", "claude-fixture-password")
	if err != nil {
		t.Fatal(err)
	}
	f := &claudeFixture{server: s, nodeID: nodeID, token: token, cookie: strings.SplitN(login.Cookie, ";", 2)[0], csrf: login.CSRFToken, t: t}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Optional interactive preview serves current source assets without rerunning
		// native model calls for each layout adjustment. Normal tests use embedded assets.
		if os.Getenv("MIRA_CLAUDE_PREVIEW_FILE") != "" && r.Method == "GET" {
			switch r.URL.Path {
			case "/", "/app.js", "/claude.js", "/styles.css":
				name := r.URL.Path
				if name == "/" {
					name = "/index.html"
				}
				w.Header().Set("Cache-Control", "no-cache")
				http.ServeFile(w, r, filepath.Join("../../../server/public", name[1:]))
				return
			}
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/entries") {
			if f.failMirror.Load() {
				w.WriteHeader(503)
				return
			}
			operation := r.URL.Query().Get("operationId")
			if f.lostOperation.Load() == operation {
				f.replayedLost.Store(true)
			}
			if f.dropNextMirror.CompareAndSwap(true, false) {
				recorder := httptest.NewRecorder()
				s.ServeHTTP(recorder, r)
				if recorder.Code == 200 {
					f.lostOperation.Store(operation)
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
		}
		s.ServeHTTP(w, r)
	}))
	f.endpoint = httpServer.URL
	t.Cleanup(httpServer.Close)
	return f
}
func (f *claudeFixture) request(method, path string, body any, headers map[string]string) (int, []byte) {
	f.t.Helper()
	var data []byte
	switch b := body.(type) {
	case string:
		data = []byte(b)
	case nil:
	default:
		data, _ = json.Marshal(body)
	}
	r, _ := http.NewRequest(method, f.endpoint+path, strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", f.cookie)
	r.Header.Set("X-Mira-CSRF", f.csrf)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return response.StatusCode, raw
}
func (f *claudeFixture) call(method, path string, body any) map[string]any {
	f.t.Helper()
	status, raw := f.request(method, path, body, nil)
	if status != 200 {
		f.t.Fatalf("%s %s: %d %s", method, path, status, raw)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		f.t.Fatal(err)
	}
	return result
}
func uuidClaude() string { id, _ := randomUUID(); return id }
func (f *claudeFixture) reserved() (string, string, map[string]string) {
	f.t.Helper()
	id, turn := uuidClaude(), uuidClaude()
	_, err := f.server.pool.Exec(context.Background(), `INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,active_turn) VALUES($1,$1,$2,'/test',$3)`, id, f.nodeID, turn)
	if err != nil {
		f.t.Fatal(err)
	}
	_, err = f.server.pool.Exec(context.Background(), `INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request) VALUES($1,$2,$3,1,'{}')`, turn, id, f.nodeID)
	if err != nil {
		f.t.Fatal(err)
	}
	return id, turn, map[string]string{"Authorization": "Bearer " + f.token, "X-Mira-Claude-Turn": turn, "X-Mira-Claude-Revision": "1"}
}
func TestClaudeCompletionWithCodexPushSubscription(t *testing.T) {
	f := newClaudeFixture(t)
	f.call("GET", "/v1/push/config", nil)
	f.call("POST", "/v1/push/subscription", testPushSubscription(t))
	id, _, headers := f.reserved()
	route := "/v1/claude/sessions/" + id
	status, raw := f.request("POST", route+"/entries?operationId="+uuidClaude(), "{\"type\":\"user\",\"uuid\":\""+uuidClaude()+"\"}\n", headers)
	if status != 200 {
		t.Fatalf("append: %d %s", status, raw)
	}
	body := map[string]any{"eventId": uuidClaude(), "payload": map[string]any{"type": "mira_completed"}}
	for range 2 {
		status, raw = f.request("POST", route+"/events", body, headers)
		if status != 200 {
			t.Fatalf("completion with push subscription: %d %s", status, raw)
		}
	}
	s := f.call("GET", route, nil)
	if s["activeTurn"] != nil || s["persistence"] != "saved" {
		t.Fatalf("completion did not commit: %#v", s)
	}
	var deliveries int
	if err := f.server.pool.QueryRow(context.Background(), `SELECT count(*) FROM mira_push_deliveries`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("Claude entered the Codex-only notification queue: %d %v", deliveries, err)
	}
}

func TestClaudeNativeStorage(t *testing.T) {
	f := newClaudeFixture(t)
	id, _, headers := f.reserved()
	route := "/v1/claude/sessions/" + id
	operation := uuidClaude()
	entryUUID := uuidClaude()
	batch := `{"type":"user","uuid":"` + entryUUID + `","unknown":{"text":"nul\u0000你好"}}` + "\n" + `{"type":"metadata","future":42}` + "\n"
	for range 2 {
		status, raw := f.request("POST", route+"/entries?operationId="+operation, batch, headers)
		if status != 200 {
			t.Fatalf("append: %d %s", status, raw)
		}
	}
	status, raw := f.request("GET", route+"/entries", nil, headers)
	if status != 200 || string(raw) != batch {
		t.Fatalf("round trip: %d %s", status, raw)
	}
	first := strings.Split(batch, "\n")[0]
	digest := sha256.Sum256([]byte(first))
	prefix := hex.EncodeToString(digest[:])
	for _, check := range []struct{ query, want, start string }{
		{"cache=1&after=0", batch, "0"},
		{"cache=1&after=1&prefix=" + prefix, strings.Split(batch, "\n")[1] + "\n", "1"},
		{"cache=1&after=1&prefix=stale", batch, "0"},
		{"cache=1&after=3", batch, "0"},
	} {
		req, _ := http.NewRequest("GET", f.endpoint+route+"/entries?"+check.query, nil)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || string(data) != check.want || res.Header.Get("X-Mira-Claude-Cache-Version") != "1" || res.Header.Get("X-Mira-Claude-Cache-Start") != check.start || res.Header.Get("X-Mira-Claude-Cache-End") != "2" || len(res.Header.Get("X-Mira-Claude-Cache-Prefix")) != 64 {
			t.Fatalf("cache wire %s: %d %v %s", check.query, res.StatusCode, res.Header, data)
		}
	}
	status, _ = f.request("GET", route+"/entries?cache=1&after=-1", nil, headers)
	if status != 400 {
		t.Fatalf("negative cache cursor: %d", status)
	}
	staleHeaders := map[string]string{"Authorization": headers["Authorization"], "X-Mira-Claude-Turn": headers["X-Mira-Claude-Turn"], "X-Mira-Claude-Revision": "2"}
	status, _ = f.request("GET", route+"/entries?cache=1&after=1&prefix="+prefix, nil, staleHeaders)
	if status != 409 {
		t.Fatalf("stale cache reader: %d", status)
	}
	status, _ = f.request("POST", route+"/entries?operationId="+operation, batch+"{}\n", headers)
	if status != 409 {
		t.Fatalf("changed replay: %d", status)
	}
	status, _ = f.request("POST", route+"/entries?operationId="+operation+"&subpath=subagents/test", batch, headers)
	if status != 409 {
		t.Fatalf("different subpath replay: %d", status)
	}
	status, _ = f.request("POST", route+"/entries?operationId="+uuidClaude()+"&subpath=../escape", batch, headers)
	if status != 400 {
		t.Fatalf("subpath traversal: %d", status)
	}
	status, raw = f.request("POST", route+"/entries?operationId="+uuidClaude()+"&subpath=subagents/test", batch, headers)
	if status != 200 {
		t.Fatalf("child: %d %s", status, raw)
	}
	status, raw = f.request("GET", route+"/subkeys", nil, headers)
	if status != 200 || !strings.Contains(string(raw), "subagents/test") {
		t.Fatalf("subkeys: %d %s", status, raw)
	}
	for _, e := range []any{map[string]any{"type": "system", "subtype": "mirror_error"}, map[string]any{"type": "mira_completed"}} {
		status, raw = f.request("POST", route+"/events", map[string]any{"eventId": uuidClaude(), "payload": e}, headers)
		if status != 200 {
			t.Fatalf("event: %d %s", status, raw)
		}
	}
	nulEventID := uuidClaude()
	eventBody := map[string]any{"eventId": nulEventID, "payload": map[string]any{"type": "future_event", "unknown": "nul\x00value"}}
	status, raw = f.request("POST", route+"/events", eventBody, headers)
	if status != 200 {
		t.Fatalf("NUL event: %d %s", status, raw)
	}
	timeline := f.call("GET", route+"/events?view=transcript", nil)
	if len(timeline["data"].([]any)) != 3 {
		t.Fatal("native event projection discarded a record")
	}
	eventBody["payload"] = map[string]any{"type": "changed"}
	status, _ = f.request("POST", route+"/events", eventBody, headers)
	if status != 409 {
		t.Fatalf("changed event replay: %d", status)
	}
	status, _ = f.request("POST", "/v1/nodes/"+f.nodeID+"/invoke", map[string]any{"capability": "claude", "params": map[string]any{"action": "start"}}, nil)
	if status != 400 {
		t.Fatalf("private runtime control exposed as a device tool: %d", status)
	}
	s := f.call("GET", route, nil)
	if s["persistence"] != "incomplete" || s["activeTurn"] != nil {
		t.Fatalf("degradation lost: %#v", s)
	}
	headers["X-Mira-Claude-Revision"] = "2"
	status, _ = f.request("GET", route+"/entries", nil, headers)
	if status != 409 {
		t.Fatalf("stale owner: %d", status)
	}
	status, _ = f.request("POST", "/v1/claude/sessions", map[string]any{}, map[string]string{"X-Mira-CSRF": ""})
	if status != 403 {
		t.Fatalf("csrf: %d", status)
	}
}

func TestClaudeManagedSDK(t *testing.T) {
	if os.Getenv("MIRA_CLAUDE_SDK_TEST") != "1" {
		t.Skip("set MIRA_CLAUDE_SDK_TEST=1 for real SDK/native subprocess validation")
	}
	f := newClaudeFixture(t)
	ctx := context.Background()
	workspace := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("MIRA_NODE_CLAUDE_CONFIG_DIR", configDir)
	t.Setenv("ANTHROPIC_API_KEY", "disposable-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("ENABLE_TOOL_SEARCH", "false")
	var requests atomic.Int32
	var modelAuth atomic.Value
	var systemPrompt atomic.Value
	var imageSeen atomic.Bool
	var resumed atomic.Bool
	var hold atomic.Bool
	var ask atomic.Bool
	var child atomic.Bool
	var background, backgroundPhase, backgroundDone atomic.Bool
	backgroundStarted := make(chan struct{}, 1)
	backgroundRelease := make(chan struct{})
	var releaseBackground sync.Once
	defer releaseBackground.Do(func() { close(backgroundRelease) })
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{}`)
			return
		}
		requests.Add(1)
		modelAuth.Store(r.Header.Get("x-api-key"))
		if system, _ := json.Marshal(b["system"]); strings.Contains(string(system), "Use home_nodes MCP tools") {
			systemPrompt.Store(string(system))
		}
		bytes, _ := json.Marshal(b["messages"])
		if strings.Contains(string(bytes), `"type":"image"`) {
			imageSeen.Store(true)
		}
		if strings.Contains(string(bytes), "CLAUDE_MIRA_SAVED_MARKER") {
			resumed.Store(true)
		}
		if hold.Load() {
			<-r.Context().Done()
			return
		}
		block := map[string]any{"type": "text", "text": "CLAUDE_MIRA_SAVED_MARKER"}
		stop := "end_turn"
		if !strings.Contains(string(bytes), "tool_result") {
			block = map[string]any{"type": "tool_use", "id": "tool_" + uuidClaude(), "name": "mcp__home_nodes__status", "input": map[string]any{"action": "list"}}
			stop = "tool_use"
		}
		if ask.CompareAndSwap(true, false) {
			block = map[string]any{"type": "tool_use", "id": "tool_" + uuidClaude(), "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"header": "Validation", "question": "Choose one option", "multiSelect": false, "options": []any{map[string]any{"label": "First", "description": "First choice"}, map[string]any{"label": "Second", "description": "Second choice"}}}}}}
			stop = "tool_use"
		}
		if child.CompareAndSwap(true, false) {
			block = map[string]any{"type": "tool_use", "id": "tool_" + uuidClaude(), "name": "Agent", "input": map[string]any{"description": "Native child validation", "prompt": "Reply CHILD_NATIVE_TEST", "subagent_type": "general-purpose"}}
			stop = "tool_use"
		}
		if strings.Contains(string(bytes), "CHILD_NATIVE_TEST") && !strings.Contains(string(bytes), "CLAUDE_MIRA_SAVED_MARKER") {
			block = map[string]any{"type": "text", "text": "CHILD_NATIVE_TEST"}
			stop = "end_turn"
		}
		if backgroundPhase.Load() {
			isChild := false
			for _, raw := range b["messages"].([]any) {
				message := raw.(map[string]any)
				if message["role"] != "user" {
					continue
				}
				switch content := message["content"].(type) {
				case string:
					isChild = isChild || strings.Contains(content, "BACKGROUND_CHILD_REQUEST")
				case []any:
					for _, raw := range content {
						entry := raw.(map[string]any)
						if text, _ := entry["text"].(string); entry["type"] == "text" && strings.Contains(text, "BACKGROUND_CHILD_REQUEST") {
							isChild = true
						}
					}
				}
			}
			text := "BACKGROUND_PARENT_WAIT"
			if isChild {
				select {
				case backgroundStarted <- struct{}{}:
				default:
				}
				select {
				case <-backgroundRelease:
				case <-r.Context().Done():
					return
				}
				backgroundDone.Store(true)
				text = "BACKGROUND_CHILD_FINAL"
			} else if backgroundDone.Load() {
				text = "BACKGROUND_PARENT_FINAL"
			}
			block = map[string]any{"type": "text", "text": text}
			stop = "end_turn"
		}
		if background.CompareAndSwap(true, false) {
			block = map[string]any{"type": "tool_use", "id": "tool_" + uuidClaude(), "name": "Agent", "input": map[string]any{"description": "Delayed background validation", "prompt": "BACKGROUND_CHILD_REQUEST: reply with the final marker", "subagent_type": "general-purpose", "run_in_background": true}}
			stop = "tool_use"
		}
		usage := map[string]any{"input_tokens": 10, "output_tokens": 4}
		message := map[string]any{"id": "msg_" + uuidClaude(), "type": "message", "role": "assistant", "model": b["model"], "content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": usage}
		if b["stream"] != true {
			_ = json.NewEncoder(w).Encode(message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(e map[string]any) {
			raw, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], raw)
		}
		message["content"] = []any{}
		message["stop_reason"] = nil
		emit(map[string]any{"type": "message_start", "message": message})
		initial := map[string]any{"type": "text", "text": ""}
		delta := map[string]any{"type": "text_delta", "text": block["text"]}
		if block["type"] == "tool_use" {
			initial = map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}
			data, _ := json.Marshal(block["input"])
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(data)}
		}
		emit(map[string]any{"type": "content_block_start", "index": 0, "content_block": initial})
		emit(map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
		emit(map[string]any{"type": "content_block_stop", "index": 0})
		emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
		emit(map[string]any{"type": "message_stop"})
	}))
	defer model.Close()
	t.Setenv("ANTHROPIC_BASE_URL", model.URL)
	runtimeRoot := t.TempDir()
	manager := clauderuntime.New(runtimeRoot)
	defer manager.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"mira-node-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(f.token))}}
	ws, _, err := dialer.Dial("ws"+strings.TrimPrefix(f.endpoint, "http")+"/v1/nodes/"+f.nodeID+"/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var writes sync.Mutex
	go func() {
		for {
			var b map[string]any
			if ws.ReadJSON(&b) != nil {
				return
			}
			if b["type"] != "request" {
				continue
			}
			go func(b map[string]any) {
				params, _ := b["params"].(map[string]any)
				var value any
				var err error
				if b["capability"] == "file" {
					// The Server reads Node instruction files through the bounded file capability.
					var data []byte
					if data, err = os.ReadFile(params["path"].(string)); err == nil {
						value = map[string]any{"content": base64.StdEncoding.EncodeToString(data), "encoding": "base64", "bytesRead": len(data), "eof": true}
					}
				} else {
					params["endpoint"] = f.endpoint
					params["credential"] = f.token
					value, err = manager.Call(params)
				}
				reply := map[string]any{"type": "response", "requestId": b["requestId"], "ok": err == nil, "result": value}
				if err != nil {
					reply["error"] = map[string]any{"message": err.Error()}
				}
				writes.Lock()
				_ = ws.WriteJSON(reply)
				writes.Unlock()
			}(b)
		}
	}()
	wait := func(name string, timeout time.Duration, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timeout: %s", name)
	}
	wait("channel", 5*time.Second, func() bool { return f.server.channel.IsConnected(f.nodeID) })
	cacheRoute := "/v1/claude/runtimes/" + f.nodeID
	settings := f.call("POST", cacheRoute+"/cache-status", map[string]any{})
	if settings["maxBytes"] != float64(4*1024*1024*1024) {
		t.Fatalf("cache default: %v", settings)
	}
	settings = f.call("POST", cacheRoute+"/cache-configure", map[string]any{"maxBytes": 1024 * 1024})
	if settings["maxBytes"] != float64(1024*1024) {
		t.Fatalf("cache configure: %v", settings)
	}
	for _, invalid := range []any{-1, 0.5, "4", nil} {
		code, _ := f.request("POST", cacheRoute+"/cache-configure", map[string]any{"maxBytes": invalid}, nil)
		if code != 400 {
			t.Fatalf("invalid cache limit %v: %d", invalid, code)
		}
	}
	f.call("POST", "/v1/claude/runtimes/"+f.nodeID+"/prepare", map[string]any{})
	wait("runtime preparation", 3*time.Minute, func() bool {
		s := f.call("POST", "/v1/claude/runtimes/"+f.nodeID+"/status", map[string]any{})
		if s["status"] == "error" {
			t.Fatalf("prepare: %#v", s)
		}
		return s["status"] == "ready"
	})
	description := f.call("POST", "/v1/claude/runtimes/"+f.nodeID+"/describe", map[string]any{})
	if len(description["models"].([]any)) == 0 {
		t.Fatal("empty native model catalog")
	}
	codexInstructions, claudeInstructions := filepath.Join(t.TempDir(), "codex.md"), filepath.Join(t.TempDir(), "claude.md")
	if err = os.WriteFile(codexInstructions, []byte("CODEX_ONLY_INSTRUCTION_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(claudeInstructions, []byte("CLAUDE_NODE_INSTRUCTION_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	f.call("PUT", "/v1/nodes/"+f.nodeID+"/desired-app-server", map[string]any{"running": false, "developerInstructionsFile": codexInstructions, "claudeInstructionsFile": claudeInstructions})
	created := f.call("POST", "/v1/claude/sessions", map[string]any{"requestId": uuidClaude(), "nodeId": f.nodeID, "cwd": workspace, "title": "SDK test"})
	id := created["sessionId"].(string)
	route := "/v1/claude/sessions/" + id
	imagePath := filepath.Join(workspace, "attachment.png")
	imageData, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aV1sAAAAASUVORK5CYII=")
	if err = os.WriteFile(imagePath, imageData, 0600); err != nil {
		t.Fatal(err)
	}
	var nextAttachments any = []any{map[string]any{"path": imagePath, "mime": "image/png", "name": "attachment.png"}}
	runTurn := func(text string) string {
		b := map[string]any{"requestId": uuidClaude(), "text": text, "model": "claude-sonnet-4-6", "attachments": nextAttachments}
		nextAttachments = nil
		f.call("POST", route+"/turns", b)
		f.call("POST", route+"/turns", b)
		return b["requestId"].(string)
	}
	completed := func() bool {
		s := f.call("GET", route, nil)
		if s["activeTurn"] != nil {
			return false
		}
		events := f.call("GET", route+"/events", nil)
		for _, raw := range events["data"].([]any) {
			e := raw.(map[string]any)["payload"].(map[string]any)
			if e["type"] == "mira_error" {
				t.Fatalf("SDK: %#v", e)
			}
		}
		if s["persistence"] != "saved" {
			t.Fatalf("not saved: %#v", s)
		}
		return true
	}
	f.dropNextMirror.Store(true)
	runTurn("Use the Mira status tool and reply with the marker.")
	wait("first SDK turn", 60*time.Second, completed)
	var count int
	if err = f.server.pool.QueryRow(ctx, `SELECT count(*) FROM mira_claude_entries WHERE session_id=$1`, id).Scan(&count); err != nil || count == 0 {
		t.Fatalf("no raw entries: %v %d", err, count)
	}
	if !imageSeen.Load() {
		t.Fatal("attachment did not become a native typed image")
	}
	if !f.replayedLost.Load() {
		t.Fatal("SDK did not retry the exact committed mirror operation")
	}
	if requests.Load() != 2 {
		t.Fatalf("duplicate execution or no tool: %d", requests.Load())
	}
	if system, _ := systemPrompt.Load().(string); !strings.Contains(system, "CLAUDE_NODE_INSTRUCTION_MARKER") || strings.Contains(system, "CODEX_ONLY_INSTRUCTION_MARKER") {
		t.Fatalf("Claude system prompt did not use only the Claude instructions file: %s", system)
	}
	if err = os.RemoveAll(configDir); err != nil {
		t.Fatal(err)
	}
	runTurn("What marker was saved?")
	wait("resume without local history", 60*time.Second, completed)
	settings = f.call("POST", cacheRoute+"/cache-status", map[string]any{})
	if settings["usedBytes"].(float64) <= 0 || settings["entries"].(float64) <= 0 {
		t.Fatalf("native resume did not populate the disposable cache: %v", settings)
	}

	if !resumed.Load() {
		t.Fatal("native resume lost previous messages")
	}

	ask.Store(true)
	runTurn("Ask me a question.")
	questionID := ""
	wait("question", 30*time.Second, func() bool {
		events := f.call("GET", route+"/events", nil)
		for _, raw := range events["data"].([]any) {
			e := raw.(map[string]any)["payload"].(map[string]any)
			if e["type"] == "mira_question" {
				questionID = e["questionId"].(string)
				return true
			}
		}
		return false
	})
	f.call("POST", route+"/answer", map[string]any{"questionId": questionID, "answers": map[string]any{"Choose one option": "First"}})
	wait("question answer", 30*time.Second, completed)
	child.Store(true)
	runTurn("Run a native subagent.")
	wait("native subagent", 60*time.Second, completed)
	children := f.call("GET", route+"/children", nil)
	if len(children["data"].([]any)) == 0 {
		t.Fatal("native subagent transcript was not mirrored")
	}

	// A result can be an interim parent reply while research is still running.
	// The input stream must stay open so its notification can wake the parent.
	backgroundPhase.Store(true)
	background.Store(true)
	backgroundTurn := runTurn("Run a delayed background agent, then synthesize its findings.")
	wait("background request", 30*time.Second, func() bool {
		select {
		case <-backgroundStarted:
			return true
		default:
			return false
		}
	})
	wait("interim parent result", 30*time.Second, func() bool {
		var count int
		_ = f.server.pool.QueryRow(ctx, `SELECT count(*) FROM mira_claude_events WHERE turn_id=$1 AND event_type='result'`, backgroundTurn).Scan(&count)
		return count > 0
	})
	if f.call("GET", route, nil)["activeTurn"] == nil {
		t.Fatal("background work was marked complete at the interim parent result")
	}
	releaseBackground.Do(func() { close(backgroundRelease) })
	wait("parent synthesis after background task", 45*time.Second, completed)
	var final string
	if err = f.server.pool.QueryRow(ctx, `SELECT payload->>'result' FROM mira_claude_events WHERE turn_id=$1 AND event_type='result' ORDER BY seq DESC LIMIT 1`, backgroundTurn).Scan(&final); err != nil || final != "BACKGROUND_PARENT_FINAL" {
		t.Fatalf("background result did not reach the parent: %q %v", final, err)
	}
	backgroundPhase.Store(false)

	// A second approved Node uses a fresh SDK cache/config directory. Server ownership
	// moves only after completion, and the old writer remains fenced out.
	secondNode, credentialID := uuidClaude(), uuidClaude()
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash, _ := foundation.NodeSecretHash(secret)
	secondToken := "mira_node_" + credentialID + "_" + secret
	_, err = f.server.pool.Exec(ctx, `INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status,approved_at,last_seen_at) VALUES($1::uuid,$1::text,'claude-second','linux','amd64','linux','test','{"claudeRuntimeV1":true,"files":true}','[]','approved',now(),now())`, secondNode)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.server.pool.Exec(ctx, `INSERT INTO mira_node_credentials(credential_id,node_id,secret_hash) VALUES($1,$2,$3)`, credentialID, secondNode, hash)
	if err != nil {
		t.Fatal(err)
	}
	secondManager := clauderuntime.New(t.TempDir())
	defer secondManager.Close()
	secondDialer := websocket.Dialer{Subprotocols: []string{"mira-node-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(secondToken))}}
	secondWS, _, err := secondDialer.Dial("ws"+strings.TrimPrefix(f.endpoint, "http")+"/v1/nodes/"+secondNode+"/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer secondWS.Close()
	go func() {
		for {
			var b map[string]any
			if secondWS.ReadJSON(&b) != nil {
				return
			}
			if b["type"] != "request" {
				continue
			}
			params := b["params"].(map[string]any)
			params["endpoint"] = f.endpoint
			params["credential"] = secondToken
			value, err := secondManager.Call(params)
			reply := map[string]any{"type": "response", "requestId": b["requestId"], "ok": err == nil, "result": value}
			if err != nil {
				reply["error"] = map[string]any{"message": err.Error()}
			}
			_ = secondWS.WriteJSON(reply)
		}
	}()
	wait("second channel", 5*time.Second, func() bool { return f.server.channel.IsConnected(secondNode) })
	f.call("POST", "/v1/claude/runtimes/"+secondNode+"/prepare", map[string]any{})
	wait("second runtime", 3*time.Minute, func() bool {
		return f.call("POST", "/v1/claude/runtimes/"+secondNode+"/status", map[string]any{})["status"] == "ready"
	})
	t.Setenv("MIRA_NODE_CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "second-config"))
	resumed.Store(false)
	f.call("POST", route+"/turns", map[string]any{"requestId": uuidClaude(), "text": "Continue the same native session on the second Node.", "nodeId": secondNode, "model": "claude-sonnet-4-6", "cwd": t.TempDir()})
	wait("cross-Node resume", 60*time.Second, completed)
	if !resumed.Load() || f.call("GET", route, nil)["nodeId"] != secondNode {
		t.Fatal("cross-Node resume lost history or ownership")
	}
	status, _ := f.request("GET", route+"/entries", nil, map[string]string{"Authorization": "Bearer " + f.token, "X-Mira-Claude-Turn": uuidClaude(), "X-Mira-Claude-Revision": "1"})
	if status != 409 {
		t.Fatalf("old Node not fenced: %d", status)
	}
	hold.Store(true)
	before := requests.Load()
	turn := runTurn("Wait while I interrupt.")
	wait("model request", 30*time.Second, func() bool { return requests.Load() > before })
	f.call("POST", route+"/interrupt", map[string]any{})
	wait("interrupt and flush", 30*time.Second, func() bool { return f.call("GET", route, nil)["activeTurn"] == nil })
	// The accepted turn must remain idempotent after its process has exited.
	var turnState string
	err = f.server.pool.QueryRow(ctx, `SELECT status FROM mira_claude_turns WHERE turn_id=$1`, turn).Scan(&turnState)
	if err != nil || turnState == "running" {
		t.Fatalf("interrupt state: %s %v", turnState, err)
	}

	hold.Store(false)
	f.failMirror.Store(true)
	degradedTurn := runTurn("Complete even when the transcript mirror fails.")
	wait("degraded mirror", 45*time.Second, func() bool { return f.call("GET", route, nil)["activeTurn"] == nil })
	if f.call("GET", route, nil)["persistence"] != "incomplete" {
		t.Fatal("native mirror_error did not persist degraded status")
	}
	f.failMirror.Store(false)
	status, _ = f.request("POST", route+"/turns", map[string]any{"requestId": uuidClaude(), "text": "Must not silently resume"}, nil)
	if status != 409 {
		t.Fatalf("incomplete resume should require acknowledged-history choice: %d", status)
	}

	// A fresh manager has no memory of the old process; do not mistake its empty
	// process map for proof that a turn from another instance has stopped.
	_, err = f.server.pool.Exec(ctx, `UPDATE mira_claude_turns SET runtime_id='previous-instance' WHERE turn_id=$1`, degradedTurn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.server.pool.Exec(ctx, `UPDATE mira_claude_sessions SET active_turn=$2 WHERE session_id=$1`, id, degradedTurn)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = f.request("POST", route+"/reconcile", map[string]any{}, nil)
	if status != 409 {
		t.Fatalf("restarted runtime asserted absence: %d", status)
	}
	if f.call("GET", route, nil)["activeTurn"] == nil {
		t.Fatal("unknown old turn was silently released")
	}
	_, err = f.server.pool.Exec(ctx, `UPDATE mira_claude_sessions SET active_turn=NULL WHERE session_id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	// Shared account management keeps API credentials local and persists an
	// explicit per-session account binding, including an idle account switch.
	accountIDs := []string{}
	for _, name := range []string{"Messages A", "Messages B"} {
		account := f.call("POST", "/v1/claude/accounts", map[string]any{"nodeId": f.nodeID, "name": name})
		accountID := account["nodeAccountId"].(string)
		accountIDs = append(accountIDs, accountID)
		f.call("POST", "/v1/claude/accounts/"+accountID+"/configure", map[string]any{"provider": map[string]any{"id": "anthropic", "baseUrl": model.URL, "model": "claude-sonnet-4-6"}, "apiKey": "private-test-" + accountID})
	}
	managed := f.call("POST", "/v1/claude/sessions", map[string]any{"requestId": uuidClaude(), "nodeId": f.nodeID, "cwd": workspace, "nodeAccountId": accountIDs[0]})
	managedRoute := "/v1/claude/sessions/" + managed["sessionId"].(string)
	for index, accountID := range accountIDs {
		resumed.Store(false)
		f.call("POST", managedRoute+"/turns", map[string]any{"requestId": uuidClaude(), "text": "Remember the marker and use the Mira status tool.", "nodeAccountId": accountID})
		wait("managed account turn", 60*time.Second, func() bool { return f.call("GET", managedRoute, nil)["activeTurn"] == nil })
		state := f.call("GET", managedRoute, nil)
		if state["nodeAccountId"] != accountID || state["persistence"] != "saved" || modelAuth.Load() != "private-test-"+accountID {
			t.Fatalf("account routing/binding failed: %#v", state)
		}
		if index == 1 && !resumed.Load() {
			t.Fatal("account switch lost native context")
		}
	}
	public := f.call("GET", "/v1/nodes/"+f.nodeID, nil)
	encoded, _ := json.Marshal(public)
	if strings.Contains(string(encoded), "private-test-") {
		t.Fatal("credential appeared in public account metadata")
	}
	if len(public["claudeAccounts"].([]any)) != 2 {
		t.Fatal("managed accounts missing from shared Node account view")
	}
	f.call("PATCH", "/v1/claude/accounts/"+accountIDs[1], map[string]any{"enabled": false})
	status, _ = f.request("POST", managedRoute+"/turns", map[string]any{"requestId": uuidClaude(), "text": "Disabled account must not run"}, nil)
	if status != 409 || f.call("GET", managedRoute, nil)["activeTurn"] != nil {
		t.Fatal("disabled account reserved a turn")
	}
	status, _ = f.request("POST", "/v1/claude/sessions", map[string]any{"requestId": uuidClaude(), "nodeId": secondNode, "cwd": workspace, "nodeAccountId": accountIDs[0]}, nil)
	if status != 409 {
		t.Fatal("account accepted on a different Node")
	}
	f.call("PATCH", "/v1/claude/accounts/"+accountIDs[1], map[string]any{"enabled": true})
	if preview := os.Getenv("MIRA_CLAUDE_PREVIEW_FILE"); preview != "" {
		hold.Store(false)
		if err := os.WriteFile(preview, []byte(f.endpoint), 0600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(preview); os.IsNotExist(err) {
				break
			}
			time.Sleep(time.Second)
		}
	}

}
