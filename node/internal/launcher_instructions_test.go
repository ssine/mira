package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func TestFetchLauncherInstructions(t *testing.T) {
	nodeID := "019a0000-0000-7000-8000-0000000000aa"
	fileError := any(nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/nodes/"+nodeID+"/launcher-instructions" || r.Header.Get("Authorization") != "Bearer test-only-token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cliInstructions": "MIRA_CLI_INSTRUCTIONS_V1_BEGIN\ncli\nMIRA_CLI_INSTRUCTIONS_V1_END",
			"developerInstructions": "node file", "developerInstructionsError": fileError})
	}))
	defer server.Close()
	client := &cliClient{identity: &persistedNodeState{ServerURL: server.URL, NodeID: nodeID, Token: "test-only-token"}, http: &http.Client{Timeout: 5 * time.Second}}
	var stderr bytes.Buffer
	if text := fetchLauncherInstructions(context.Background(), client, &stderr); !strings.HasSuffix(text, "MIRA_CLI_INSTRUCTIONS_V1_END\n\nnode file") || stderr.Len() != 0 {
		t.Fatalf("instructions %q, stderr %q", text, stderr.String())
	}
	fileError = "file missing"
	if text := fetchLauncherInstructions(context.Background(), client, &stderr); !strings.Contains(stderr.String(), "file missing") || text == "" {
		t.Fatalf("file error: %q %q", text, stderr.String())
	}
	stderr.Reset()
	client.identity.NodeID = "019a0000-0000-7000-8000-0000000000bb" // Not served: 404.
	if text := fetchLauncherInstructions(context.Background(), client, &stderr); text != "" || !strings.Contains(stderr.String(), "launching without them") {
		t.Fatalf("unavailable: %q %q", text, stderr.String())
	}
	if fetchLauncherInstructions(context.Background(), nil, &stderr) != "" {
		t.Fatal("offline launches inject nothing")
	}
}

func TestCodexDeveloperInstructionsMerge(t *testing.T) {
	home := t.TempDir()
	mira := "MIRA_CLI_INSTRUCTIONS_V1_BEGIN\nnew \"quoted\" <tag> 中文\nMIRA_CLI_INSTRUCTIONS_V1_END"
	decode := func(override string) string {
		t.Helper()
		var parsed struct {
			Value string `toml:"developer_instructions"`
		}
		if err := toml.Unmarshal([]byte(override), &parsed); err != nil {
			t.Fatalf("override %q is not TOML: %v", override, err)
		}
		return parsed.Value
	}
	if override, rest := codexDeveloperInstructions("", home, []string{"a=1"}); override != "" || len(rest) != 1 {
		t.Fatal("empty instructions must not override")
	}
	if override, _ := codexDeveloperInstructions(mira, home, nil); decode(override) != mira {
		t.Fatalf("no config: %q", decode(override))
	}
	config := "model = \"x\"\ndeveloper_instructions = \"\"\"\nmine\nMIRA_CLI_INSTRUCTIONS_V1_BEGIN\nstale\nMIRA_CLI_INSTRUCTIONS_V1_END\n\"\"\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if override, _ := codexDeveloperInstructions(mira, home, nil); decode(override) != "mine\n\n"+mira {
		t.Fatalf("config merge: %q", decode(override))
	}
	override, rest := codexDeveloperInstructions(mira, home, []string{`developer_instructions="account"`, `model="y"`})
	if decode(override) != "account\n\n"+mira || len(rest) != 1 || rest[0] != `model="y"` {
		t.Fatalf("account override merge: %q %v", decode(override), rest)
	}
}

func TestClaudeSystemPromptArguments(t *testing.T) {
	var stderr bytes.Buffer
	extra, rest := claudeSystemPromptArguments("MIRA", []string{"-p", "hi"}, &stderr)
	if strings.Join(extra, "|") != "--append-system-prompt|MIRA" || strings.Join(rest, "|") != "-p|hi" {
		t.Fatalf("plain: %v %v", extra, rest)
	}
	extra, rest = claudeSystemPromptArguments("MIRA", []string{"--append-system-prompt", "mine", "--append-system-prompt=more", "-p", "x", "--", "--append-system-prompt"}, &stderr)
	if strings.Join(extra, "|") != "--append-system-prompt|MIRA\n\nmine\n\nmore" || strings.Join(rest, "|") != "-p|x|--|--append-system-prompt" {
		t.Fatalf("merge: %q %v", extra, rest)
	}
	extra, rest = claudeSystemPromptArguments("MIRA", []string{"--append-system-prompt-file", "f"}, &stderr)
	if extra != nil || len(rest) != 2 || !strings.Contains(stderr.String(), "not added") {
		t.Fatalf("file: %v %v", extra, rest)
	}
	if extra, rest = claudeSystemPromptArguments("", []string{"x"}, &stderr); extra != nil || len(rest) != 1 {
		t.Fatal("empty instructions add nothing")
	}
	options, err := parseLauncherArgs([]string{"--no-mira-instructions", "--account", "a", "-p", "x"}, false)
	if err != nil || !options.noInstructions || options.account != "a" || strings.Join(options.args, "|") != "-p|x" {
		t.Fatalf("launcher args: %v %#v", err, options)
	}
}
