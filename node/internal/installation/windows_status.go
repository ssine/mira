package installation

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

type windowsServiceState int

const (
	windowsServiceStopped windowsServiceState = iota + 1
	windowsServiceStartPending
	windowsServiceStopPending
	windowsServiceRunning
	windowsServiceContinuePending
	windowsServicePausePending
	windowsServicePaused
)

var windowsServiceStatePattern = regexp.MustCompile(`(?mi)\bSTATE\s*:\s*([1-7])\b`)

func parseWindowsServiceState(output string) (windowsServiceState, error) {
	match := windowsServiceStatePattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("cannot parse sc.exe service state")
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value < int(windowsServiceStopped) || value > int(windowsServicePaused) {
		return 0, fmt.Errorf("invalid sc.exe service state %q", match[1])
	}
	return windowsServiceState(value), nil
}

// waitWindowsServiceStable makes recovery from an interrupted Windows install
// idempotent. Retrying while SCM still reports START_PENDING or STOP_PENDING
// waits for the existing transition instead of issuing a conflicting start.
func waitWindowsServiceStable(ctx context.Context, runner Runner, serviceName string) (windowsServiceState, error) {
	waitContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		output, err := runner.Run(waitContext, "sc.exe", "query", serviceName)
		if err != nil {
			return 0, err
		}
		state, err := parseWindowsServiceState(output)
		if err != nil {
			return 0, err
		}
		switch state {
		case windowsServiceStartPending, windowsServiceStopPending, windowsServiceContinuePending, windowsServicePausePending:
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-waitContext.Done():
				timer.Stop()
				return 0, waitContext.Err()
			case <-timer.C:
			}
		default:
			return state, nil
		}
	}
}
