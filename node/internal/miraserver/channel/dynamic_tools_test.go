package channel

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ssine/mira/node/internal/agenttools"
)

func TestCodexToolAdapter(t *testing.T) {
	namespace := DynamicToolSpecs()[0].(map[string]any)
	if namespace["type"] != "namespace" || namespace["name"] != "home_nodes" {
		t.Fatalf("Codex namespace changed: %#v", namespace)
	}
	tools := namespace["tools"].([]any)
	for index, shared := range agenttools.Catalog() {
		tool := tools[index].(map[string]any)
		if tool["type"] != "function" || tool["name"] != shared.Name || !reflect.DeepEqual(tool["inputSchema"], shared.InputSchema) {
			t.Fatalf("Codex tool adapter changed schema: %#v", tool)
		}
	}
	result := map[string]any{"action": "screenshot", "mimeType": "image/png", "encoding": "base64", "content": "cG5n"}
	items := DynamicToolContentItems("screen", result)
	if len(items) != 2 || items[0]["type"] != "inputText" || items[1]["type"] != "inputImage" || items[1]["imageUrl"] != "data:image/png;base64,cG5n" {
		t.Fatalf("Codex image wire format changed: %#v", items)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(items[0]["text"].(string)), &metadata); err != nil || metadata["content"] != nil {
		t.Fatalf("image bytes leaked into text: %#v, %v", metadata, err)
	}
}
