package node

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const nodeVersion = "0.8.0"

func Log(message string, fields map[string]any) {
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

func Run(ctx context.Context) error {
	configuration, err := loadConfig()
	if err != nil {
		return err
	}
	if configuration.ExitWithParent {
		if err := enableParentExitGuard(); err != nil {
			return err
		}
	}
	capabilities, err := newCapabilityRuntime(configuration)
	if err != nil {
		return err
	}
	defer capabilities.close()

	client := newControlClient(configuration, capabilities)
	defer client.close()
	for ctx.Err() == nil {
		if err := client.register(ctx); err != nil {
			Log("Mira Node registration failed", map[string]any{"error": err.Error()})
			if !sleepContext(ctx, 3*time.Second) {
				break
			}
			continue
		}
		if err := client.heartbeat(ctx); err != nil {
			Log("initial heartbeat failed", map[string]any{"error": err.Error()})
		}
		if err := client.reconcile(ctx); err != nil {
			Log("initial App Server reconciliation failed", map[string]any{"error": err.Error()})
		}
		if err := client.serve(ctx); err != nil && ctx.Err() == nil {
			Log("reverse channel disconnected", map[string]any{"error": err.Error()})
		}
		if !sleepContext(ctx, 2*time.Second) {
			break
		}
	}
	return ctx.Err()
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
