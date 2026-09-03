//go:build windows

package node

import (
	"fmt"
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")

func diskStatus(roots []string) []map[string]any {
	result := make([]map[string]any, 0, len(roots))
	for _, root := range roots {
		rootPointer, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			result = append(result, map[string]any{"path": root, "error": err.Error()})
			continue
		}
		var available, total, free uint64
		ok, _, callError := getDiskFreeSpaceEx.Call(
			uintptr(unsafe.Pointer(rootPointer)), uintptr(unsafe.Pointer(&available)),
			uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)),
		)
		if ok == 0 {
			result = append(result, map[string]any{"path": root, "error": fmt.Sprintf("GetDiskFreeSpaceExW failed: %v", callError)})
			continue
		}
		used := int64(total - available)
		result = append(result, map[string]any{
			"path": root, "totalBytes": int64(total), "availableBytes": int64(available),
			"usedBytes": used, "usagePercent": usagePercent(used, int64(total)),
		})
	}
	return result
}
