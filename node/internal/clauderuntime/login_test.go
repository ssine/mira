package clauderuntime

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionLoginLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "claude")
	script := `#!/bin/sh
if [ "$1 $2" = "auth logout" ]; then
  rm -f "$CLAUDE_CONFIG_DIR/.credentials.json"
  echo "Logged out"
  exit 0
fi
echo "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&state=fixture"
echo "Paste code here if prompted > "
IFS= read -r code
[ -n "$code" ] || exit 1
mkdir -p "$CLAUDE_CONFIG_DIR"
cat > "$CLAUDE_CONFIG_DIR/.credentials.json" <<EOF
{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","expiresAt":$(( $(date +%s) * 1000 + 3600000 )),"refreshTokenExpiresAt":$(( $(date +%s) * 1000 + 86400000 )),"scopes":["user:inference"]}}
EOF
echo "Login successful"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_NODE_CLAUDE_BINARY", binary)

	m := New(root)
	defer m.Close()
	m.state = "ready"
	m.dir = root
	id := "00000000-0000-4000-8000-000000000001"
	result, err := m.Call(map[string]any{
		"action": "account/configure", "nodeAccountId": id, "authType": "subscription",
		"provider": map[string]any{"id": "anthropic", "baseUrl": "https://api.anthropic.com", "model": "opus"},
	})
	if err != nil || result.(map[string]any)["configured"] != false {
		t.Fatalf("configure subscription: %#v %v", result, err)
	}
	started, err := m.Call(map[string]any{"action": "account/login/start", "nodeAccountId": id, "email": "person@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	state := started.(map[string]any)
	if state["status"] != "pending" || !strings.Contains(state["verificationUrl"].(string), "oauth/authorize") {
		t.Fatalf("login start: %#v", state)
	}
	sessionID := state["sessionId"].(string)
	completed, err := m.Call(map[string]any{"action": "account/login/complete", "nodeAccountId": id, "sessionId": sessionID, "code": "code#state"})
	if err != nil || completed.(map[string]any)["status"] != "completed" {
		t.Fatalf("login complete: %#v %v", completed, err)
	}
	home, _ := m.accountHome(id)
	credentials := filepath.Join(home, ".credentials.json")
	info, err := os.Stat(credentials)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private credentials: %v %v", info, err)
	}
	if values, err := m.accountEnvironment(id); err != nil || len(values) == 0 {
		t.Fatalf("subscription environment after login: %v", err)
	}
	loggedOut, err := m.Call(map[string]any{"action": "account/logout", "nodeAccountId": id})
	if err != nil || loggedOut.(map[string]any)["configured"] != false {
		t.Fatalf("logout: %#v %v", loggedOut, err)
	}
	if _, err = m.accountEnvironment(id); err == nil {
		t.Fatal("logged-out subscription environment remained configured")
	}
}

func TestExpiredOAuthRefreshTokenIsUnconfigured(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	id := "00000000-0000-4000-8000-000000000002"
	home, _ := m.accountHome(id)
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Hour).UnixMilli()
	data := `{"claudeAiOauth":{"accessToken":"access","refreshToken":"refresh","refreshTokenExpiresAt":` + strconv.FormatInt(expired, 10) + `,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if validOAuthCredentials(filepath.Join(home, ".credentials.json")) {
		t.Fatal("expired refresh token was accepted")
	}
}
