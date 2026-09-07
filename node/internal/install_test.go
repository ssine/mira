package node

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestReleaseVersionFromChecksumManifest(t *testing.T) {
	hash := strings.Repeat("a", 64)
	manifest := strings.Join([]string{
		"not a checksum line",
		hash + "  install.ps1",
		hash + "  mira_1.2.3_linux_amd64.tar.gz",
		hash + " *mira_1.2.3_windows_amd64.zip",
		hash + "  mira_1.2.3_android_arm64.apk",
	}, "\n")
	version, err := releaseVersionFromChecksumManifest(manifest)
	if err != nil || version != "1.2.3" {
		t.Fatalf("releaseVersionFromChecksumManifest() = %q, %v", version, err)
	}
	if _, err := releaseVersionFromChecksumManifest(hash + "  install.ps1\n"); err == nil {
		t.Fatal("accepted a manifest without Mira release assets")
	}
	conflicting := manifest + "\n" + hash + "  mira_1.2.4_linux_arm64.tar.gz\n"
	if _, err := releaseVersionFromChecksumManifest(conflicting); err == nil {
		t.Fatal("accepted conflicting release versions")
	}
}

func TestSetupPreservesConfigurationAndBinding(t *testing.T) {
	directory := t.TempDir()
	identity := filepath.Join(directory, "identity.json")
	t.Setenv("MIRA_IDENTITY_FILE", identity)
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
	var parsed fileConfig
	if err := json.Unmarshal(after, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.IdentityFile != identity {
		t.Fatalf("configured identity path = %q, want %q", parsed.IdentityFile, identity)
	}
}

func TestSetupStoresExplicitServiceIdentity(t *testing.T) {
	directory := t.TempDir()
	configuration := filepath.Join(directory, "state", "node.json")
	identity := filepath.Join(directory, "state", "node-identity.json")
	if _, err := runSetup([]string{
		"--server", "https://mira.example.test",
		"--config", configuration,
		"--identity", identity,
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(configuration)
	if err != nil {
		t.Fatal(err)
	}
	var parsed fileConfig
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.IdentityFile != identity {
		t.Fatalf("configured identity path = %q, want %q", parsed.IdentityFile, identity)
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
