//go:build !windows

package clauderuntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A descendant that outlives the wrapper (like the CLI saving a rotated OAuth
// token) gets SIGTERM and time to finish; a stuck one is killed at the deadline.
func TestSettleProcessGroupLetsDescendantsFinish(t *testing.T) {
	// Each descendant installs its trap and reports readiness before the
	// wrapper exits, so SIGTERM never races the trap.
	dir := t.TempDir()
	saved, ready := filepath.Join(dir, "saved"), filepath.Join(dir, "ready")
	cmd := exec.Command("sh", "-c", `(trap 'sleep 0.3; touch "$0"; exit 0' TERM; touch "$1"; while :; do sleep 0.05; done) &
while [ ! -e "$1" ]; do sleep 0.01; done; exit 0`, saved, ready)
	configureCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	settleProcessGroup(cmd, 5*time.Second)
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("descendant was killed before finishing its exit path: %v", err)
	}
	if syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH || time.Since(started) > 3*time.Second {
		t.Fatalf("group still alive or settle waited too long (%s)", time.Since(started))
	}

	stuckReady := filepath.Join(dir, "stuck-ready")
	stuck := exec.Command("sh", "-c", `(trap '' TERM; touch "$0"; while :; do sleep 0.05; done) &
while [ ! -e "$0" ]; do sleep 0.01; done; exit 0`, stuckReady)
	configureCommand(stuck)
	if err := stuck.Run(); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	settleProcessGroup(stuck, 300*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(-stuck.Process.Pid, 0) != syscall.ESRCH {
		t.Fatal("a descendant ignoring SIGTERM survived the deadline")
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("deadline not honored: %s", elapsed)
	}
}
