package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Local `mira codex`/`mira claude` launches receive the Developer instructions
// that managed runtimes on this Node get: the Mira CLI block and the Node's
// configured Developer instructions file. Mira Server renders both, so the text
// stays identical; everything in it needs the Server, so an offline launch
// injects nothing.
const launcherInstructionsTimeout = 3 * time.Second

var launcherInstructionBlocks = regexp.MustCompile(`(?s)\n?MIRA_(CLI|NODE_DEVELOPER)_INSTRUCTIONS_V1_BEGIN.*?MIRA_(CLI|NODE_DEVELOPER)_INSTRUCTIONS_V1_END\n?`)

func fetchLauncherInstructions(ctx context.Context, client *cliClient, stderr io.Writer) string {
	if client == nil || client.identity == nil || client.identity.NodeID == "" {
		return ""
	}
	requestCtx, cancel := context.WithTimeout(ctx, launcherInstructionsTimeout)
	defer cancel()
	var result struct {
		CLIInstructions            string  `json:"cliInstructions"`
		DeveloperInstructions      string  `json:"developerInstructions"`
		DeveloperInstructionsError *string `json:"developerInstructionsError"`
	}
	if err := client.request(requestCtx, http.MethodGet, "/v1/nodes/"+url.PathEscape(client.identity.NodeID)+"/launcher-instructions", nil, &result); err != nil {
		fmt.Fprintf(stderr, "mira: Mira instructions unavailable (%v); launching without them\n", client.explainMissingRoute(ctx, err, "launcher instructions", "1.0.131"))
		return ""
	}
	if result.DeveloperInstructionsError != nil {
		fmt.Fprintf(stderr, "mira: Node Developer instructions file skipped: %s\n", *result.DeveloperInstructionsError)
	}
	return joinInstructions(result.CLIInstructions, result.DeveloperInstructions)
}

// joinInstructions joins non-empty sections after removing earlier Mira blocks.
func joinInstructions(sections ...string) string {
	parts := []string{}
	for _, section := range sections {
		if section = strings.TrimSpace(section); section != "" {
			parts = append(parts, section)
		}
	}
	return strings.Join(parts, "\n\n")
}

func withoutMiraBlocks(text string) string {
	return strings.TrimSpace(launcherInstructionBlocks.ReplaceAllString(text, "\n"))
}

// codexDeveloperInstructions merges Mira's block with the developer
// instructions the launch would otherwise use: an account override, else the
// Codex home's config.toml. It returns the merged -c override and the account
// overrides without the replaced entry. Explicit user -c arguments still win.
func codexDeveloperInstructions(mira, codexHome string, overrides []string) (string, []string) {
	if mira == "" {
		return "", overrides
	}
	existing, found := "", false
	remaining := make([]string, 0, len(overrides))
	for _, override := range overrides {
		if key, _, ok := strings.Cut(override, "="); ok && strings.TrimSpace(key) == "developer_instructions" {
			var parsed struct {
				Value string `toml:"developer_instructions"`
			}
			if toml.Unmarshal([]byte(override), &parsed) == nil {
				existing, found = parsed.Value, true
				continue
			}
		}
		remaining = append(remaining, override)
	}
	if !found {
		if raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil {
			var parsed struct {
				Value string `toml:"developer_instructions"`
			}
			if toml.Unmarshal(raw, &parsed) == nil {
				existing = parsed.Value
			}
		}
	}
	merged := joinInstructions(withoutMiraBlocks(existing), mira)
	// JSON string escapes are valid TOML basic-string escapes.
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(merged)
	return "developer_instructions=" + strings.TrimSpace(encoded.String()), remaining
}

// claudeSystemPromptArguments appends Mira's block with --append-system-prompt,
// merging a user-supplied --append-system-prompt into one flag. A user prompt
// file is left alone and Mira's block is skipped, since Claude Code may not
// combine both forms.
func claudeSystemPromptArguments(mira string, userArgs []string, stderr io.Writer) ([]string, []string) {
	if mira == "" {
		return nil, userArgs
	}
	for _, arg := range userArgs {
		if arg == "--" {
			break
		}
		if arg == "--append-system-prompt-file" || strings.HasPrefix(arg, "--append-system-prompt-file=") {
			fmt.Fprintln(stderr, "mira: --append-system-prompt-file given; Mira instructions are not added")
			return nil, userArgs
		}
	}
	rest := make([]string, 0, len(userArgs))
	user := ""
	for index := 0; index < len(userArgs); index++ {
		arg := userArgs[index]
		if arg == "--" {
			rest = append(rest, userArgs[index:]...)
			break
		}
		switch {
		case arg == "--append-system-prompt" && index+1 < len(userArgs):
			user = joinInstructions(user, userArgs[index+1])
			index++
		case strings.HasPrefix(arg, "--append-system-prompt="):
			user = joinInstructions(user, strings.TrimPrefix(arg, "--append-system-prompt="))
		default:
			rest = append(rest, arg)
		}
	}
	return []string{"--append-system-prompt", joinInstructions(mira, user)}, rest
}
