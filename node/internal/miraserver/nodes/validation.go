package nodes

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"golang.org/x/text/cases"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

const enrollmentLifetimeMinutes = 15

var (
	credentialIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	secretHashPattern   = regexp.MustCompile(`(?i)^[0-9a-f]{64}$`)
	aliasPattern        = regexp.MustCompile(`^[\p{L}\p{N}](?:[\p{L}\p{N}._-]*[\p{L}\p{N}])?$`)
	labelKeyPattern     = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)
	secretOverride      = regexp.MustCompile(`(?i)(?:bearer_token|access_token|password|secret|api_key)\s*=`)
	enUSLower           = cases.Lower(language.AmericanEnglish)
)

func unixMilliseconds() int64 { return time.Now().UnixMilli() }

func secureVerificationCode() (string, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", value.Int64()), nil
}

func jsStringLength(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func requiredString(name string, value any, maximum int) (string, error) {
	text, ok := value.(string)
	if !ok || text == "" || jsStringLength(text) > maximum {
		return "", fmt.Errorf("%s must be a non-empty string of at most %d characters", name, maximum)
	}
	return text, nil
}

func object(value any, fallback map[string]any) map[string]any {
	if result, ok := value.(map[string]any); ok && result != nil {
		return result
	}
	if fallback != nil {
		return fallback
	}
	return map[string]any{}
}

func array(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = typed[index]
		}
		return result, true
	default:
		return nil, false
	}
}

func containsControl(value string, includeC1 bool) bool {
	for _, codepoint := range value {
		if codepoint <= 0x1f || codepoint == 0x7f || (includeC1 && codepoint >= 0x80 && codepoint <= 0x9f) {
			return true
		}
	}
	return false
}

// NormalizeAliasKey matches String.normalize("NFKC").toLocaleLowerCase("en-US").
func NormalizeAliasKey(value string) string {
	return enUSLower.String(norm.NFKC.String(value))
}

type alias struct {
	Value string
	Key   string
}

type metadata struct {
	DisplayName      *string
	Aliases          []alias
	Labels           map[string]string
	ExpectedRevision int64
}

func normalizedMetadata(body map[string]any) (metadata, error) {
	rawRevision, exists := body["expectedRevision"]
	revision, ok := integer(rawRevision)
	if !exists || !ok || revision < 0 {
		return metadata{}, errors.New("expectedRevision must be a non-negative integer")
	}

	var displayName *string
	if raw, exists := body["displayName"]; exists && raw != nil {
		value, ok := raw.(string)
		if !ok {
			return metadata{}, errors.New("displayName must be a string or null")
		}
		value = norm.NFKC.String(strings.TrimSpace(value))
		if value != "" {
			if utf8.RuneCountInString(value) > 80 || containsControl(value, true) {
				return metadata{}, errors.New("displayName must contain at most 80 characters and no control characters")
			}
			displayName = &value
		}
	}

	rawAliases, ok := array(body["aliases"])
	if !ok || len(rawAliases) > 8 {
		return metadata{}, errors.New("aliases must be an array containing at most 8 values")
	}
	aliases := make([]alias, 0, len(rawAliases))
	aliasKeys := map[string]bool{}
	for _, raw := range rawAliases {
		value, ok := raw.(string)
		if !ok {
			return metadata{}, errors.New("each alias must be a string")
		}
		value = norm.NFKC.String(strings.TrimSpace(value))
		if utf8.RuneCountInString(value) > 48 || !aliasPattern.MatchString(value) {
			return metadata{}, errors.New("aliases must be 1-48 letters, numbers, dots, underscores or hyphens and must start and end with a letter or number")
		}
		key := NormalizeAliasKey(value)
		if aliasKeys[key] {
			return metadata{}, fmt.Errorf("duplicate alias: %s", value)
		}
		aliasKeys[key] = true
		aliases = append(aliases, alias{Value: value, Key: key})
	}
	aliasCollator := collate.New(language.AmericanEnglish)
	sort.SliceStable(aliases, func(left, right int) bool {
		return aliasCollator.CompareString(aliases[left].Key, aliases[right].Key) < 0
	})

	rawLabels, ok := body["labels"].(map[string]any)
	if !ok || rawLabels == nil || len(rawLabels) > 32 {
		return metadata{}, errors.New("labels must be an object containing at most 32 values")
	}
	labels := make(map[string]string, len(rawLabels))
	for rawKey, rawValue := range rawLabels {
		key := strings.TrimSpace(enUSLower.String(rawKey))
		value, stringValue := rawValue.(string)
		if !labelKeyPattern.MatchString(key) || !stringValue {
			return metadata{}, errors.New("label keys must use 1-32 lowercase letters, numbers, dots, underscores or hyphens and values must be strings")
		}
		value = norm.NFKC.String(strings.TrimSpace(value))
		if utf8.RuneCountInString(value) == 0 || utf8.RuneCountInString(value) > 64 || containsControl(value, true) {
			return metadata{}, fmt.Errorf("label %s must contain 1-64 characters and no control characters", key)
		}
		if _, duplicate := labels[key]; duplicate {
			return metadata{}, fmt.Errorf("duplicate label key: %s", key)
		}
		labels[key] = value
	}
	return metadata{DisplayName: displayName, Aliases: aliases, Labels: labels, ExpectedRevision: revision}, nil
}

func integer(value any) (int64, bool) {
	const maximumSafeInteger = int64(9_007_199_254_740_991)
	valid := func(number int64) (int64, bool) {
		return number, number >= -maximumSafeInteger && number <= maximumSafeInteger
	}
	switch number := value.(type) {
	case int:
		return valid(int64(number))
	case int64:
		return valid(number)
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || math.Abs(number) > float64(maximumSafeInteger) {
			return 0, false
		}
		return int64(number), true
	case json.Number:
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil {
			return 0, false
		}
		return valid(parsed)
	default:
		return 0, false
	}
}

type optionalPath struct {
	Present bool
	Value   *string
}

func optionalAbsolutePath(name string, body map[string]any) (optionalPath, error) {
	raw, present := body[name]
	if !present {
		return optionalPath{}, nil
	}
	if raw == nil || raw == "" {
		return optionalPath{Present: true}, nil
	}
	value, ok := raw.(string)
	if !ok || jsStringLength(value) > 4096 || containsControl(value, false) {
		return optionalPath{}, fmt.Errorf("%s must be null or an absolute path of at most 4096 characters", name)
	}
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "/") && !windowsAbsolute(value) {
		return optionalPath{}, fmt.Errorf("%s must be an absolute Unix or Windows path", name)
	}
	return optionalPath{Present: true, Value: &value}, nil
}

func windowsAbsolute(value string) bool {
	if strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func nativeAbsolutePath(value string, platform string) bool {
	if platform == "windows" {
		return windowsAbsolute(value)
	}
	return strings.HasPrefix(value, "/")
}

func requestNodeToken(request *http.Request) (foundation.ParsedNodeToken, bool) {
	if request == nil {
		return foundation.ParsedNodeToken{}, false
	}
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return foundation.ParsedNodeToken{}, false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	if token == "" || strings.IndexFunc(token, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }) >= 0 {
		return foundation.ParsedNodeToken{}, false
	}
	return foundation.ParseNodeToken(token)
}

func constantTimeHashEqual(left string, right string) bool {
	leftBytes, leftError := hex.DecodeString(left)
	rightBytes, rightError := hex.DecodeString(right)
	if leftError != nil || rightError != nil || len(leftBytes) != 32 || len(rightBytes) != 32 {
		return false
	}
	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}

func fingerprint(secretHash string) string {
	prefix := secretHash[:16]
	return strings.Join([]string{prefix[:4], prefix[4:8], prefix[8:12], prefix[12:16]}, "-")
}
