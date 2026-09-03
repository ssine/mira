package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReleaseVersionComparison(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"0.9.0", "0.9.0", 0}, {"0.10.0", "0.9.999", 1},
		{"1.0.0", "0.99.99", 1}, {"0.9.1", "0.10.0", -1},
	} {
		if got := compareReleaseVersions(test.left, test.right); got != test.want {
			t.Fatalf("%s vs %s = %d", test.left, test.right, got)
		}
	}
	for _, value := range []string{"01.2.3", "1..3", "1.2.3/other", "1.2.3-beta"} {
		if releaseVersionPattern.MatchString(value) {
			t.Fatalf("accepted invalid release %q", value)
		}
	}
}

func TestSetupPreservesConfigurationAndBinding(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("MIRA_IDENTITY_FILE", filepath.Join(directory, "identity.json"))
	arguments := []string{"--server", "https://mira.example.test"}
	if _, err := runSetup(arguments); err != nil {
		t.Fatal(err)
	}
	configuration := filepath.Join(directory, "node.json")
	before, err := os.ReadFile(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runSetup(arguments); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetup([]string{"--server", "https://other.example.test"}); err == nil {
		t.Fatal("rebound existing configuration")
	}
	after, err := os.ReadFile(configuration)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("setup modified existing configuration")
	}
}

func TestUpdateVersionFlagIsNotGlobalVersionCommand(t *testing.T) {
	_, arguments, err := parseGlobalCLI([]string{"update", "--version", "0.9.0"})
	if err != nil || !reflect.DeepEqual(arguments, []string{"update", "--version", "0.9.0"}) {
		t.Fatalf("unexpected update arguments: %v %v", arguments, err)
	}
	_, arguments, err = parseGlobalCLI([]string{"--version"})
	if err != nil || !reflect.DeepEqual(arguments, []string{"version"}) {
		t.Fatalf("unexpected version arguments: %v %v", arguments, err)
	}
}

func TestOutputPreservesSplitUTF8(t *testing.T) {
	var buffer outputBuffer
	writer := streamWriter{buffer: &buffer, stream: "stdout"}
	expected := "Mira 你好 🪐\x1b[0m"
	for _, value := range []byte(expected) {
		_, _ = writer.Write([]byte{value})
	}
	buffer.flush()
	var output bytes.Buffer
	for _, chunk := range buffer.read(0)["chunks"].([]outputChunk) {
		output.WriteString(chunk.Text)
	}
	if output.String() != expected {
		t.Fatalf("UTF-8 corrupted: %q", output.String())
	}
}

func TestUpdatePreflightRequiresKnownIdleState(t *testing.T) {
	for _, test := range []struct {
		name, status, appServer, want string
		busy                          bool
		sshSessions                   int
	}{
		{"offline", "offline", "stopped", "offline", false, 0},
		{"app-server", "online", "running", "App Server is active", false, 0},
		{"process", "online", "stopped", "active process", true, 0},
		{"ssh", "online", "stopped", "active SSH", false, 1},
		{"idle", "online", "stopped", "", false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var nodeID string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "application/json")
				if r.URL.Path == "/v1/nodes" {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"nodeId": nodeID, "nodeKey": "preflight-test", "status": test.status}}})
					return
				}
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"status": test.status, "sshSessionCount": test.sshSessions, "reportedAppServer": map[string]any{"status": test.appServer}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"processes": []map[string]any{{"running": test.busy}}, "sessions": []any{}}})
			}))
			defer server.Close()
			identity := filepath.Join(t.TempDir(), "identity.json")
			state, err := loadOrCreateNodeState(config{ServerURL: server.URL, IdentityFile: identity}, nodeIdentity{NodeKey: "preflight-test"})
			if err != nil {
				t.Fatal(err)
			}
			state.NodeID, _ = randomUUID()
			nodeID = state.NodeID
			state.Enrollment.Status = "approved"
			if err := state.save(identity); err != nil {
				t.Fatal(err)
			}
			err = updatePreflight(context.Background(), cliOptions{Identity: identity, Timeout: time.Second})
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("wanted %q, got %v", test.want, err)
			}
		})
	}
}
