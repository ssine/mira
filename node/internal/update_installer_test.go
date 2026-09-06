package node

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestEffectiveUpdateTargetReconcilesInterruptedHandoff(t *testing.T) {
	tests := []struct {
		name                       string
		requested, target, current string
		recorded, wantTarget       string
		wantSettled                bool
	}{
		{name: "settled current", requested: "2.0.0", target: "2.0.0", current: "2.0.0", recorded: "2.0.0", wantTarget: "2.0.0", wantSettled: true},
		{name: "same version needs finalize", requested: "2.0.0", target: "2.0.0", current: "2.0.0", recorded: "1.0.0", wantTarget: "2.0.0"},
		{name: "older latest does not downgrade", requested: "latest", target: "1.0.0", current: "2.0.0", recorded: "2.0.0", wantTarget: "2.0.0", wantSettled: true},
		{name: "older latest still finalizes", requested: "latest", target: "1.0.0", current: "2.0.0", recorded: "1.0.0", wantTarget: "2.0.0"},
		{name: "explicit downgrade remains explicit", requested: "1.0.0", target: "1.0.0", current: "2.0.0", recorded: "2.0.0", wantTarget: "1.0.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, settled := effectiveUpdateTarget(test.requested, test.target, test.current, test.recorded)
			if target != test.wantTarget || settled != test.wantSettled {
				t.Fatalf("effectiveUpdateTarget() = (%q, %v), want (%q, %v)", target, settled, test.wantTarget, test.wantSettled)
			}
		})
	}
}

func TestDownloadReleaseMatchedInstaller(t *testing.T) {
	for _, name := range []string{"install.sh", "install.ps1"} {
		for _, mode := range []string{"valid", "corrupt", "missing", "duplicate", "oversized", "http-error"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				content := "release-specific installer"
				digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "http-error" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					if r.URL.Path == "/SHA256SUMS" {
						if mode == "missing" {
							return
						}
						fmt.Fprintf(w, "%s  %s\r\n", digest, name)
						if mode == "duplicate" {
							fmt.Fprintf(w, "%s  %s\n", digest, name)
						}
						return
					}
					if r.URL.Path != "/"+name {
						t.Errorf("unexpected asset %s", r.URL.Path)
					}
					switch mode {
					case "corrupt":
						fmt.Fprint(w, "modified")
					case "oversized":
						fmt.Fprint(w, strings.Repeat("x", 1024*1024+1))
					default:
						fmt.Fprint(w, content)
					}
				}))
				defer server.Close()
				directory := t.TempDir()
				file, err := downloadUpdateInstaller(context.Background(), server.URL, directory, name)
				if mode == "valid" {
					if err != nil {
						t.Fatal(err)
					}
					actual, err := os.ReadFile(file)
					if err != nil || string(actual) != content {
						t.Fatalf("wrong downloaded installer: %v", err)
					}
				} else {
					if err == nil {
						t.Fatal("accepted invalid installer")
					}
					entries, _ := os.ReadDir(directory)
					if len(entries) != 0 {
						t.Fatal("failed download left an executable candidate")
					}
				}
			})
		}
	}
}
