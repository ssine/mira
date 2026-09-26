//go:build !windows

package node

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func codexResidentBytes(pid int) (uint64, error) {
	value := memoryValue(readText(fmt.Sprintf("/proc/%d/status", pid), 64*1024), "VmRSS")
	if value <= 0 {
		return 0, fmt.Errorf("cannot read Codex resident memory")
	}
	return uint64(value), nil
}

func codexEffectiveMemory() (uint64, error) {
	total := memoryValue(readText("/proc/meminfo", 64*1024), "MemTotal")
	if total <= 0 {
		return 0, fmt.Errorf("cannot read system memory")
	}
	return effectiveCgroupMemory(uint64(total), readText("/proc/self/cgroup", 64*1024),
		readText("/proc/self/mountinfo", 256*1024), func(path string) string { return readText(path, 128) }), nil
}

// Account for nested cgroup v1/v2 limits, including an ancestor's tighter limit.
func effectiveCgroupMemory(total uint64, cgroups, mounts string, read func(string) string) uint64 {
	for _, group := range strings.Split(cgroups, "\n") {
		parts := strings.SplitN(group, ":", 3)
		if len(parts) != 3 {
			continue
		}
		v2 := parts[0] == "0" && parts[1] == ""
		if !v2 && !strings.Contains(","+parts[1]+",", ",memory,") {
			continue
		}
		for _, mount := range strings.Split(mounts, "\n") {
			left, right, ok := strings.Cut(mount, " - ")
			fields, kind := strings.Fields(left), strings.Fields(right)
			if !ok || len(fields) < 5 || len(kind) < 3 {
				continue
			}
			if v2 && kind[0] != "cgroup2" || !v2 && (kind[0] != "cgroup" || !strings.Contains(","+kind[2]+",", ",memory,")) {
				continue
			}
			decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
			root, mountpoint := decode.Replace(fields[3]), decode.Replace(fields[4])
			rel, err := filepath.Rel(root, parts[2])
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				continue
			}
			for path := filepath.Join(mountpoint, rel); ; path = filepath.Dir(path) {
				file := "memory.max"
				if !v2 {
					file = "memory.limit_in_bytes"
				}
				if limit, err := strconv.ParseUint(strings.TrimSpace(read(filepath.Join(path, file))), 10, 64); err == nil && limit > 0 {
					total = min(total, limit)
				}
				if path == mountpoint || filepath.Dir(path) == path {
					break
				}
			}
		}
	}
	return total
}
