package node

import "runtime"

type cpuSample struct {
	total uint64
	idle  uint64
}

func usagePercent(used int64, total int64) float64 {
	if total <= 0 || used <= 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

func (runtimeValue *capabilityRuntime) cpuStatus() map[string]any {
	result := map[string]any{
		"logicalCount": runtime.NumCPU(),
		"model":        platformCPUModel(),
	}
	if load := platformLoadAverage(); len(load) == 3 {
		result["loadAverage"] = map[string]float64{"one": load[0], "five": load[1], "fifteen": load[2]}
	}
	sample, err := platformCPUSample()
	if err != nil {
		result["sampleError"] = err.Error()
		return result
	}
	runtimeValue.resourceMu.Lock()
	previous, hasPrevious := runtimeValue.lastCPU, runtimeValue.hasLastCPU
	runtimeValue.lastCPU, runtimeValue.hasLastCPU = sample, true
	runtimeValue.resourceMu.Unlock()
	if hasPrevious && sample.total > previous.total {
		totalDelta := sample.total - previous.total
		idleDelta := uint64(0)
		if sample.idle > previous.idle {
			idleDelta = sample.idle - previous.idle
		}
		if idleDelta > totalDelta {
			idleDelta = totalDelta
		}
		result["usagePercent"] = float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
	}
	return result
}

func memoryStatus(total int64, available int64, free int64) map[string]any {
	used := total - available
	if used < 0 {
		used = 0
	}
	return map[string]any{
		"totalBytes": total, "availableBytes": available, "freeBytes": free,
		"usedBytes": used, "usagePercent": usagePercent(used, total),
	}
}
