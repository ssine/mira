package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type capabilityRuntime struct {
	roots     []string
	realRoots []string
	mode      string
	bridge    *androidBridge
	processMu sync.Mutex
	processes map[string]*managedProcess
}

type actionParams struct {
	Action string `json:"action"`
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

func newCapabilityRuntime(configuration config) (*capabilityRuntime, error) {
	runtime := &capabilityRuntime{
		roots:     make([]string, 0, len(configuration.AllowedRoots)),
		realRoots: make([]string, 0, len(configuration.AllowedRoots)),
		mode:      configuration.PrivilegeMode,
		processes: make(map[string]*managedProcess),
	}
	if configuration.BridgeURL != "" {
		runtime.bridge = newAndroidBridge(configuration.BridgeURL, configuration.BridgeToken)
	}
	for _, root := range configuration.AllowedRoots {
		cleaned, resolved, err := resolveExistingAncestor(root)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed root %s: %w", root, err)
		}
		runtime.roots = append(runtime.roots, cleaned)
		runtime.realRoots = append(runtime.realRoots, resolved)
	}
	return runtime, nil
}

func (runtime *capabilityRuntime) execute(
	ctx context.Context,
	capability string,
	params json.RawMessage,
) (any, error) {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	switch capability {
	case "status":
		return runtime.machineStatus(ctx)
	case "screen":
		var value screenParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode screen params: %w", err)
		}
		return runtime.screen(ctx, value)
	case "file":
		var value fileParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode file params: %w", err)
		}
		return runtime.file(value)
	case "process":
		var value processParams
		if err := json.Unmarshal(params, &value); err != nil {
			return nil, fmt.Errorf("decode process params: %w", err)
		}
		return runtime.process(ctx, value)
	default:
		return nil, fmt.Errorf("unsupported Android native capability: %s", capability)
	}
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s failed: %s", name, strings.TrimSpace(string(output)))
	}
	return strings.ReplaceAll(string(output), "\r\n", "\n"), nil
}

func getProperty(ctx context.Context, name string) string {
	value, err := commandOutput(ctx, "getprop", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func (runtime *capabilityRuntime) deviceInfo(ctx context.Context) (deviceInfo, error) {
	serial := getProperty(ctx, "ro.serialno")
	if serial == "" {
		serial = getProperty(ctx, "ro.boot.serialno")
	}
	if serial == "" {
		hostname, _ := os.Hostname()
		serial = hostname
	}
	return deviceInfo{
		Serial:       serial,
		Manufacturer: getProperty(ctx, "ro.product.manufacturer"),
		Model:        getProperty(ctx, "ro.product.model"),
		Device:       getProperty(ctx, "ro.product.device"),
		Release:      getProperty(ctx, "ro.build.version.release"),
		SDK:          getProperty(ctx, "ro.build.version.sdk"),
		Fingerprint:  getProperty(ctx, "ro.build.fingerprint"),
		ABI:          getProperty(ctx, "ro.product.cpu.abi"),
	}, nil
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

func (runtime *capabilityRuntime) machineStatus(ctx context.Context) (map[string]any, error) {
	device, err := runtime.deviceInfo(ctx)
	if err != nil {
		return nil, err
	}
	uptimeText := readText("/proc/uptime", 4096)
	uptimeFields := strings.Fields(uptimeText)
	uptime := float64(0)
	if len(uptimeFields) > 0 {
		uptime, _ = strconv.ParseFloat(uptimeFields[0], 64)
	}
	memory := readText("/proc/meminfo", 1024*1024)
	display, _ := runtime.displayInfo(ctx)
	battery, _ := commandOutput(ctx, "dumpsys", "battery")
	focus, _ := commandOutput(ctx, "sh", "-c", "dumpsys window | grep -E 'mCurrentFocus|mFocusedApp' | head -4")
	networks, _ := commandOutput(ctx, "sh", "-c", "ip -brief address 2>/dev/null || ip address")
	disks := make([]map[string]any, 0, len(runtime.realRoots))
	for _, root := range runtime.realRoots {
		var value syscall.Statfs_t
		if err := syscall.Statfs(root, &value); err != nil {
			disks = append(disks, map[string]any{"path": root, "error": err.Error()})
			continue
		}
		disks = append(disks, map[string]any{
			"path":           root,
			"totalBytes":     int64(value.Blocks) * int64(value.Bsize),
			"availableBytes": int64(value.Bavail) * int64(value.Bsize),
		})
	}

	runtime.processMu.Lock()
	managedProcessCount := len(runtime.processes)
	runtime.processMu.Unlock()
	status := map[string]any{
		"sampledAt":     time.Now().UTC().Format(time.RFC3339Nano),
		"hostname":      device.Manufacturer + "-" + device.Model,
		"platform":      "android",
		"release":       device.Release,
		"architecture":  runtimePackageArch(),
		"uptimeSeconds": uptime,
		"memory": map[string]any{
			"totalBytes":     memoryValue(memory, "MemTotal"),
			"freeBytes":      memoryValue(memory, "MemFree"),
			"availableBytes": memoryValue(memory, "MemAvailable"),
		},
		"native": map[string]any{
			"pid": os.Getpid(), "uid": os.Getuid(), "gid": os.Getgid(), "transport": "native",
			"privilegeMode": runtime.mode, "androidBridge": runtime.bridge != nil,
		},
		"device":           device,
		"display":          display,
		"disk":             disks,
		"battery":          strings.TrimSpace(battery),
		"focus":            strings.TrimSpace(focus),
		"networks":         strings.TrimSpace(networks),
		"allowedRoots":     runtime.roots,
		"rootEnabled":      os.Geteuid() == 0,
		"managedProcesses": managedProcessCount,
		"processLimit":     maxProcessCount,
	}
	if runtime.bridge != nil {
		permissions, err := runtime.bridge.screen(ctx, screenParams{Action: "permissions"})
		if err != nil {
			status["androidPermissions"] = map[string]any{"error": err.Error()}
		} else {
			status["androidPermissions"] = permissions
		}
	}
	return status, nil
}

func runtimePackageArch() string {
	return runtime.GOARCH
}

func (runtime *capabilityRuntime) close() {
	runtime.processMu.Lock()
	processes := make([]*managedProcess, 0, len(runtime.processes))
	for _, process := range runtime.processes {
		processes = append(processes, process)
	}
	runtime.processMu.Unlock()
	for _, process := range processes {
		process.mu.Lock()
		running := process.running
		process.mu.Unlock()
		if running && process.command.Process != nil {
			_ = process.command.Process.Signal(syscall.SIGTERM)
		}
	}
}
