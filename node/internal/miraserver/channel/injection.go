package channel

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

const (
	miraCLIInstructionsBegin       = "MIRA_CLI_INSTRUCTIONS_V1_BEGIN"
	miraCLIInstructionsEnd         = "MIRA_CLI_INSTRUCTIONS_V1_END"
	nodeDeveloperInstructionsBegin = "MIRA_NODE_DEVELOPER_INSTRUCTIONS_V1_BEGIN"
	nodeDeveloperInstructionsEnd   = "MIRA_NODE_DEVELOPER_INSTRUCTIONS_V1_END"
)

var (
	bundledCodexPathPattern = regexp.MustCompile(`(?i)^(.*[\\/])mira-codex-package[\\/]bin[\\/]codex(?:\.exe)?$`)
	miraInstructionsPattern = regexp.MustCompile(`(?s)\n?MIRA_CLI_INSTRUCTIONS_V1_BEGIN.*?MIRA_CLI_INSTRUCTIONS_V1_END\n?`)
	nodeInstructionsPattern = regexp.MustCompile(`(?s)\n?MIRA_NODE_DEVELOPER_INSTRUCTIONS_V1_BEGIN.*?MIRA_NODE_DEVELOPER_INSTRUCTIONS_V1_END\n?`)
)

func ProtocolToken(request *http.Request) (string, bool) {
	for _, protocol := range websocketProtocols(request) {
		if !strings.HasPrefix(protocol, "auth.") {
			continue
		}
		encoded := strings.TrimPrefix(protocol, "auth.")
		if encoded == "" {
			return "", false
		}
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || !utf8.Valid(decoded) {
			return "", false
		}
		return string(decoded), true
	}
	return "", false
}

func websocketProtocols(request *http.Request) []string {
	var result []string
	for _, header := range request.Header.Values("Sec-WebSocket-Protocol") {
		for _, value := range strings.Split(header, ",") {
			if value = strings.TrimSpace(value); value != "" {
				result = append(result, value)
			}
		}
	}
	return result
}

func hasWebSocketProtocol(request *http.Request, expected string) bool {
	for _, protocol := range websocketProtocols(request) {
		if protocol == expected {
			return true
		}
	}
	return false
}

func validNativeAbsolutePath(value, platform string) bool {
	if value == "" || len(utf16.Encode([]rune(value))) > 4096 {
		return false
	}
	for _, character := range value {
		if character <= 0x1f || character == 0x7f {
			return false
		}
	}
	if platform == "windows" {
		if strings.HasPrefix(value, `\\`) {
			return true
		}
		return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
			value[1] == ':' && (value[2] == '/' || value[2] == '\\')
	}
	return strings.HasPrefix(value, "/")
}

func targetMiraCLIPath(target *nodes.Node) string {
	if target == nil {
		return ""
	}
	candidates := []any{target.ReportedAppServer["miraCliPath"], target.MachineStatus["miraCliPath"]}
	if codexPath, ok := target.ReportedAppServer["codexPath"].(string); ok {
		if match := bundledCodexPathPattern.FindStringSubmatch(codexPath); match != nil {
			name := "mira"
			if target.Platform == "windows" {
				name = "mira.exe"
			}
			candidates = append(candidates, match[1]+name)
		}
	}
	for _, candidate := range candidates {
		if path, ok := candidate.(string); ok && validNativeAbsolutePath(path, target.Platform) {
			return path
		}
	}
	return ""
}

func targetDefaultCWD(target *nodes.Node) string {
	if target == nil {
		return ""
	}
	value, _ := target.DesiredAppServer["defaultCwd"].(string)
	if validNativeAbsolutePath(value, target.Platform) {
		return value
	}
	return ""
}

func targetDeveloperInstructionsFile(target *nodes.Node) (string, error) {
	if target == nil {
		return "", nil
	}
	value := target.DesiredAppServer["developerInstructionsFile"]
	if value == nil || value == "" {
		return "", nil
	}
	path, ok := value.(string)
	if ok && validNativeAbsolutePath(path, target.Platform) {
		return path, nil
	}
	return "", channelError("the configured Node Developer instructions file is not a valid native absolute path", 500, "invalid_developer_instructions_file")
}

func shellInvocation(path, platform string) string {
	if platform == "windows" {
		return "& '" + strings.ReplaceAll(path, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
}

func miraCLIInstructions(target *nodes.Node) string {
	path := targetMiraCLIPath(target)
	if path == "" {
		return ""
	}
	executable := shellInvocation(path, target.Platform)
	return strings.Join([]string{
		miraCLIInstructionsBegin,
		"Mira node-to-node SSH access is available from this execution node.",
		"The Mira CLI absolute path is " + jsQuote(path) + ". Invoke that exact path as a normal shell command; do not assume mira is on PATH.",
		"SSH, SCP, and SFTP are CLI-only operations. They are not home_nodes dynamic tools and must not be modeled or invoked as dynamic tools.",
		"List SSH-capable nodes before selecting a target: " + executable + " nodes list --summary --capability ssh --json",
		"Run a remote command: " + executable + " ssh <node-id-or-exact-node-key-or-alias> -- pwd",
		"Open an interactive remote shell: " + executable + " ssh -t <node-id-or-exact-node-key-or-alias>",
		"Upload one regular file: " + executable + " scp <local-path> <node-id>::<absolute-remote-path>",
		"Download one regular file: " + executable + " scp <node-id>::<absolute-remote-path> <local-path>",
		"Copy a directory with native SCP: " + executable + " scp -rp <local-directory> <node-id>::<absolute-remote-directory>",
		"Use SFTP interactively: " + executable + " sftp <node-id>; batch: " + executable + " sftp -b <commands-file> <node-id>. Native SCP/SFTP overwrite semantics apply.",
		"Use a Node ID, exact nodeKey, or exact user-defined alias returned by nodes list; do not guess selectors. Never read or expose the Mira identity credential.",
		miraCLIInstructionsEnd,
	}, "\n")
}

func mergeDeveloperInstructions(existing any, additions ...string) (string, bool) {
	current, _ := existing.(string)
	current = strings.TrimSpace(nodeInstructionsPattern.ReplaceAllString(miraInstructionsPattern.ReplaceAllString(current, "\n"), "\n"))
	sections := make([]string, 0, 1+len(additions))
	if current != "" {
		sections = append(sections, current)
	}
	for _, addition := range additions {
		if addition != "" {
			sections = append(sections, addition)
		}
	}
	if len(sections) == 0 {
		return "", false
	}
	return strings.Join(sections, "\n\n"), true
}

func safeStoreID(value string, supplied bool) (string, bool) {
	if !supplied {
		return "personal", true
	}
	if len(value) < 1 || len(value) > 128 {
		return "", false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._-", character)) {
			return "", false
		}
	}
	return value, true
}

func canonicalJSON(value any) (string, error) {
	var output bytes.Buffer
	if err := writeCanonicalJSON(&output, value); err != nil {
		return "", err
	}
	return output.String(), nil
}

func requestDigest(value any) (string, error) {
	canonical, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:]), nil
}

func writeCanonicalJSON(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		output.WriteString(jsQuote(typed))
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return err
		}
		return writeCanonicalJSON(output, number)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			output.WriteString("null")
			return nil
		}
		if typed == 0 {
			output.WriteByte('0')
			return nil
		}
		encoded, _ := json.Marshal(typed)
		output.Write(encoded)
	case int:
		output.WriteString(strconv.Itoa(typed))
	case int64:
		output.WriteString(strconv.FormatInt(typed, 10))
	case []any:
		output.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonicalJSON(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(left, right int) bool { return compareUTF16(keys[left], keys[right]) < 0 })
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			output.WriteString(jsQuote(key))
			output.WriteByte(':')
			if err := writeCanonicalJSON(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("canonical JSON: %w", err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return err
		}
		return writeCanonicalJSON(output, decoded)
	}
	return nil
}

func compareUTF16(left, right string) int {
	a := utf16.Encode([]rune(left))
	b := utf16.Encode([]rune(right))
	for index := 0; index < len(a) && index < len(b); index++ {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

func jsQuote(value string) string {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	quoted := strings.TrimSuffix(output.String(), "\n")
	quoted = strings.ReplaceAll(quoted, `\u2028`, "\u2028")
	quoted = strings.ReplaceAll(quoted, `\u2029`, "\u2029")
	return quoted
}
