package miraserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
)

func object(value any) map[string]any {
	if result, ok := value.(map[string]any); ok && result != nil {
		return result
	}
	return map[string]any{}
}

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return map[string]any{}, nil
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return value, nil
}

func decodeArray(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(destination)
}

// encoding/json sorts map keys, giving Mira the same canonical object ordering
// that the former Server used for operation and payload fingerprints.
func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func jsonEqual(left, right any) bool {
	return reflect.DeepEqual(normalizeJSON(left), normalizeJSON(right))
}

func normalizeJSON(value any) any {
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&normalized) != nil {
		return value
	}
	return normalized
}

func digestJSON(value any) (string, error) {
	payload, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func cloneObject(value map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("clone JSON object: %w", err)
	}
	return result, nil
}
