//go:build aix || android || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnixCurrentIsRelativeTraversableLink(t *testing.T) {
	layout, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bootstrapVersion(t, layout, "1.0.0")
	target, err := os.Readlink(layout.Current)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) || target != filepath.Join("versions", "1.0.0") {
		t.Fatalf("current link target=%q", target)
	}
	executable, err := layout.CurrentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if executable != filepath.Join(layout.Current, "mira") {
		t.Fatalf("Nix cannot use current/mira: %q", executable)
	}
}
