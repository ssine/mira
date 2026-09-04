//go:build linux || android

package node

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
	"time"
)

// Only the isolated SSH worker becomes a subreaper. OpenSSH sessions call setsid;
// killing a process group alone would miss their shells after transport loss.
func prepareOpenSSHWorker() (func(), error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return nil, fmt.Errorf("SSH child supervision: %w", err)
	}
	return func() {
		deadline := time.Now().Add(800 * time.Millisecond)
		for time.Now().Before(deadline) {
			entries, _ := os.ReadDir("/proc/self/task")
			children := map[int]bool{}
			for _, entry := range entries {
				b, _ := os.ReadFile("/proc/self/task/" + entry.Name() + "/children")
				for _, field := range strings.Fields(string(b)) {
					pid, _ := strconv.Atoi(field)
					if pid > 0 {
						children[pid] = true
					}
				}
			}
			if len(children) == 0 {
				return
			}
			for pid := range children {
				_ = unix.Kill(pid, unix.SIGKILL)
			}
			for {
				var status unix.WaitStatus
				pid, _ := unix.Wait4(-1, &status, unix.WNOHANG, nil)
				if pid <= 0 {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}, nil
}
