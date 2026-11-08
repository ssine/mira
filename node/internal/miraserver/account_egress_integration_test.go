package miraserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/accountegress"
	"github.com/ssine/mira/node/internal/clauderuntime"
)

func TestClaudeAccountEgressLifecycle(t *testing.T) {
	f := newClaudeFixture(t)
	ctx := context.Background()
	if _, err := f.server.pool.Exec(ctx, `UPDATE codex_nodes SET capabilities=capabilities||'{"accountEgressV1":true}' WHERE node_id=$1`, f.nodeID); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	proxyScript := filepath.Join(root, "proxy.py")
	if err := os.WriteFile(proxyScript, []byte(egressTestProxy), 0600); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(root, "sing-box")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexec python3 \"$EGRESS_TEST_PROXY\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_NODE_SING_BOX", command)
	t.Setenv("EGRESS_TEST_PROXY", proxyScript)
	egress := accountegress.New(root)
	defer egress.Close()
	dialer := websocket.Dialer{Subprotocols: []string{"mira-node-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(f.token))}}
	ws, _, err := dialer.Dial("ws"+strings.TrimPrefix(f.endpoint, "http")+"/v1/nodes/"+f.nodeID+"/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var writes sync.Mutex
	go func() {
		for {
			var message map[string]any
			if ws.ReadJSON(&message) != nil {
				return
			}
			if message["type"] != "request" || message["capability"] != "egress" {
				continue
			}
			params, _ := message["params"].(map[string]any)
			id, _ := params["nodeAccountId"].(string)
			var value any
			var err error
			switch params["action"] {
			case "configure":
				spec, public, parseErr := accountegress.ParsePublic(params["egress"])
				if parseErr == nil {
					_, parseErr = egress.Configure(id, *spec)
					if parseErr == nil {
						value = map[string]any{"configured": true, "egress": public}
					}
				}
				err = parseErr
			case "delete":
				err = egress.Delete(id)
				value = map[string]any{"configured": false}
			case "status":
				value, err = egress.Status(id)
			}
			reply := map[string]any{"type": "response", "requestId": message["requestId"], "ok": err == nil, "result": value}
			if err != nil {
				reply["error"] = map[string]any{"message": err.Error()}
			}
			writes.Lock()
			_ = ws.WriteJSON(reply)
			writes.Unlock()
		}
	}()

	account := f.call("POST", "/v1/claude/accounts", map[string]any{"nodeId": f.nodeID, "name": "Egress account"})
	accountID := account["nodeAccountId"].(string)
	if _, err := f.server.pool.Exec(ctx, `UPDATE mira_claude_accounts SET provider='{"id":"anthropic","baseUrl":"https://api.example.test","model":"claude-example"}',configured=true WHERE node_account_id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	route := "/v1/accounts/claude/" + accountID + "/egress"
	result := f.call("POST", route, map[string]any{"type": "socks5", "server": "proxy.example", "port": 8022, "username": "user", "password": "secret", "profile": "claude"})
	if result["configured"] != true {
		t.Fatalf("configure result: %#v", result)
	}
	accountDir := filepath.Join(root, "accounts", accountID, "claude")
	if err := os.MkdirAll(accountDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accountDir, "mira-account.json"), []byte(`{"provider":{"id":"anthropic","baseUrl":"https://api.example.test","model":"claude-example"},"apiKey":"private-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	claude := clauderuntime.NewWithEgress(root, egress)
	defer claude.Close()
	if _, err := claude.Call(map[string]any{"action": "status", "nodeAccountId": accountID}); err != nil {
		t.Fatalf("Claude account environment did not acquire egress: %v", err)
	}
	var configured bool
	if err := f.server.pool.QueryRow(ctx, `SELECT egress_configured FROM mira_claude_accounts WHERE node_account_id=$1`, accountID).Scan(&configured); err != nil || !configured {
		t.Fatalf("egress not stored: %v", err)
	}
	public := f.call("GET", "/v1/nodes/"+f.nodeID, nil)
	raw, _ := json.Marshal(public)
	if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), `"egressConfigured":true`) {
		t.Fatalf("public egress metadata: %s", raw)
	}
	status := f.call("GET", route, nil)
	if status["configured"] != true || status["reported"] == nil {
		t.Fatalf("status: %#v", status)
	}
	if result = f.call("DELETE", route, nil); result["configured"] != false {
		t.Fatalf("delete result: %#v", result)
	}
	time.Sleep(50 * time.Millisecond)
}

const egressTestProxy = `import json, select, socket, sys, threading
config = json.load(open(sys.argv[sys.argv.index("-c") + 1]))
port = config["inbounds"][0]["listen_port"]
server = socket.socket()
server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
server.bind(("127.0.0.1", port))
server.listen()
def pipe(client):
    try:
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = client.recv(4096)
            if not chunk: return
            data += chunk
        line = data.split(b"\r\n", 1)[0].decode()
        method, target, _ = line.split(" ", 2)
        if method != "CONNECT": raise ValueError()
        host, target_port = target.rsplit(":", 1)
        upstream = socket.create_connection((host, int(target_port)), 5)
        client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        while True:
            ready, _, _ = select.select([client, upstream], [], [], 10)
            if not ready: break
            for source in ready:
                chunk = source.recv(65536)
                if not chunk: return
                (upstream if source is client else client).sendall(chunk)
    except Exception:
        pass
    finally:
        client.close()
while True:
    client, _ = server.accept()
    threading.Thread(target=pipe, args=(client,), daemon=True).start()
`
