package channel

import "github.com/ssine/mira/node/internal/agenttools"

const DynamicToolNamespace = agenttools.Namespace

// DynamicToolSpecs adapts the shared catalog to Codex App Server's namespaced
// dynamicTools protocol. The legacy HTTP endpoint uses this same adapter.
func DynamicToolSpecs() []any {
	catalog := agenttools.Catalog()
	tools := make([]any, 0, len(catalog))
	for _, tool := range catalog {
		tools = append(tools, map[string]any{
			"type": "function", "name": tool.Name,
			"description": tool.Description, "inputSchema": tool.InputSchema,
		})
	}
	return []any{map[string]any{
		"type": "namespace", "name": agenttools.Namespace,
		"description": agenttools.Description, "tools": tools,
	}}
}

func mergeDynamicTools(existing any) []any {
	tools, _ := existing.([]any)
	result := make([]any, 0, len(tools)+1)
	for _, tool := range tools {
		record, _ := tool.(map[string]any)
		if name, _ := record["name"].(string); name == DynamicToolNamespace {
			continue
		}
		result = append(result, tool)
	}
	return append(result, DynamicToolSpecs()...)
}

// DynamicToolContentItems keeps Codex-specific content types at the boundary.
func DynamicToolContentItems(tool string, result any) []map[string]any {
	content := agenttools.Content(tool, result)
	items := make([]map[string]any, 0, len(content))
	for _, block := range content {
		if block.Type == "image" {
			items = append(items, map[string]any{"type": "inputImage", "imageUrl": "data:" + block.MIMEType + ";base64," + block.Data})
		} else {
			items = append(items, map[string]any{"type": "inputText", "text": block.Text})
		}
	}
	return items
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
