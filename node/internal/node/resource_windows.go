//go:build windows

package node

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

type windowsFiletime struct {
	low  uint32
	high uint32
}

func (value windowsFiletime) ticks() uint64 {
	return uint64(value.high)<<32 | uint64(value.low)
}

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	ntdll                = syscall.NewLazyDLL("ntdll.dll")
	getSystemTimes       = kernel32.NewProc("GetSystemTimes")
	globalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	getTickCount64       = kernel32.NewProc("GetTickCount64")
	rtlGetVersion        = ntdll.NewProc("RtlGetVersion")
)

func platformCPUSample() (cpuSample, error) {
	var idle, kernel, user windowsFiletime
	ok, _, callError := getSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)),
	)
	if ok == 0 {
		return cpuSample{}, fmt.Errorf("GetSystemTimes failed: %w", callError)
	}
	return cpuSample{total: kernel.ticks() + user.ticks(), idle: idle.ticks()}, nil
}

func platformLoadAverage() []float64 { return nil }

func platformCPUModel() string { return os.Getenv("PROCESSOR_IDENTIFIER") }

func platformUptimeSeconds() float64 {
	value, _, _ := getTickCount64.Call()
	return float64(value) / 1000
}

type windowsVersionInfo struct {
	size        uint32
	major       uint32
	minor       uint32
	build       uint32
	platformID  uint32
	servicePack [128]uint16
}

func platformRelease() string {
	version := windowsVersionInfo{size: uint32(unsafe.Sizeof(windowsVersionInfo{}))}
	status, _, _ := rtlGetVersion.Call(uintptr(unsafe.Pointer(&version)))
	if status != 0 {
		return os.Getenv("OS")
	}
	return fmt.Sprintf("%d.%d build %d", version.major, version.minor, version.build)
}

type windowsMemoryStatus struct {
	length                   uint32
	memoryLoad               uint32
	totalPhysical            uint64
	availablePhysical        uint64
	totalPageFile            uint64
	availablePageFile        uint64
	totalVirtual             uint64
	availableVirtual         uint64
	availableExtendedVirtual uint64
}

func platformMemoryStatus() map[string]any {
	status := windowsMemoryStatus{length: uint32(unsafe.Sizeof(windowsMemoryStatus{}))}
	ok, _, callError := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		return map[string]any{"error": fmt.Sprintf("GlobalMemoryStatusEx failed: %v", callError)}
	}
	return memoryStatus(int64(status.totalPhysical), int64(status.availablePhysical), int64(status.availablePhysical))
}
