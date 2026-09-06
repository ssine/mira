package accountsampler

import (
	"encoding/json"
	"math"
	"strconv"
)

const weeklyWindowMinutes = 7 * 24 * 60

// Quota is the deliberately small subset of an App Server rate-limit response
// that Mira persists. Keeping this projection here prevents credentials or
// upstream error details from entering the history table.
type Quota struct {
	Remaining      *float64
	ResetsAtMillis *int64
	ResetCount     *int64
}

// ProjectWeeklyQuota selects the Codex seven-day window using the same
// precedence as the browser-facing JavaScript implementation.
func ProjectWeeklyQuota(result any) Quota {
	record, _ := result.(map[string]any)
	if record == nil {
		return Quota{}
	}

	var snapshot any
	if byID, ok := record["rateLimitsByLimitId"].(map[string]any); ok {
		if value, exists := byID["codex"]; exists && value != nil {
			snapshot = value
		}
	}
	if snapshot == nil {
		snapshot = record["rateLimits"]
	}
	codex, _ := snapshot.(map[string]any)
	if codex == nil || hasForeignLimitID(codex["limitId"]) {
		codex = nil
	}

	var window map[string]any
	if codex != nil {
		for _, name := range []string{"primary", "secondary"} {
			candidate, _ := codex[name].(map[string]any)
			minutes, ok := finiteNumber(candidate["windowDurationMins"])
			if ok && minutes == weeklyWindowMinutes {
				window = candidate
				break
			}
		}
	}

	quota := Quota{}
	if used, ok := finiteNumber(valueAt(window, "usedPercent")); ok {
		remaining := math.Max(0, math.Min(100, 100-used))
		quota.Remaining = &remaining
	}
	if seconds, ok := finiteNumber(valueAt(window, "resetsAt")); ok && seconds > 0 && seconds < 8.64e12 {
		milliseconds := int64(seconds * 1000)
		quota.ResetsAtMillis = &milliseconds
	}
	if credits, ok := record["rateLimitResetCredits"].(map[string]any); ok {
		if count, ok := safeNonnegativeInteger(credits["availableCount"]); ok {
			quota.ResetCount = &count
		}
	}
	return quota
}

func hasForeignLimitID(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return typed != "" && typed != "codex"
	case bool:
		return typed
	default:
		value, ok := finiteNumber(value)
		return !ok || value != 0
	}
}

func valueAt(record map[string]any, key string) any {
	if record == nil {
		return nil
	}
	return record[key]
}

func finiteNumber(value any) (float64, bool) {
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int8:
		number = float64(typed)
	case int16:
		number = float64(typed)
	case int32:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case uint:
		number = float64(typed)
	case uint8:
		number = float64(typed)
	case uint16:
		number = float64(typed)
	case uint32:
		number = float64(typed)
	case uint64:
		number = float64(typed)
	case json.Number:
		parsed, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func safeNonnegativeInteger(value any) (int64, bool) {
	number, ok := finiteNumber(value)
	if !ok || number < 0 || math.Trunc(number) != number || number > 9_007_199_254_740_991 {
		return 0, false
	}
	return int64(number), true
}
