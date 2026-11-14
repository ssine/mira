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

func writeOfflineAccount(t *testing.T, identityDir, id string) string {
	t.Helper()
	home := filepath.Join(identityDir, "accounts", id, "claude")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"mira-account.json": `{"provider":{"id":"anthropic","baseUrl":"https://api.anthropic.com","model":"claude-test","effort":"high"},"apiKey":"","authType":"subscription"}`,
		".credentials.json": `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":` + strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `,"scopes":["user:inference"]}}`,
		".claude.json":      `{"oauthAccount":{"emailAddress":"user@example.test"}}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestOfflineAccountsAndSessionEnvironment(t *testing.T) {
	identityDir := t.TempDir()
	id := "00000000-0000-4000-8000-0000000000c1"
	home := writeOfflineAccount(t, identityDir, id)
	accounts, err := ListOfflineAccounts(identityDir)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts: %#v %v", accounts, err)
	}
	account := accounts[0]
	if !account.Configured || account.Email != "user@example.test" || account.ConfigDir != home || account.Provider.Model != "claude-test" || account.Egress != nil {
		t.Fatalf("unexpected offline account: %#v", account)
	}
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-leak")
	session, err := New(identityDir).OfflineSession(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Release()
	env := map[string]string{}
	for _, entry := range session.Environment {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	if env["CLAUDE_CONFIG_DIR"] != home || env["ANTHROPIC_MODEL"] != "claude-test" {
		t.Fatalf("account environment missing: %v", env)
	}
	for _, key := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "ANTHROPIC_API_KEY"} {
		if _, ok := env[key]; ok {
			t.Fatalf("%s leaked into the offline session", key)
		}
	}
	if strings.Join(session.Arguments, " ") != "--effort high" {
		t.Fatalf("effort default: %v", session.Arguments)
	}
	explicit, err := New(identityDir).OfflineSession(id, []string{"--effort=low"})
	if err != nil {
		t.Fatal(err)
	}
	explicit.Release()
	if len(explicit.Arguments) != 0 {
		t.Fatalf("explicit effort must win: %v", explicit.Arguments)
	}
}

func TestLocalExecutableFindsPreparedSDKBinary(t *testing.T) {
	identityDir := t.TempDir()
	t.Setenv("MIRA_NODE_CLAUDE_BINARY", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := LocalExecutable(identityDir); err == nil {
		t.Fatal("expected a missing executable error")
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := filepath.Join(identityDir, "runtimes", "claude", "older-sdk")
	binary := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-agent-sdk-"+sdkPlatform(), name)
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{binary, filepath.Join(dir, "ready")} {
		if err := os.WriteFile(path, []byte("x"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := LocalExecutable(identityDir); err != nil || got != binary {
		t.Fatalf("LocalExecutable = %q, %v; want %q", got, err, binary)
	}
	t.Setenv("MIRA_NODE_CLAUDE_BINARY", "/explicit/claude")
	if got, _ := LocalExecutable(identityDir); got != "/explicit/claude" {
		t.Fatalf("explicit binary ignored: %q", got)
	}
}
