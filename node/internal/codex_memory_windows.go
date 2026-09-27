//go:build windows

package node

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var codexGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func codexEffectiveMemory() (uint64, error) {
	status := windowsMemoryStatus{length: uint32(unsafe.Sizeof(windowsMemoryStatus{}))}
	ok, _, err := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		return 0, fmt.Errorf("cannot read system memory: %w", err)
	}
	return status.totalPhysical, nil
}

func codexResidentBytes(pid int) (uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var counters struct {
		Size, PageFaultCount                               uint32
		PeakWorkingSetSize, WorkingSetSize                 uintptr
		QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage       uintptr
		QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage uintptr
		PagefileUsage, PeakPagefileUsage                   uintptr
	}
	counters.Size = uint32(unsafe.Sizeof(counters))
	ok, _, callErr := codexGetProcessMemoryInfo.Call(
		uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
	if ok == 0 {
		return 0, fmt.Errorf("cannot read Codex resident memory: %w", callErr)
	}
	return uint64(counters.WorkingSetSize), nil
}
