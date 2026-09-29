package nodes

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalizeMetadata(t *testing.T) {
	value, err := normalizedMetadata(map[string]any{
		"expectedRevision": json.Number("7"),
		"displayName":      "  Ｍira 节点  ",
		"aliases":          []any{"Zulu", "ａｌｐｈａ"},
		"labels":           map[string]any{" Role ": "  Router  ", "site": "家"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.ExpectedRevision != 7 || value.DisplayName == nil || *value.DisplayName != "Mira 节点" {
		t.Fatalf("unexpected normalized metadata: %#v", value)
	}
	if got := []string{value.Aliases[0].Value, value.Aliases[1].Value}; !reflect.DeepEqual(got, []string{"alpha", "Zulu"}) {
		t.Fatalf("aliases = %#v", got)
	}
	if !reflect.DeepEqual(value.Labels, map[string]string{"role": "Router", "site": "家"}) {
		t.Fatalf("labels = %#v", value.Labels)
	}

	_, err = normalizedMetadata(map[string]any{
		"expectedRevision": float64(0), "displayName": nil,
		"aliases": []any{"MIRA", "ｍｉｒａ"}, "labels": map[string]any{},
	})
	if err == nil || err.Error() != "duplicate alias: mira" {
		t.Fatalf("duplicate alias error = %v", err)
	}
	_, err = normalizedMetadata(map[string]any{
		"expectedRevision": float64(0), "displayName": nil,
		"aliases": []any{"bad:alias"}, "labels": map[string]any{},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "aliases must be") {
		t.Fatalf("invalid alias error = %v", err)
	}
}

func TestMetadataUsesJavaScriptLengthRules(t *testing.T) {
	if _, err := requiredString("value", strings.Repeat("😀", 32), 64); err != nil {
		t.Fatal(err)
	}
	if _, err := requiredString("value", strings.Repeat("😀", 33), 64); err == nil {
		t.Fatal("33 astral characters should occupy 66 JavaScript UTF-16 units")
	}
	if _, ok := integer(float64(9_007_199_254_740_992)); ok {
		t.Fatal("Number.isSafeInteger boundary was not enforced")
	}
}

func TestDesiredStateValidation(t *testing.T) {
	service := &Service{now: func() int64 { return 123456789 }}
	desired, err := service.normalizedDesiredState(map[string]any{
		"running": true, "configOverrides": []any{"model=fast"}, "defaultCwd": `C:\\work`,
		"developerInstructionsFile": `C:\\work\\AGENTS.md`, "codexPath": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if desired.JSON["revision"] != int64(123456789) || desired.JSON["codexPath"] != nil {
		t.Fatalf("desired state = %#v", desired.JSON)
	}
	if desired.DefaultCwd.Value == nil || !nativeAbsolutePath(*desired.DefaultCwd.Value, "windows") {
		t.Fatal("Windows path was not retained")
	}
	for _, body := range []map[string]any{
		{"running": "yes"},
		{"running": true, "defaultCwd": "relative/path"},
		{"running": true, "configOverrides": []any{"api_key=leak"}},
		{"running": true, "configOverrides": []any{strings.Repeat("x", 2049)}},
	} {
		if _, err := service.normalizedDesiredState(body); err == nil {
			t.Fatalf("invalid desired state accepted: %#v", body)
		}
	}
}

func TestDescriptorDefaultsAndBuildVersion(t *testing.T) {
	body := map[string]any{
		"nodeKey": "node", "hostname": "host", "platform": "linux",
		"architecture": "amd64", "nodeMode": "linux", "agentVersion": "0.13.5",
	}
	value, err := normalizeDescriptor(body)
	if err != nil {
		t.Fatal(err)
	}
	if value.NodeVersion != "0.13.5" || !reflect.DeepEqual(value.DefaultDesiredAppServer, map[string]any{"running": false}) || len(value.CodexInstallations) != 0 {
		t.Fatalf("descriptor defaults = %#v", value)
	}
	body["nodeBuild"] = map[string]any{"version": "other"}
	if _, err := normalizeDescriptor(body); err == nil || err.Error() != "nodeBuild.version must match nodeVersion" {
		t.Fatalf("build mismatch error = %v", err)
	}
}

func TestEnrollmentTokenAndHash(t *testing.T) {
	secret := make([]byte, 32)
	for index := range secret {
		secret[index] = byte(index)
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	credentialID := "12345678-1234-4123-8123-123456789abc"
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer mira_node_"+credentialID+"_"+encoded)
	token, ok := requestNodeToken(request)
	if !ok || token.CredentialID != credentialID {
		t.Fatalf("parsed token = %#v, %v", token, ok)
	}
	digest := sha256.Sum256(secret)
	wantHash := hex.EncodeToString(digest[:])
	if token.SecretHash != wantHash || !constantTimeHashEqual(token.SecretHash, wantHash) {
		t.Fatalf("secret hash = %q, want %q", token.SecretHash, wantHash)
	}
	if fingerprint(wantHash) != wantHash[0:4]+"-"+wantHash[4:8]+"-"+wantHash[8:12]+"-"+wantHash[12:16] {
		t.Fatal("credential fingerprint changed")
	}
	request.Header.Set("Authorization", "bearer mira_node_"+credentialID+"_"+encoded)
	if _, ok := requestNodeToken(request); ok {
		t.Fatal("lowercase Bearer scheme was accepted")
	}
}

func TestEnrollmentAndNodeViews(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 11, 12, 987654321, time.FixedZone("test", 8*60*60))
	address := "192.0.2.4"
	view := enrollmentView(enrollmentRow{
		EnrollmentID: "enrollment", CredentialID: "credential", CredentialFingerprint: "fingerprint",
		VerificationCode: "123456", NodeKey: "node", Hostname: "host", Platform: "linux",
		Architecture: "amd64", NodeMode: "linux", NodeVersion: "1", NodeBuild: map[string]any{},
		Capabilities: map[string]any{}, CodexInstallations: []any{}, MachineStatus: map[string]any{},
		Status: "pending", RequestedAt: now, ExpiresAt: now.Add(15 * time.Minute), RequestedFrom: &address,
	}, false)
	if _, exists := view["requestedFrom"]; exists {
		t.Fatal("public enrollment view exposed the request address")
	}
	if view["requestedAt"] != "2026-09-06T02:11:12.987Z" || view["nodeId"] != nil {
		t.Fatalf("enrollment view = %#v", view)
	}
	adminView := enrollmentView(enrollmentRow{RequestedFrom: &address, RequestedAt: now, ExpiresAt: now}, true)
	if adminView["requestedFrom"] != address {
		t.Fatal("administrator enrollment view omitted request address")
	}

	summary := Summary(Node{
		NodeID: "id", NodeKey: "key", Hostname: "host", Platform: "linux", Architecture: "amd64",
		NodeMode: "linux", Capabilities: map[string]any{"files": true, "process": false, "appServer": true},
		ReportedAppServer: map[string]any{"status": "running"}, Aliases: []string{}, Labels: map[string]any{},
	})
	if !reflect.DeepEqual(summary["capabilities"], map[string]any{"files": true, "appServer": true}) || summary["appServerStatus"] != "running" {
		t.Fatalf("Node summary = %#v", summary)
	}
}

func TestTruncateNoteUsesUTF16Limit(t *testing.T) {
	note := strings.Repeat("x", 1999) + "😀tail"
	truncated := truncateNote(note)
	if truncated == nil || *truncated != strings.Repeat("x", 1999)+"�" {
		t.Fatalf("truncated note suffix = %q", (*truncated)[len(*truncated)-4:])
	}
}
