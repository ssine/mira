package node

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCLIHelpIsLocalAndSuccessful(t *testing.T) {
	t.Setenv("MIRA_IDENTITY_FILE", t.TempDir()+"/missing-identity.json")
	for _, args := range [][]string{{"--help"}, {"help", "nodes"}, {"nodes", "list", "--help"}, {"ssh", "--help"}} {
		var stdout, stderr bytes.Buffer
		if exit := RunCLI(context.Background(), args, nil, &stdout, &stderr); exit != 0 {
			t.Fatalf("RunCLI(%q) exit = %d, stderr = %q", args, exit, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
			t.Fatalf("RunCLI(%q) stdout = %q, stderr = %q", args, stdout.String(), stderr.String())
		}
	}
}

func TestNodeSummaryAndAliasMatching(t *testing.T) {
	node := map[string]any{
		"nodeId": "id", "nodeKey": "key", "hostname": "host", "displayName": "软路由",
		"aliases": []any{"软路由", "OpenWrt"}, "labels": map[string]any{"role": "router"},
		"status": "online", "platform": "linux", "architecture": "amd64", "nodeMode": "linux",
		"capabilities":      map[string]any{"files": true, "screen": false, "ssh": true},
		"reportedAppServer": map[string]any{"status": "running"}, "machineStatus": map[string]any{"large": true},
	}
	if !nodeHasAlias(node, "openwrt") {
		t.Fatal("alias matching should be case-insensitive")
	}
	summary := summarizeNode(node)
	if _, ok := summary["machineStatus"]; ok {
		t.Fatal("summary retained detailed machine status")
	}
	capabilities := summary["capabilities"].(map[string]any)
	if capabilities["ssh"] != true || capabilities["files"] != true || capabilities["screen"] != nil {
		t.Fatalf("unexpected summary capabilities: %#v", capabilities)
	}
}
