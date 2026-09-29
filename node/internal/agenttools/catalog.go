// Package agenttools describes Mira's device tools independently of an Agent
// runtime. Execution and authorization remain in the capability service.
package agenttools

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

const Namespace = "home_nodes"
const Description = "Inspect and operate trusted computers connected to the home control server."

// Tool uses the MCP tools/list shape. Codex's namespace and function wrappers
// belong to the App Server adapter, not to the capability definitions.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

//go:embed tools.json
var catalogJSON []byte

// Catalog returns an independent copy so runtime adapters cannot modify the
// schema subsequently advertised to another client.
func Catalog() []Tool {
	var tools []Tool
	if err := json.Unmarshal(catalogJSON, &tools); err != nil {
		panic(err)
	}
	return tools
}

// ContentBlock is the MCP text/image content shape. Screenshots remain typed
// images; runtime adapters must not bury their bytes inside JSON text.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

func Content(tool string, result any) []ContentBlock {
	record, _ := result.(map[string]any)
	if tool == "screen" && record["action"] == "screenshot" &&
		record["mimeType"] == "image/png" && record["encoding"] == "base64" {
		if content, ok := record["content"].(string); ok {
			metadata := make(map[string]any, len(record)-1)
			for name, value := range record {
				if name != "content" {
					metadata[name] = value
				}
			}
			return []ContentBlock{
				{Type: "text", Text: encodeText(metadata)},
				{Type: "image", Data: content, MIMEType: "image/png"},
			}
		}
	}
	return []ContentBlock{{Type: "text", Text: encodeText(result)}}
}

func encodeText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}
