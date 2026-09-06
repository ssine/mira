package imports

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func rawObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value map[string]json.RawMessage
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	return value, nil
}

func decodeValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON value")
	}
	return value, nil
}

func marshalRawObject(value map[string]json.RawMessage) (json.RawMessage, error) {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := []byte{'{'}
	for index, key := range keys {
		if index > 0 {
			result = append(result, ',')
		}
		encodedKey, _ := json.Marshal(key)
		result = append(result, encodedKey...)
		result = append(result, ':')
		if !json.Valid(value[key]) {
			return nil, fmt.Errorf("invalid raw JSON field %q", key)
		}
		result = append(result, value[key]...)
	}
	result = append(result, '}')
	return result, nil
}

func cloneMap(value map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return decodeObject(encoded)
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func array(value any) []any {
	result, _ := value.([]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func integer(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		return parsed, err == nil && parsed >= -9_007_199_254_740_991 && parsed <= 9_007_199_254_740_991
	case float64:
		parsed := int64(number)
		return parsed, float64(parsed) == number && parsed >= -9_007_199_254_740_991 && parsed <= 9_007_199_254_740_991
	case int64:
		return number, number >= -9_007_199_254_740_991 && number <= 9_007_199_254_740_991
	case int:
		return integer(int64(number))
	default:
		return 0, false
	}
}

func stableJSON(value any) ([]byte, error) {
	switch current := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer := []byte{'{'}
		for index, key := range keys {
			if index > 0 {
				buffer = append(buffer, ',')
			}
			encodedKey, _ := json.Marshal(key)
			buffer = append(buffer, encodedKey...)
			buffer = append(buffer, ':')
			encodedValue, err := stableJSON(current[key])
			if err != nil {
				return nil, err
			}
			buffer = append(buffer, encodedValue...)
		}
		return append(buffer, '}'), nil
	case []any:
		buffer := []byte{'['}
		for index, item := range current {
			if index > 0 {
				buffer = append(buffer, ',')
			}
			encoded, err := stableJSON(item)
			if err != nil {
				return nil, err
			}
			buffer = append(buffer, encoded...)
		}
		return append(buffer, ']'), nil
	case json.RawMessage:
		if !json.Valid(current) {
			return nil, fmt.Errorf("invalid raw JSON")
		}
		return current, nil
	default:
		return json.Marshal(current)
	}
}

func jsonEqual(left, right any) bool {
	a, errA := stableJSON(left)
	b, errB := stableJSON(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func rawString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range strings.ToLower(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}
