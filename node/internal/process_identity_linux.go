//go:build linux

package node

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Directory-backed NSS lookups may involve LDAP or another remote service. Keep
// this diagnostic deliberately infrequent; the SSH authentication path never
// waits for it and a failed lookup only suppresses the warning.
const identityGroupCheckInterval = 10 * time.Minute

func normalizedGroupIDs(values []int) []int {
	seen := make(map[int]bool, len(values))
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value < 0 || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func parseGroupIDs(value string) ([]int, bool) {
	fields := strings.Fields(value)
	groups := make([]int, 0, len(fields))
	for _, field := range fields {
		parsed, err := strconv.ParseUint(field, 10, 32)
		if err != nil {
			return nil, false
		}
		groups = append(groups, int(parsed))
	}
	if len(groups) == 0 {
		return nil, false
	}
	return normalizedGroupIDs(groups), true
}

func equalGroupIDs(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func identityIDProgram() string {
	for _, candidate := range []string{"/usr/bin/id", "/bin/id", "/run/current-system/sw/bin/id"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func currentProcessIdentityStatus() map[string]any {
	groups, err := os.Getgroups()
	if err != nil {
		groups = nil
	}
	effective := normalizedGroupIDs(append(groups, os.Getgid()))
	status := map[string]any{
		"uid": os.Getuid(), "gid": os.Getgid(), "effectiveGids": effective,
		"directoryGroupCheck": "unavailable",
	}
	return status
}

func sampleDirectoryGroupStatus(ctx context.Context) map[string]any {
	status := map[string]any{"directoryGroupCheck": "unavailable"}
	username, err := openSSHUsername()
	program := identityIDProgram()
	if err != nil || program == "" {
		return status
	}
	output, truncated, err := commandOutputLimited(ctx, 8*1024, program, "-G", username)
	if err != nil || truncated {
		return status
	}
	directory, ok := parseGroupIDs(output)
	if !ok {
		return status
	}
	status["directoryGroupCheck"] = "ok"
	status["directoryGids"] = directory
	return status
}

func (runtimeValue *capabilityRuntime) processIdentityStatus(context.Context) map[string]any {
	status := currentProcessIdentityStatus()
	runtimeValue.identityMu.Lock()
	if runtimeValue.identityState != nil {
		status["directoryGroupCheck"] = runtimeValue.identityState["directoryGroupCheck"]
		if directory, ok := runtimeValue.identityState["directoryGids"].([]int); ok {
			status["directoryGids"] = append([]int(nil), directory...)
			status["directoryGroupsDiffer"] = !equalGroupIDs(status["effectiveGids"].([]int), directory)
		}
	}
	if !runtimeValue.identityBusy && (runtimeValue.identityAt.IsZero() || time.Since(runtimeValue.identityAt) >= identityGroupCheckInterval) {
		runtimeValue.identityBusy = true
		runtimeValue.identityAt = time.Now()
		go runtimeValue.refreshDirectoryGroupStatus()
	}
	runtimeValue.identityMu.Unlock()
	return status
}

func (runtimeValue *capabilityRuntime) refreshDirectoryGroupStatus() {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	state := sampleDirectoryGroupStatus(ctx)
	runtimeValue.identityMu.Lock()
	runtimeValue.identityState = state
	runtimeValue.identityAt = time.Now()
	runtimeValue.identityBusy = false
	runtimeValue.identityMu.Unlock()
}
