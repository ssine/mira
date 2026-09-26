package node

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// A zero value selects the automatic, Node-wide residency budget.
type codexMemoryBudget struct {
	bytes   uint64
	percent float64
}

func parseCodexMemoryBudget(raw string) (codexMemoryBudget, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" || value == "auto" {
		return codexMemoryBudget{}, nil
	}
	invalid := fmt.Errorf("codexMemoryBudget must be auto, a capacity such as 2GiB, or a percentage greater than 0 and at most 100")
	if strings.HasSuffix(value, "%") {
		percent, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, "%")), 64)
		if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent <= 0 || percent > 100 {
			return codexMemoryBudget{}, invalid
		}
		return codexMemoryBudget{percent: percent}, nil
	}
	units := []struct {
		suffix     string
		multiplier float64
	}{
		{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
		{"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3}, {"b", 1},
	}
	for _, unit := range units {
		if !strings.HasSuffix(value, unit.suffix) {
			continue
		}
		number, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, unit.suffix)), 64)
		bytes := number * unit.multiplier
		// Keep arithmetic and JSON status exact; an invalid budget never disables reclamation.
		if err != nil || math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes < 1 || bytes > 1<<53 {
			return codexMemoryBudget{}, invalid
		}
		return codexMemoryBudget{bytes: uint64(bytes)}, nil
	}
	return codexMemoryBudget{}, invalid
}

func (budget codexMemoryBudget) resolve(total uint64) uint64 {
	if budget.bytes != 0 {
		return budget.bytes
	}
	if budget.percent != 0 {
		return uint64(float64(total) * budget.percent / 100)
	}
	const fourGiB = uint64(4 << 30)
	if total <= fourGiB {
		return total / 5
	}
	return max(fourGiB/5, total/10)
}
