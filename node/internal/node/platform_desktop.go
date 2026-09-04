//go:build !android

package node

import (
	"context"
	"net"
	"os"
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

func detectNodeMode() string {
	if runtime.GOOS != "linux" {
		return runtime.GOOS
	}
	release := readText("/proc/sys/kernel/osrelease", 16*1024)
	if strings.Contains(strings.ToLower(release), "microsoft") || os.Getenv("WSL_DISTRO_NAME") != "" {
		return "wsl"
	}
	return "linux"
}

func (runtimeValue *capabilityRuntime) identity(context.Context) nodeIdentity {
	hostname, _ := os.Hostname()
	mode := detectNodeMode()
	nodeKey := runtimeValue.configuration.NodeKey
	if nodeKey == "" {
		nodeKey = hostname + ":" + runtime.GOOS + ":" + mode + ":" + runtime.GOARCH
	}
	return nodeIdentity{NodeKey: nodeKey, Hostname: hostname, Platform: runtime.GOOS, Architecture: runtime.GOARCH, Mode: mode}
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

func networkStatus() map[string]any {
	result := map[string]any{}
	interfaces, _ := net.Interfaces()
	for _, networkInterface := range interfaces {
		addresses, _ := networkInterface.Addrs()
		values := make([]string, 0, len(addresses))
		for _, address := range addresses {
			values = append(values, address.String())
		}
		result[networkInterface.Name] = map[string]any{"addresses": values, "hardwareAddress": networkInterface.HardwareAddr.String(), "flags": networkInterface.Flags.String()}
	}
	return result
}

func (runtimeValue *capabilityRuntime) machineStatus(ctx context.Context) (map[string]any, error) {
	identity := runtimeValue.identity(ctx)
	memory := platformMemoryStatus()
	uptime := platformUptimeSeconds()
	runtimeValue.processMu.Lock()
	managedProcesses := len(runtimeValue.processes)
	runtimeValue.processMu.Unlock()
	runtimeValue.ptyMu.Lock()
	ptySessions := len(runtimeValue.ptys)
	runtimeValue.ptyMu.Unlock()
	processCount, _ := systemProcessCount()
	return map[string]any{
		"sampledAt": time.Now().UTC().Format(time.RFC3339Nano), "hostname": identity.Hostname,
		"platform": runtime.GOOS, "release": platformRelease(),
		"architecture": runtime.GOARCH, "uptimeSeconds": uptime, "cpuCount": runtime.NumCPU(),
		"cpu": runtimeValue.cpuStatus(), "memory": memory,
		"disk": diskStatus(runtimeValue.realRoots), "networks": networkStatus(), "allowedRoots": runtimeValue.roots,
		"processCount": processCount, "managedProcesses": managedProcesses, "ptySessions": ptySessions, "sessionLimit": maxProcessCount,
		"ptyBackend": ptyBackendName(), "miraCliPath": localMiraCLIPath(),
	}, nil
}

func (runtimeValue *capabilityRuntime) advertisedCapabilities(context.Context) map[string]any {
	return map[string]any{
		"appServer": true, "shell": true, "files": true, "processes": true, "pty": true,
		"codexSessions": true,
		"ssh":           true, "sshProtocolVersion": 1, "sshFeatures": []string{"exec", "shell", "pty", "sftp"},
		"screen": false, "input": false, "reverseChannel": true, "nativePaths": true,
		"rootAvailable": currentUserIsRoot(), "nodeMode": detectNodeMode(), "transport": "native",
	}
}
