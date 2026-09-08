package installation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUserRuntimeDirectoryFallback(t *testing.T) {
	for _, name := range []string{"missing environment", "empty environment", "explicit environment", "system scope", "other command", "missing directory", "unsafe permissions", "symlink", "wrong owner"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			uid := os.Geteuid()
			if name == "wrong owner" {
				uid++
			}
			directory := filepath.Join(root, strconv.Itoa(uid))
			if name != "missing directory" {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "unsafe permissions" {
				if err := os.Chmod(directory, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if name == "symlink" {
				if err := os.Remove(directory); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), directory); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command("/usr/bin/systemctl", "--user", "show", "mira.service")
			command.Env = []string{"PATH=/usr/bin", "DBUS_SESSION_BUS_ADDRESS=unix:path=/explicit/bus"}
			want := ""
			switch name {
			case "missing environment", "empty environment":
				want = directory
				if name == "empty environment" {
					command.Env = append(command.Env, "XDG_RUNTIME_DIR=")
				}
			case "explicit environment":
				command.Env = append(command.Env, "XDG_RUNTIME_DIR=/explicit/runtime")
				want = "/explicit/runtime"
			case "system scope":
				command.Args = []string{"systemctl", "show", "mira.service"}
			case "other command":
				command.Path = "/usr/bin/other"
			}
			configureUserRuntimeDirectory(command, root, uid)
			got := ""
			for _, entry := range command.Environ() {
				if strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") {
					got = strings.TrimPrefix(entry, "XDG_RUNTIME_DIR=")
				}
			}
			if got != want {
				t.Fatalf("runtime directory = %q, want %q", got, want)
			}
			if !strings.Contains(strings.Join(command.Environ(), "\n"), "DBUS_SESSION_BUS_ADDRESS=unix:path=/explicit/bus") {
				t.Fatal("changed explicit bus address")
			}
		})
	}
}
