//go:build !windows

package node

import "syscall"

func diskStatus(roots []string) []map[string]any {
	result := make([]map[string]any, 0, len(roots))
	for _, root := range roots {
		var value syscall.Statfs_t
		if err := syscall.Statfs(root, &value); err != nil {
			result = append(result, map[string]any{"path": root, "error": err.Error()})
		} else {
			total := int64(value.Blocks) * int64(value.Bsize)
			available := int64(value.Bavail) * int64(value.Bsize)
			used := total - available
			result = append(result, map[string]any{
				"path": root, "totalBytes": total, "availableBytes": available,
				"usedBytes": used, "usagePercent": usagePercent(used, total),
			})
		}
	}
	return result
}
