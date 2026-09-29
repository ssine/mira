package clauderuntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAccountCredentialsStayLocalAndIsolated(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "wrong-inherited-token")
	t.Setenv("AWS_ACCESS_KEY_ID", "wrong-inherited-key")
	ids := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}
	providers := []AccountProvider{{ID: "anthropic", BaseURL: "https://messages.example", Model: "claude-example"}, {ID: "bedrock", BaseURL: "https://bedrock.example", Region: "us-east-1", Model: "anthropic.claude-example"}}
	for i, id := range ids {
		result, err := m.Call(map[string]any{"action": "account/configure", "nodeAccountId": id, "provider": providers[i], "apiKey": "private-" + id})
		if err != nil || result.(map[string]any)["configured"] != true {
			t.Fatalf("configure: %v %v", result, err)
		}
	}
	for i, id := range ids {
		values, err := m.accountEnvironment(id)
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		for _, v := range values {
			k, v, _ := strings.Cut(v, "=")
			env[k] = v
		}
		if env["ANTHROPIC_AUTH_TOKEN"] != "" || env["AWS_ACCESS_KEY_ID"] != "" {
			t.Fatal("inherited credentials contaminated the account")
		}
		keyName := "ANTHROPIC_API_KEY"
		if i == 1 {
			keyName = "AWS_BEARER_TOKEN_BEDROCK"
			if env["ANTHROPIC_API_KEY"] != "" || env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
				t.Fatal("Bedrock mixed with Messages authentication")
			}
		}
		if env[keyName] != "private-"+id || !strings.Contains(env["CLAUDE_CONFIG_DIR"], id) {
			t.Fatal("account credentials/config directory crossed")
		}
		info, err := os.Stat(filepath.Join(env["CLAUDE_CONFIG_DIR"], "mira-account.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private account file permissions: %v", err)
		}
	}
	m.processes["active"] = &process{accountID: ids[0]}
	if _, err := m.Call(map[string]any{"action": "account/configure", "nodeAccountId": ids[0], "apiKey": "changed"}); err == nil {
		t.Fatal("modified active account")
	}
	delete(m.processes, "active")
	if _, err := m.accountEnvironment("00000000-0000-4000-8000-000000000003"); err == nil {
		t.Fatal("unknown account fell back to ambient login")
	}
	result, err := m.Call(map[string]any{"action": "account/configure", "nodeAccountId": ids[0], "apiKey": ""})
	if err != nil || result.(map[string]any)["configured"] != false {
		t.Fatal("clearing key did not unconfigure account")
	}
	if _, err = m.accountEnvironment(ids[0]); err == nil {
		t.Fatal("cleared account retained authentication")
	}
}

func TestAccountProviderRejectsCredentialsInPublicMetadata(t *testing.T) {
	for _, base := range []string{"https://user:secret@example.test", "https://example.test?key=secret", "file:///tmp/config"} {
		if (AccountProvider{ID: "anthropic", BaseURL: base, Model: "claude"}).Validate() == nil {
			t.Fatal("accepted credentials or unsupported transport in public URL")
		}
	}
}
