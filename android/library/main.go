package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const agentVersion = "0.5.0"

func logEvent(message string, fields map[string]any) {
	value := map[string]any{
		"time":    time.Now().UTC().Format(time.RFC3339Nano),
		"message": message,
	}
	for key, field := range fields {
		value[key] = field
	}
	encoded, _ := json.Marshal(value)
	fmt.Println(string(encoded))
}

func run(ctx context.Context) error {
	configuration, err := loadConfig()
	if err != nil {
		return err
	}
	if configuration.ExitWithParent {
		if err := enableParentExitGuard(); err != nil {
			return err
		}
	}
	runtime, err := newCapabilityRuntime(configuration)
	if err != nil {
		return err
	}
	defer runtime.close()

	client := newControlClient(configuration, runtime)
	for ctx.Err() == nil {
		if err := client.register(ctx); err != nil {
			logEvent("Android native node registration failed", map[string]any{"error": err.Error()})
			if !sleepContext(ctx, 3*time.Second) {
				break
			}
			continue
		}
		if err := client.heartbeat(ctx); err != nil {
			logEvent("Android native initial heartbeat failed", map[string]any{"error": err.Error()})
		}
		if err := client.serve(ctx); err != nil && ctx.Err() == nil {
			logEvent("Android native control channel disconnected", map[string]any{"error": err.Error()})
		}
		if !sleepContext(ctx, 2*time.Second) {
			break
		}
	}
	return ctx.Err()
}

func enableParentExitGuard() error {
	parentPID := os.Getppid()
	if parentPID <= 1 {
		return fmt.Errorf("APK parent process already exited")
	}
	const prSetParentDeathSignal = 1
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PRCTL,
		prSetParentDeathSignal,
		// This process can be blocked in a control-channel read, so use SIGKILL
		// instead of relying on graceful signal handling after Android replaces or
		// kills the APK process.
		uintptr(syscall.SIGKILL),
		0,
		0,
		0,
		0,
	)
	if errno != 0 {
		return fmt.Errorf("configure parent-death signal: %w", errno)
	}
	if os.Getppid() != parentPID {
		return fmt.Errorf("APK parent process exited during agent startup")
	}
	// Some Android su implementations reparent the executable without
	// delivering PR_SET_PDEATHSIG. Keep an independent PPID check so an APK
	// upgrade cannot leave a privileged process behind on those devices.
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

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil && err != context.Canceled {
		logEvent("Android native node agent failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	logEvent("Android native node agent stopped", nil)
}
