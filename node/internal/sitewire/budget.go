package sitewire

import (
	"fmt"
	"strconv"
)

const DefaultStreamBudget = 128
const MaxStreamBudget = 65536

// ParseStreamBudget shares the configurable resource budget between Server and
// Node workers. The Server budget is shared across all serving Nodes and sites.
func ParseStreamBudget(raw string) (int, error) {
	if raw == "" {
		return DefaultStreamBudget, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > MaxStreamBudget {
		return 0, fmt.Errorf("site stream budget must be an integer between 1 and %d", MaxStreamBudget)
	}
	return value, nil
}
