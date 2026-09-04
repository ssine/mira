//go:build !windows

package node

import (
	"fmt"
	"strconv"
	"strings"
)

func platformCPUSample() (cpuSample, error) {
	line, _, _ := strings.Cut(readText("/proc/stat", 16*1024), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("/proc/stat does not contain aggregate CPU times")
	}
	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, fmt.Errorf("parse /proc/stat: %w", err)
		}
		values = append(values, value)
	}
	total := uint64(0)
	// guest and guest_nice are already included in user/nice by Linux, so only
	// sum through steal to avoid double-counting virtual CPU time.
	for index, value := range values {
		if index >= 8 {
			break
		}
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuSample{total: total, idle: idle}, nil
}

func platformLoadAverage() []float64 {
	fields := strings.Fields(readText("/proc/loadavg", 4096))
	if len(fields) < 3 {
		return nil
	}
	result := make([]float64, 3)
	for index := range result {
		value, err := strconv.ParseFloat(fields[index], 64)
		if err != nil {
			return nil
		}
		result[index] = value
	}
	return result
}

func platformCPUModel() string {
	for _, line := range strings.Split(readText("/proc/cpuinfo", 1024*1024), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "Hardware" || strings.TrimSpace(key) == "Processor") {
			if model := strings.TrimSpace(value); model != "" {
				return model
			}
		}
	}
	return ""
}

func platformMemoryStatus() map[string]any {
	memory := readText("/proc/meminfo", 1024*1024)
	return memoryStatus(
		memoryValue(memory, "MemTotal"),
		memoryValue(memory, "MemAvailable"),
		memoryValue(memory, "MemFree"),
	)
}

func platformUptimeSeconds() float64 {
	fields := strings.Fields(readText("/proc/uptime", 4096))
	if len(fields) == 0 {
		return 0
	}
	value, _ := strconv.ParseFloat(fields[0], 64)
	return value
}

func platformRelease() string {
	return strings.TrimSpace(readText("/proc/sys/kernel/osrelease", 16*1024))
}
