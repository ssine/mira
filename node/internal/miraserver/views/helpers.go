package views

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
)

var safeStorePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func array(value any) []any {
	switch values := value.(type) {
	case []any:
		return values
	case []map[string]any:
		result := make([]any, len(values))
		for index := range values {
			result[index] = values[index]
		}
		return result
	case []string:
		result := make([]any, len(values))
		for index := range values {
			result[index] = values[index]
		}
		return result
	default:
		return nil
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, !math.IsNaN(value) && !math.IsInf(value, 0)
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		return parsed, err == nil && !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
	default:
		return 0, false
	}
}

func safeInteger(value any) (int64, bool) {
	number, ok := number(value)
	if !ok || math.Trunc(number) != number || math.Abs(number) > 9_007_199_254_740_991 {
		return 0, false
	}
	return int64(number), true
}

func decodeJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(destination)
}

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var result map[string]any
	if err := decodeJSON(raw, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func scanJSON(row pgx.Row) (map[string]any, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func optionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatTime(*value)
	return &formatted
}

func milliseconds(value time.Time) int64 { return value.UnixMilli() }

func first(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	var result map[string]any
	_ = decodeJSON(payload, &result)
	return result
}

func jsUTF16Length(value string) int { return len(utf16.Encode([]rune(value))) }

func jsSlice(value string, end int) string {
	units := utf16.Encode([]rune(value))
	if end < 0 {
		end = 0
	}
	if end > len(units) {
		end = len(units)
	}
	return string(utf16.Decode(units[:end]))
}

func postgresTextInt(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse PostgreSQL integer %q: %w", value, err)
	}
	return parsed, nil
}

func includes(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	if value != "" && !includes(values, value) {
		return append(values, value)
	}
	return values
}

func normalizedType(value any) string {
	return strings.ToLower(strings.ReplaceAll(fmt.Sprint(value), "_", ""))
}
