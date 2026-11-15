//go:build !windows

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseLauncherArgs(t *testing.T) {
	got, err := parseLauncherArgs([]string{"--account", "work", "--offline", "exec", "--account", "x"}, true)
	if err != nil || got.account != "work" || got.mode != "offline" || strings.Join(got.args, " ") != "exec --account x" {
		t.Fatalf("parse: %#v %v", got, err)
	}
	got, err = parseLauncherArgs([]string{"-a=ignored", "--", "--offline"}, false)
	if err != nil || got.account != "" || strings.Join(got.args, " ") != "-a=ignored -- --offline" {
		t.Fatalf("unknown option must start engine arguments: %#v %v", got, err)
	}
	got, _ = parseLauncherArgs([]string{"--account=abcd", "--", "--offline"}, false)
	if got.account != "abcd" || strings.Join(got.args, " ") != "--offline" {
		t.Fatalf("separator: %#v", got)
	}
	if _, err := parseLauncherArgs([]string{"--account"}, false); err == nil {
		t.Fatal("missing selector accepted")
	}
}

func TestMatchAndChooseAccount(t *testing.T) {
	choices := []localAccountChoice{
		{Engine: "codex", Selector: "11111111-0000-4000-8000-000000000001", ID: "11111111-0000-4000-8000-000000000001", Name: "Work", Usable: true},
		{Engine: "codex", Selector: "11112222-0000-4000-8000-000000000002", ID: "11112222-0000-4000-8000-000000000002", Usable: false, Problem: "disabled in Mira"},
		{Engine: "codex", Selector: "local", Usable: true},
	}
	for selector, want := range map[string]string{"work": "11111111-0000-4000-8000-000000000001", "LOCAL": "local", "11112222": "11112222-0000-4000-8000-000000000002"} {
		if got, err := matchAccount(choices, selector); err != nil || got.Selector != want {
			t.Fatalf("match %q = %#v %v", selector, got, err)
		}
	}
	if _, err := matchAccount(choices, "1111"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	identityFile := filepath.Join(t.TempDir(), "identity.json")
	if _, err := chooseAccount("Codex", "11112222", identityFile, choices, bytes.NewReader(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("disabled account accepted")
	}
	_, err := chooseAccount("Codex", "", identityFile, choices, bytes.NewReader(nil), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--account") || !strings.Contains(err.Error(), "Work") {
		t.Fatalf("non-interactive selection must list accounts: %v", err)
	}
	single := choices[:1]
	if got, err := chooseAccount("Codex", "", identityFile, single, bytes.NewReader(nil), &bytes.Buffer{}); err != nil || got.Name != "Work" {
		t.Fatalf("single account: %#v %v", got, err)
	}
	rememberRecentAccount(identityFile, "Codex", "local")
	if readRecentAccounts(identityFile)["Codex"] != "local" {
		t.Fatal("recent account was not remembered")
	}
}

func TestLocalAccountIndexFromDesiredState(t *testing.T) {
	directory := t.TempDir()
	identityFile := filepath.Join(directory, "identity.json")
	t.Setenv("CODEX_HOME", filepath.Join(directory, "node-codex"))
	client := &controlClient{configuration: config{IdentityFile: identityFile, ConfigOverrides: []string{`model="local"`}}}
	client.desired = desiredAppServer{ConfigOverrides: []string{`model="node"`}}
	id := "21dfcf17-1486-4e89-922a-b67a4fd644d9"
	client.setClaudeLabels(&[]localClaudeLabel{{NodeAccountID: "C71824CA-BD0C-4E5E-B224-4553561D6944", Name: "Max", Enabled: true}, {NodeAccountID: "bad", Name: "x"}})
	client.setDesiredAccounts([]desiredCodexAccount{
		{NodeAccountID: id, Name: "Mafia", Enabled: true, Desired: desiredAppServer{ConfigOverrides: []string{`model="mafia"`}, InheritEnv: []string{"EXTRA"}}},
		{NodeAccountID: "63799911-4ed0-5f3d-d831-97d4560f7a47", Name: "Node", IsDefault: true, Enabled: true},
	})
	index, err := readLocalAccountIndex(identityFile)
	if err != nil || len(index.Codex) != 2 {
		t.Fatalf("index: %#v %v", index, err)
	}
	byID := map[string]localCodexAccount{}
	for _, account := range index.Codex {
		byID[account.NodeAccountID] = account
	}
	mafia := byID[id]
	if mafia.Name != "Mafia" || mafia.Legacy || mafia.CodexHome != filepath.Join(directory, "accounts", id, "codex") ||
		strings.Join(mafia.ConfigOverrides, ";") != `agents.max_threads=1000;model="mafia"` || strings.Join(mafia.InheritEnv, ",") != "EXTRA" {
		t.Fatalf("account entry: %#v", mafia)
	}
	legacy := byID["63799911-4ed0-5f3d-d831-97d4560f7a47"]
	if !legacy.Legacy || legacy.CodexHome != filepath.Join(directory, "node-codex") ||
		strings.Join(legacy.ConfigOverrides, ";") != `agents.max_threads=1000;model="node";model="local"` {
		t.Fatalf("legacy entry: %#v", legacy)
	}
	if len(index.Claude) != 1 || index.Claude[0].NodeAccountID != "c71824ca-bd0c-4e5e-b224-4553561d6944" || index.Claude[0].Name != "Max" {
		t.Fatalf("Claude labels: %#v", index.Claude)
	}
	// A Server that omits Claude labels must not erase the cached ones.
	restarted := &controlClient{configuration: client.configuration}
	restarted.setClaudeLabels(nil)
	restarted.setDesiredAccounts(nil)
	if again, err := readLocalAccountIndex(identityFile); err != nil || len(again.Claude) != 1 || len(again.Codex) != 1 {
		t.Fatalf("labels after an older Server response: %#v %v", again, err)
	}
	info, err := os.Stat(filepath.Join(directory, "accounts", localAccountIndexName))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("index must be private: %v %v", info, err)
	}
}

func TestCodexLauncherRunsSelectedAccountOffline(t *testing.T) {
	directory := t.TempDir()
	identityFile := filepath.Join(directory, "identity.json")
	id := "21dfcf17-1486-4e89-922a-b67a4fd644d9"
	home := filepath.Join(directory, "accounts", id, "codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(managedAccountProfile{Provider: managedAccountProvider{ID: "mafia", Name: "Mafia API", BaseURL: "https://api.example.test/v1", EnvKey: "MAFIA_KEY"}, APIKey: "profile-secret"})
	if err := os.WriteFile(filepath.Join(directory, "accounts", id, "profile.json"), profile, 0600); err != nil {
		t.Fatal(err)
	}
	index, _ := json.Marshal(localAccountIndex{SchemaVersion: 1, Codex: []localCodexAccount{{NodeAccountID: id, Name: "Mafia", Enabled: true, CodexHome: home, ConfigOverrides: []string{codexDefaultSubagentOverride}}}})
	if err := os.WriteFile(filepath.Join(directory, "accounts", localAccountIndexName), index, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "codex")
	record := filepath.Join(directory, "record")
	script := `#!/bin/sh
case "$*" in *"features list"*) exit 1 ;; esac
{ printf '%s\n' "$@"; echo "HOME_DIR=$CODEX_HOME"; echo "KEY=$MAFIA_KEY"; echo "TOKEN=$MIRA_NODE_TOKEN"; } > "` + record + `"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BINARY", binary)
	var stderr bytes.Buffer
	err := runCodexLauncher(context.Background(), cliOptions{Identity: identityFile}, []string{"--offline", "--account", "mafia", "exec", "hi"}, bytes.NewReader(nil), &bytes.Buffer{}, &stderr)
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	for _, expected := range []string{`model_provider="mafia"`, `approval_policy="never"`, "HOME_DIR=" + home, "KEY=profile-secret", "TOKEN=\n", "exec\nhi\n"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("offline Codex launch missing %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, "experimental_thread_store") {
		t.Fatalf("offline launch must not use the remote ThreadStore:\n%s", output)
	}
	if !strings.Contains(stderr.String(), "Mafia") || !strings.Contains(stderr.String(), "offline") {
		t.Fatalf("launcher banner: %s", stderr.String())
	}
}

func TestLauncherHelpBelongsToEngineAfterItsArguments(t *testing.T) {
	for _, test := range []struct {
		args []string
		help bool
	}{
		{[]string{"codex", "--help"}, true},
		{[]string{"claude", "--account", "x", "-h"}, true},
		{[]string{"codex", "exec", "--help"}, false},
		{[]string{"claude", "--", "--help"}, false},
	} {
		topic, help := requestedCLIHelp(test.args)
		if help != test.help || help && strings.Join(topic, " ") != test.args[0] {
			t.Fatalf("%v: help=%v topic=%v", test.args, help, topic)
		}
	}
}

func TestCodexLauncherSendsAccountAndRuntimeContextOnline(t *testing.T) {
	directory := t.TempDir()
	id := "21dfcf17-1486-4e89-922a-b67a4fd644d9"
	home := filepath.Join(directory, "accounts", id, "codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "codex")
	record := filepath.Join(directory, "record")
	script := `#!/bin/sh
case "$*" in *"features list"*) exit 0 ;; esac
{ echo "ACCOUNT=$MIRA_NODE_CODEX_ACCOUNT_ID"; echo "RUNTIME=$MIRA_NODE_CODEX_RUNTIME_ID"; } > "` + record + `"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := &cliClient{identity: &persistedNodeState{ServerURL: "https://mira.example.test", Token: "node-token"}}
	launch := codexLaunch{account: localCodexAccount{NodeAccountID: id, Name: "Mafia", Enabled: true, CodexHome: home},
		client: client, identityFile: filepath.Join(directory, "identity.json"), binary: binary}
	var stderr bytes.Buffer
	if err := runCodexAccount(context.Background(), launch, []string{"exec", "hi"}, bytes.NewReader(nil), &bytes.Buffer{}, &stderr); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	runtimeLine := regexp.MustCompile(`(?m)^RUNTIME=([0-9a-f-]{36})$`).FindStringSubmatch(output)
	if !strings.Contains(output, "ACCOUNT="+id+"\n") || runtimeLine == nil || !nodeUUIDPattern.MatchString(runtimeLine[1]) {
		t.Fatalf("online launch must send a valid account binding and runtime instance:\n%s", output)
	}
}
