//go:build android

package node

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type nodeIdentity struct {
	NodeKey      string
	Hostname     string
	Platform     string
	Architecture string
	Mode         string
}

type deviceInfo struct {
	Serial       string `json:"serial"`
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Device       string `json:"device"`
	Release      string `json:"release"`
	SDK          string `json:"sdk"`
	Fingerprint  string `json:"fingerprint"`
	ABI          string `json:"abi"`
}

func getProperty(ctx context.Context, name string) string {
	value, err := commandOutput(ctx, "getprop", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func (runtimeValue *capabilityRuntime) deviceInfo(ctx context.Context) deviceInfo {
	serial := getProperty(ctx, "ro.serialno")
	if serial == "" {
		serial = getProperty(ctx, "ro.boot.serialno")
	}
	if serial == "" {
		serial, _ = os.Hostname()
	}
	return deviceInfo{
		Serial: serial, Manufacturer: getProperty(ctx, "ro.product.manufacturer"),
		Model: getProperty(ctx, "ro.product.model"), Device: getProperty(ctx, "ro.product.device"),
		Release: getProperty(ctx, "ro.build.version.release"), SDK: getProperty(ctx, "ro.build.version.sdk"),
		Fingerprint: getProperty(ctx, "ro.build.fingerprint"), ABI: getProperty(ctx, "ro.product.cpu.abi"),
	}
}

func (runtimeValue *capabilityRuntime) identity(ctx context.Context) nodeIdentity {
	device := runtimeValue.deviceInfo(ctx)
	hostname := strings.Trim(device.Manufacturer+"-"+device.Model, "-")
	if hostname == "" {
		hostname = "android-" + device.Serial
	}
	nodeKey := runtimeValue.configuration.NodeKey
	if nodeKey == "" {
		nodeKey = "android:" + device.Serial
	}
	return nodeIdentity{NodeKey: nodeKey, Hostname: hostname, Platform: "android", Architecture: runtime.GOARCH, Mode: "android"}
}

func readText(path string, maximum int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	buffer := make([]byte, maximum)
	count, _ := file.Read(buffer)
	return string(buffer[:count])
}

func memoryValue(memory string, name string) int64 {
	for _, line := range strings.Split(memory, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimSuffix(fields[0], ":") == name {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value * 1024
		}
	}
	return 0
}

func (runtimeValue *capabilityRuntime) machineStatus(ctx context.Context) (map[string]any, error) {
	device := runtimeValue.deviceInfo(ctx)
	uptime := platformUptimeSeconds()
	memory := platformMemoryStatus()
	display, _ := runtimeValue.displayInfo(ctx)
	networks, _ := commandOutput(ctx, "sh", "-c", "ip -brief address 2>/dev/null || ip address")
	runtimeValue.processMu.Lock()
	managedProcessCount := len(runtimeValue.processes)
	runtimeValue.processMu.Unlock()
	processCount, _ := systemProcessCount()
	status := map[string]any{
		"sampledAt": time.Now().UTC().Format(time.RFC3339Nano), "hostname": runtimeValue.identity(ctx).Hostname,
		"platform": "android", "release": device.Release, "architecture": runtime.GOARCH,
		"uptimeSeconds": uptime, "cpuCount": runtime.NumCPU(), "cpu": runtimeValue.cpuStatus(), "memory": memory,
		"ssh":    openSSHRuntime(),
		"native": map[string]any{"pid": os.Getpid(), "uid": os.Getuid(), "gid": os.Getgid(), "transport": "native", "privilegeMode": runtimeValue.configuration.PrivilegeMode, "androidBridge": runtimeValue.bridge != nil},
		"device": device, "display": display,
		"networks": strings.TrimSpace(networks), "allowedRoots": runtimeValue.roots,
		"rootEnabled": os.Geteuid() == 0, "processCount": processCount, "managedProcesses": managedProcessCount, "processLimit": maxProcessCount,
	}
	// Use the APK's app-accessible APIs in both modes. An unprivileged Node
	// must not probe inaccessible battery/window Binder services, even when
	// dumpsys exits successfully with an error message in stdout.
	if runtimeValue.bridge != nil {
		telemetry, err := runtimeValue.bridge.call(ctx, "/v1/status", map[string]any{})
		if err != nil {
			status["telemetryErrors"] = map[string]any{"androidBridge": err.Error()}
		} else if fields, ok := telemetry.(map[string]any); ok {
			for _, field := range []string{"battery", "focus", "disk", "telemetryErrors"} {
				if value, exists := fields[field]; exists {
					status[field] = value
				}
			}
		}
		permissions, err := runtimeValue.bridge.screen(ctx, screenParams{Action: "permissions"})
		if err != nil {
			status["androidPermissions"] = map[string]any{"error": err.Error()}
		} else {
			if fields, ok := permissions.(map[string]any); ok {
				fields["root"] = os.Geteuid() == 0
				if os.Geteuid() == 0 {
					fields["screenCapture"] = "ready"
				}
			}
			status["androidPermissions"] = permissions
		}
	} else if os.Geteuid() == 0 {
		if battery, err := commandOutputLimitedStatus(ctx, "dumpsys", "battery"); err == nil {
			status["battery"] = battery
		}
		if windows, err := commandOutputLimitedStatus(ctx, "dumpsys", "window"); err == nil {
			var focus []string
			for _, line := range strings.Split(windows, "\n") {
				if strings.Contains(line, "mCurrentFocus") || strings.Contains(line, "mFocusedApp") {
					focus = append(focus, strings.TrimSpace(line))
					if len(focus) == 4 {
						break
					}
				}
			}
			status["focus"] = strings.Join(focus, "\n")
		}
	} else {
		status["telemetryErrors"] = map[string]any{"battery": "Android app bridge unavailable", "focus": "Android app bridge unavailable"}
	}
	if _, exists := status["disk"]; !exists {
		storage := "/data"
		if filepath.IsAbs(runtimeValue.configuration.IdentityFile) {
			storage = filepath.Dir(runtimeValue.configuration.IdentityFile)
		}
		status["disk"] = diskStatus([]string{storage})
	}
	return status, nil
}

func commandOutputLimitedStatus(ctx context.Context, name string, args ...string) (string, error) {
	output, _, err := commandOutputLimited(ctx, 64*1024, name, args...)
	return strings.TrimSpace(output), err
}

func (runtimeValue *capabilityRuntime) advertisedCapabilities(context.Context) map[string]any {
	rootEnabled := os.Geteuid() == 0
	bridgeEnabled := runtimeValue.bridge != nil
	return map[string]any{
		"appServer": false, "shell": false, "files": true, "processes": true, "pty": false,
		"filePreviewV1":    true,
		"portSitesV1":      true,
		"fileChunkedWrite": true,
		"codexSessions":    false,
		"ssh":              BundledOpenSSH == "true", "sshProtocolVersion": 1, "sshFeatures": []string{"exec", "shell", "pty", "sftp"},
		"screen": rootEnabled || bridgeEnabled, "input": rootEnabled || bridgeEnabled,
		"reverseChannel": true, "nativePaths": true, "rootAvailable": rootEnabled,
		"nodeMode": "android", "transport": "native", "privilegeMode": runtimeValue.configuration.PrivilegeMode,
		"screenBackend":   map[bool]string{true: "android-api", false: "system"}[bridgeEnabled && !rootEnabled],
		"permissionModel": map[bool]string{true: "android-user-grants", false: "root-provider"}[!rootEnabled],
	}
}

func systemProcessList(ctx context.Context) (any, error) {
	const maximum = 1024 * 1024
	output, truncated, err := commandOutputLimited(ctx, maximum, "ps", "-A", "-o", "PID,PPID,USER,STAT,NAME,ARGS")
	if err != nil {
		return nil, err
	}
	return map[string]any{"format": "android-ps", "output": output, "truncated": truncated, "maxBytes": maximum}, nil
}
