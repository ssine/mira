package miraserver

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// losslessJSONValue is used only for equality checks of imported rollout
// records. encoding/json replaces every unpaired UTF-16 surrogate with U+FFFD,
// which would make distinct preserved records compare equal. Strings here are
// represented as their original UTF-16 code units instead.
type losslessJSONValue struct {
	kind   byte
	scalar string
	array  []losslessJSONValue
	object map[string]losslessJSONValue
}

type losslessJSONParser struct {
	raw    []byte
	offset int
}

func parseLosslessJSON(raw json.RawMessage) (losslessJSONValue, error) {
	parser := losslessJSONParser{raw: raw}
	value, err := parser.value()
	if err != nil {
		return losslessJSONValue{}, err
	}
	parser.space()
	if parser.offset != len(parser.raw) {
		return losslessJSONValue{}, fmt.Errorf("trailing JSON data")
	}
	return value, nil
}

func (parser *losslessJSONParser) value() (losslessJSONValue, error) {
	parser.space()
	if parser.offset >= len(parser.raw) {
		return losslessJSONValue{}, fmt.Errorf("unexpected end of JSON")
	}
	switch parser.raw[parser.offset] {
	case '{':
		return parser.objectValue()
	case '[':
		return parser.arrayValue()
	case '"':
		value, err := parser.stringValue()
		return losslessJSONValue{kind: 's', scalar: value}, err
	case 't':
		return parser.literal("true", 't')
	case 'f':
		return parser.literal("false", 'f')
	case 'n':
		return parser.literal("null", '0')
	default:
		return parser.numberValue()
	}
}

func (parser *losslessJSONParser) objectValue() (losslessJSONValue, error) {
	parser.offset++
	result := losslessJSONValue{kind: 'o', object: map[string]losslessJSONValue{}}
	parser.space()
	if parser.take('}') {
		return result, nil
	}
	for {
		parser.space()
		if parser.offset >= len(parser.raw) || parser.raw[parser.offset] != '"' {
			return losslessJSONValue{}, fmt.Errorf("invalid JSON object key")
		}
		key, err := parser.stringValue()
		if err != nil {
			return losslessJSONValue{}, err
		}
		parser.space()
		if !parser.take(':') {
			return losslessJSONValue{}, fmt.Errorf("missing JSON object colon")
		}
		value, err := parser.value()
		if err != nil {
			return losslessJSONValue{}, err
		}
		result.object[key] = value
		parser.space()
		if parser.take('}') {
			return result, nil
		}
		if !parser.take(',') {
			return losslessJSONValue{}, fmt.Errorf("invalid JSON object separator")
		}
	}
}

func (parser *losslessJSONParser) arrayValue() (losslessJSONValue, error) {
	parser.offset++
	result := losslessJSONValue{kind: 'a'}
	parser.space()
	if parser.take(']') {
		return result, nil
	}
	for {
		value, err := parser.value()
		if err != nil {
			return losslessJSONValue{}, err
		}
		result.array = append(result.array, value)
		parser.space()
		if parser.take(']') {
			return result, nil
		}
		if !parser.take(',') {
			return losslessJSONValue{}, fmt.Errorf("invalid JSON array separator")
		}
	}
}

func (parser *losslessJSONParser) stringValue() (string, error) {
	parser.offset++
	units := make([]uint16, 0)
	for parser.offset < len(parser.raw) {
		current := parser.raw[parser.offset]
		if current == '"' {
			parser.offset++
			encoded := make([]byte, len(units)*2)
			for index, unit := range units {
				encoded[index*2], encoded[index*2+1] = byte(unit>>8), byte(unit)
			}
			return string(encoded), nil
		}
		if current == '\\' {
			parser.offset++
			if parser.offset >= len(parser.raw) {
				return "", fmt.Errorf("incomplete JSON escape")
			}
			escaped := parser.raw[parser.offset]
			parser.offset++
			switch escaped {
			case '"', '\\', '/':
				units = append(units, uint16(escaped))
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			case 'n':
				units = append(units, '\n')
			case 'r':
				units = append(units, '\r')
			case 't':
				units = append(units, '\t')
			case 'u':
				if parser.offset+4 > len(parser.raw) {
					return "", fmt.Errorf("incomplete JSON unicode escape")
				}
				var decoded [2]byte
				if _, err := hex.Decode(decoded[:], parser.raw[parser.offset:parser.offset+4]); err != nil {
					return "", fmt.Errorf("invalid JSON unicode escape: %w", err)
				}
				units = append(units, uint16(decoded[0])<<8|uint16(decoded[1]))
				parser.offset += 4
			default:
				return "", fmt.Errorf("invalid JSON escape")
			}
			continue
		}
		if current < 0x20 {
			return "", fmt.Errorf("invalid JSON control character")
		}
		if current < utf8.RuneSelf {
			units = append(units, uint16(current))
			parser.offset++
			continue
		}
		decoded, size := utf8.DecodeRune(parser.raw[parser.offset:])
		if decoded == utf8.RuneError && size == 1 {
			return "", fmt.Errorf("invalid JSON UTF-8")
		}
		units = append(units, utf16.Encode([]rune{decoded})...)
		parser.offset += size
	}
	return "", fmt.Errorf("unterminated JSON string")
}

func (parser *losslessJSONParser) literal(text string, kind byte) (losslessJSONValue, error) {
	if parser.offset+len(text) > len(parser.raw) || string(parser.raw[parser.offset:parser.offset+len(text)]) != text {
		return losslessJSONValue{}, fmt.Errorf("invalid JSON literal")
	}
	parser.offset += len(text)
	return losslessJSONValue{kind: kind}, nil
}

func (parser *losslessJSONParser) numberValue() (losslessJSONValue, error) {
	start := parser.offset
	for parser.offset < len(parser.raw) {
		current := parser.raw[parser.offset]
		if current == ',' || current == ']' || current == '}' || current == ' ' || current == '\t' || current == '\r' || current == '\n' {
			break
		}
		parser.offset++
	}
	if start == parser.offset || !json.Valid(parser.raw[start:parser.offset]) {
		return losslessJSONValue{}, fmt.Errorf("invalid JSON number")
	}
	return losslessJSONValue{kind: 'n', scalar: string(parser.raw[start:parser.offset])}, nil
}

func (parser *losslessJSONParser) space() {
	for parser.offset < len(parser.raw) {
		switch parser.raw[parser.offset] {
		case ' ', '\t', '\r', '\n':
			parser.offset++
		default:
			return
		}
	}
}

func (parser *losslessJSONParser) take(value byte) bool {
	if parser.offset < len(parser.raw) && parser.raw[parser.offset] == value {
		parser.offset++
		return true
	}
	return false
}
