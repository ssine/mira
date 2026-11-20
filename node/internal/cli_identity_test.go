package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIInstallationResolution(t *testing.T) {
	state := t.TempDir()
	identity := filepath.Join(state, "node-identity.json")
	t.Setenv("MIRA_STATE_DIR", state)
	t.Setenv("MIRA_IDENTITY_FILE", "")
	t.Setenv("HOME", t.TempDir())

	// No node.json yet: the legacy per-user default.
	resolved, err := resolveCLIInstallation()
	if err != nil || resolved.StateDir != state || resolved.IdentitySource != "default" {
		t.Fatalf("default: %#v %v", resolved, err)
	}
	config, _ := json.Marshal(map[string]any{"serverUrl": "https://mira.example.test", "identityFile": identity})
	if err := os.WriteFile(filepath.Join(state, "node.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	// The installation's identity is used even before it exists (not yet enrolled).
	resolved, err = resolveCLIInstallation()
	if err != nil || resolved.IdentityFile != identity || resolved.IdentitySource != "node.json" {
		t.Fatalf("node.json: %#v %v", resolved, err)
	}
	options, _, err := parseGlobalCLI([]string{"nodes", "list"})
	if err != nil || options.Identity != identity {
		t.Fatalf("CLI options: %#v %v", options, err)
	}
	_, err = newCLIClient(options)
	message := ""
	if err != nil {
		message = err.Error()
	}
	for _, expected := range []string{state, identity, "node.json", "Do not start another node-worker"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("not enrolled message %q lacks %q", message, expected)
		}
	}
	if cliExitCode(err) != 2 {
		t.Fatalf("exit code %d", cliExitCode(err))
	}
	// The explicit environment override still wins.
	override := filepath.Join(t.TempDir(), "other.json")
	t.Setenv("MIRA_IDENTITY_FILE", override)
	if resolved, err = resolveCLIInstallation(); err != nil || resolved.IdentityFile != override || resolved.IdentitySource != "MIRA_IDENTITY_FILE" {
		t.Fatalf("override: %#v %v", resolved, err)
	}
}

func TestInstallationSettingsAreAllowlisted(t *testing.T) {
	state := t.TempDir()
	content := "# service environment\nCODEX_BINARY=/opt/codex/bin/codex\nexport MIRA_NODE_SING_BOX=\"/opt/sing-box\"\n" +
		"MIRA_NODE_CODEX_CACHE='/data/cache'\nDATABASE_URL=postgresql://secret\nPATH=/evil\nMIRA_NODE_TOKEN=secret\nMIRA_NODE_CLAUDE_NODE=/from/file\n"
	if err := os.WriteFile(filepath.Join(state, "mira.env"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	for _, key := range []string{"CODEX_BINARY", "MIRA_NODE_SING_BOX", "MIRA_NODE_CODEX_CACHE", "DATABASE_URL", "MIRA_NODE_TOKEN"} {
		t.Setenv(key, "")
	}
	t.Setenv("MIRA_NODE_CLAUDE_NODE", "/explicit")
	applyInstallationSettings(state)
	for key, expected := range map[string]string{
		"CODEX_BINARY": "/opt/codex/bin/codex", "MIRA_NODE_SING_BOX": "/opt/sing-box", "MIRA_NODE_CODEX_CACHE": "/data/cache",
		"DATABASE_URL": "", "MIRA_NODE_TOKEN": "", "MIRA_NODE_CLAUDE_NODE": "/explicit", "PATH": path,
	} {
		if os.Getenv(key) != expected {
			t.Fatalf("%s=%q, want %q", key, os.Getenv(key), expected)
		}
	}
	applyInstallationSettings(filepath.Join(state, "missing")) // No file: no change, no error.
}

func TestMissingRouteNamesServerVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_, _ = w.Write([]byte(`{"status":"ok","version":"1.0.129"}`))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
	}))
	defer server.Close()
	client := &cliClient{identity: &persistedNodeState{ServerURL: server.URL, Token: "test-only-token"}, http: &http.Client{Timeout: 5 * time.Second}}
	_, err := client.runConversation(context.Background(), []string{"read", "019a0000-0000-7000-8000-000000000001"}, true)
	if err == nil || !strings.Contains(err.Error(), "Mira Server 1.0.129") || !strings.Contains(err.Error(), "needs 1.0.131 or newer") || cliExitCode(err) != 4 {
		t.Fatalf("unsupported Server: %v", err)
	}
	missing := &cliHTTPError{Status: 404, Code: "not_found", Message: "conversation not found"}
	if client.explainMissingRoute(context.Background(), missing, "conversation read", "1.0.131") != error(missing) {
		t.Fatal("a missing conversation keeps its own error")
	}
}
