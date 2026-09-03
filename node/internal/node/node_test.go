package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDisplaySizePrefersOverride(t *testing.T) {
	width, height := parseDisplaySize("Physical size: 1240x2772\nOverride size: 1080x2400\n")
	if width != 1080 || height != 2400 {
		t.Fatalf("got %dx%d", width, height)
	}
}

func TestPathContained(t *testing.T) {
	tests := []struct {
		root      string
		candidate string
		expected  bool
	}{
		{"/data/local/tmp", "/data/local/tmp", true},
		{"/data/local/tmp", "/data/local/tmp/file", true},
		{"/data/local/tmp", "/data/local/tmp-other/file", false},
		{"/data/local/tmp", "/data/local/file", false},
	}
	for _, test := range tests {
		if actual := pathContained(test.root, test.candidate); actual != test.expected {
			t.Errorf("pathContained(%q, %q) = %v, want %v", test.root, test.candidate, actual, test.expected)
		}
	}
}

func TestDefaultAllowedRootsExposeMachineFilesystem(t *testing.T) {
	roots := defaultAllowedRoots()
	if len(roots) == 0 {
		t.Fatal("default roots must expose at least one filesystem root")
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			t.Fatalf("default root is not absolute: %q", root)
		}
	}
}

func TestOutputBufferCursor(t *testing.T) {
	var buffer outputBuffer
	buffer.push("stdout", "abc")
	buffer.push("stderr", "def")
	result := buffer.read(2)
	chunks := result["chunks"].([]outputChunk)
	if len(chunks) != 2 || chunks[0].Text != "c" || chunks[1].Text != "def" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
	if result["cursor"] != int64(6) {
		t.Fatalf("unexpected cursor: %#v", result["cursor"])
	}
}

func TestBoundedCommandBuffer(t *testing.T) {
	var buffer boundedCommandBuffer
	buffer.limit = 5
	if written, err := buffer.Write([]byte("abcdefgh")); err != nil || written != 8 {
		t.Fatalf("Write returned %d, %v", written, err)
	}
	value, truncated := buffer.result()
	if value != "abcde" || !truncated {
		t.Fatalf("unexpected bounded output %q, truncated=%v", value, truncated)
	}
}

func TestSystemProcessCount(t *testing.T) {
	count, err := systemProcessCount()
	if err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatalf("unexpected system process count: %d", count)
	}
}

func TestLoadConfigFileAndEnvironmentOverride(t *testing.T) {
	for _, name := range []string{
		"ANDROID_NATIVE_CONFIG_FILE", "ANDROID_NATIVE_ALLOWED_ROOTS",
		"MIRA_NODE_HEARTBEAT_SECONDS", "NODE_AGENT_HEARTBEAT_SECONDS", "MIRA_SERVER_URL", "CONTROL_SERVER_URL",
		"MIRA_NODE_TOKEN", "CONTROL_SERVER_TOKEN", "MIRA_NODE_STATE_FILE", "MIRA_IDENTITY_FILE",
		"MIRA_NODE_KEY", "NODE_AGENT_KEY", "ANDROID_NATIVE_PRIVILEGE_MODE", "ANDROID_NATIVE_BRIDGE_URL",
		"ANDROID_NATIVE_BRIDGE_TOKEN", "ANDROID_NATIVE_EXIT_WITH_PARENT",
	} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "node.json")
	contents := `{
		"serverUrl":"https://control.example.test/",
		"token":"file-token",
		"nodeKey":"mira-android:test",
		"allowedRoots":["/tmp"],
		"heartbeatSeconds":7,
		"privilegeMode":"app",
		"bridgeUrl":"http://127.0.0.1:12345",
		"bridgeToken":"bridge-secret",
		"exitWithParent":true
	}`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_SERVER_TOKEN", "environment-token")
	configuration, err := loadConfigArgs([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ServerURL != "https://control.example.test" ||
		configuration.Token != "environment-token" ||
		configuration.NodeKey != "mira-android:test" ||
		configuration.PrivilegeMode != "app" ||
		configuration.BridgeURL != "http://127.0.0.1:12345" ||
		configuration.HeartbeatInterval.Seconds() != 7 {
		t.Fatalf("unexpected config: %#v", configuration)
	}
	if !configuration.ExitWithParent {
		t.Fatal("expected exitWithParent from config file")
	}
}

func TestRejectsNonLoopbackBridge(t *testing.T) {
	for _, name := range []string{
		"ANDROID_NATIVE_CONFIG_FILE", "ANDROID_NATIVE_ALLOWED_ROOTS",
		"MIRA_NODE_HEARTBEAT_SECONDS", "NODE_AGENT_HEARTBEAT_SECONDS", "MIRA_SERVER_URL", "CONTROL_SERVER_URL",
		"MIRA_NODE_TOKEN", "CONTROL_SERVER_TOKEN", "MIRA_NODE_STATE_FILE", "MIRA_IDENTITY_FILE",
		"MIRA_NODE_KEY", "NODE_AGENT_KEY", "ANDROID_NATIVE_PRIVILEGE_MODE", "ANDROID_NATIVE_BRIDGE_URL",
		"ANDROID_NATIVE_BRIDGE_TOKEN", "ANDROID_NATIVE_EXIT_WITH_PARENT",
	} {
		t.Setenv(name, "")
	}
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(`{
		"bridgeUrl":"http://192.0.2.5:12345",
		"bridgeToken":"secret"
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigArgs([]string{"--config", path}); err == nil {
		t.Fatal("expected a non-loopback bridge URL to be rejected")
	}
}

func TestNodeIdentityPersistsCredential(t *testing.T) {
	directory := t.TempDir()
	configuration := config{
		ServerURL:    "https://mira.example.test",
		IdentityFile: filepath.Join(directory, "identity.json"),
	}
	identity := nodeIdentity{NodeKey: "test-node"}
	first, err := loadOrCreateNodeState(configuration, identity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateNodeState(configuration, identity)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == "" || first.CredentialID == "" || first.Token != second.Token {
		t.Fatalf("device identity was not persisted: %#v %#v", first, second)
	}
	info, err := os.Stat(configuration.IdentityFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("node identity state is accessible outside its owner: %o", info.Mode().Perm())
	}
}

func TestNodeIdentityStateRejectsDifferentNode(t *testing.T) {
	configuration := config{
		ServerURL:    "https://mira.example.test",
		IdentityFile: filepath.Join(t.TempDir(), "identity.json"),
	}
	if _, err := loadOrCreateNodeState(configuration, nodeIdentity{NodeKey: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateNodeState(configuration, nodeIdentity{NodeKey: "second"}); err == nil {
		t.Fatal("expected state bound to a different node key to be rejected")
	}
}
