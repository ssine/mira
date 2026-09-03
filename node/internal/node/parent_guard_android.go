//go:build android

package node

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

func enableParentExitGuard() error {
	parentPID := os.Getppid()
	if parentPID <= 1 {
		return fmt.Errorf("APK parent process already exited")
	}
	const prSetParentDeathSignal = 1
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetParentDeathSignal, uintptr(syscall.SIGKILL), 0, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("configure parent-death signal: %w", errno)
	}
	if os.Getppid() != parentPID {
		return fmt.Errorf("APK parent process exited during Mira Node startup")
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if os.Getppid() != parentPID {
				os.Exit(0)
			}
		}
	}()
	return nil
}
