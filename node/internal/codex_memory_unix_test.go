//go:build !windows

package node

import "testing"

func TestEffectiveCgroupMemory(t *testing.T) {
	for _, test := range []struct {
		name, groups, mounts string
		files                map[string]string
		expected             uint64
	}{
		{"nested v2", "0::/parent/child", "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw", map[string]string{
			"/sys/fs/cgroup/parent/child/memory.max": "max", "/sys/fs/cgroup/parent/memory.max": "2147483648", "/sys/fs/cgroup/memory.max": "4294967296"}, 2 << 30},
		{"v1 mount root", "5:cpu:/elsewhere\n6:memory:/docker/abc", "1 0 0:1 /docker /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory", map[string]string{
			"/sys/fs/cgroup/memory/abc/memory.limit_in_bytes": "1073741824", "/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712"}, 1 << 30},
		{"escaped mount", "0::/child", `1 0 0:1 / /sys/fs/cgroup\040space rw - cgroup2 cgroup rw`, map[string]string{
			"/sys/fs/cgroup space/child/memory.max": "536870912"}, 512 << 20},
		{"no controller", "", "", nil, 16 << 30},
		{"unlimited", "0::/", "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw", map[string]string{
			"/sys/fs/cgroup/memory.max": "max"}, 16 << 30},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := effectiveCgroupMemory(16<<30, test.groups, test.mounts, func(path string) string { return test.files[path] })
			if got != test.expected {
				t.Fatalf("got %d want %d", got, test.expected)
			}
		})
	}
}

func TestCodexMemorySample(t *testing.T) {
	total, err := codexEffectiveMemory()
	if err != nil || total == 0 {
		t.Fatalf("system sample: %d %v", total, err)
	}
	if _, err := codexResidentBytes(-1); err == nil {
		t.Fatal("invalid PID accepted")
	}
}
