package foundation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreviewIngressConfigAndOriginMatching(t *testing.T) {
	env := map[string]string{"MIRA_NODE_PREVIEW_DOMAIN": "legacy.example.test", "MIRA_NODE_PREVIEW_INGRESSES": `[
		{"consoleOrigin":"https://CONSOLE.example.test:443/","previewOrigin":"https://preview.example.test:443"},
		{"consoleOrigin":"https://direct.example.test:24443","previewOrigin":"https://preview.direct.example.test:34443"}
	]`}
	config, err := ConfigFromLookup(func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ console, preview string }{
		{"https://console.example.test", "https://preview.example.test"},
		{"https://CONSOLE.example.test.:443", "https://preview.example.test"},
		{"https://direct.example.test:24443", "https://preview.direct.example.test:34443"},
	} {
		ingress, ok := config.PreviewIngressForOrigin(test.console)
		if !ok || ingress.PreviewOrigin != test.preview {
			t.Fatalf("%s: %#v, %v", test.console, ingress, ok)
		}
	}
	for _, origin := range []string{"https://unknown.example.test", "https://direct.example.test", "http://console.example.test", "https://console.example.test/path"} {
		if _, ok := config.PreviewIngressForOrigin(origin); ok {
			t.Fatal("unconfigured origin accepted", origin)
		}
	}
}

func TestPreviewIngressRejectsInvalidDeploymentConfig(t *testing.T) {
	valid := PreviewIngress{"https://console.example.test", "https://preview.example.test"}
	for _, bad := range []string{"not json", "null", "[]", "{}", strings.Repeat(" ", 64*1024+1)} {
		if _, err := parsePreviewIngresses(bad, true, ""); err == nil {
			t.Fatal("invalid mapping array accepted")
		}
	}
	for _, bad := range []string{"https://user:secret@preview.example.test", "https://preview.example.test/path", "https://preview.example.test?query", "https://preview.example.test#", "https://*.preview.example.test", "https://localhost", "https://127.0.0.1", "https://preview.example.test:0", "https://preview.example.test:65536", "https://preview.example.test:", "http://preview.example.test", "https://console.example.test"} {
		entry := valid
		entry.PreviewOrigin = bad
		raw, _ := json.Marshal([]PreviewIngress{entry})
		if _, err := parsePreviewIngresses(string(raw), true, ""); err == nil {
			t.Fatal("invalid preview origin accepted", bad)
		}
	}
	for _, entries := range [][]PreviewIngress{
		{valid, {"https://console.example.test:443", "https://other.example.test"}},
		{valid, {"https://console.preview.example.test", "https://other.example.test"}},
		make([]PreviewIngress, 17),
	} {
		raw, _ := json.Marshal(entries)
		if _, err := parsePreviewIngresses(string(raw), true, ""); err == nil {
			t.Fatal("invalid mapping combination accepted")
		}
	}
	if _, err := parsePreviewIngresses(`[{"consoleOrigin":"http://localhost:8787","previewOrigin":"http://preview.example.test:8788"}]`, false, ""); err != nil {
		t.Fatal("HTTP development mapping rejected", err)
	}
	if _, err := parsePreviewIngresses(`[{"consoleOrigin":"https://site.legacy.example.test","previewOrigin":"https://preview.example.test"}]`, true, "legacy.example.test"); err == nil {
		t.Fatal("legacy preview suffix captured a console host")
	}
}
