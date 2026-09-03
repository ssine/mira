//go:build !windows

package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMiraCodexUsesRemoteStoreAndNodeCredential(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "mira-codex")
	argumentsFile := filepath.Join(directory, "arguments")
	tokenFile := filepath.Join(directory, "token")
	script := `#!/bin/sh
case "$*" in
  *"features list"*) exit 0 ;;
esac
printf '%s\n' "$@" > "$MIRA_TEST_ARGUMENTS"
printf '%s' "$MIRA_NODE_TOKEN" > "$MIRA_TEST_TOKEN"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BINARY", binary)
	t.Setenv("MIRA_CODEX_STORE_ID", "test-store")
	t.Setenv("MIRA_TEST_ARGUMENTS", argumentsFile)
	t.Setenv("MIRA_TEST_TOKEN", tokenFile)
	client := &cliClient{identity: &persistedNodeState{
		ServerURL: "https://mira.example.test", Token: "node-secret",
	}}
	if err := client.runCodex(context.Background(), []string{"--", "--version"}, bytes.NewReader(nil), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(arguments)
	for _, expected := range []string{
		`experimental_thread_store.type="remote_http"`,
		`experimental_thread_store.endpoint="https://mira.example.test"`,
		`experimental_thread_store.store_id="test-store"`,
		"--version",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("Codex arguments omitted %q: %s", expected, joined)
		}
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil || string(token) != "node-secret" {
		t.Fatalf("Codex did not inherit the Node credential: %q, %v", token, err)
	}
}
