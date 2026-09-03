//go:build windows

package node

import (
	"context"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessInfo struct {
	PID     uint32 `json:"pid"`
	PPID    uint32 `json:"ppid"`
	Threads uint32 `json:"threads"`
	Command string `json:"command"`
}

func windowsProcesses(ctx context.Context) ([]windowsProcessInfo, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("snapshot Windows processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	result := make([]windowsProcessInfo, 0, 256)
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result = append(result, windowsProcessInfo{
			PID: entry.ProcessID, PPID: entry.ParentProcessID, Threads: entry.Threads,
			Command: windows.UTF16ToString(entry.ExeFile[:]),
		})
		if len(result) > 65536 {
			return nil, fmt.Errorf("Windows process snapshot exceeds 65536 entries")
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return nil, fmt.Errorf("enumerate Windows processes: %w", err)
	}
	return result, nil
}

func systemProcessCount() (int, error) {
	processes, err := windowsProcesses(context.Background())
	return len(processes), err
}

func systemProcessList(ctx context.Context) (any, error) {
	processes, err := windowsProcesses(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"format": "windows-processes", "processes": processes, "truncated": false}, nil
}
