package agenttools

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCatalogIsolationAndBounds(t *testing.T) {
	tools := Catalog()
	if len(tools) != 5 || tools[0].Name != "status" {
		t.Fatalf("unexpected tool catalog: %#v", tools)
	}
	file := tools[1].InputSchema["properties"].(map[string]any)
	length := file["length"].(map[string]any)
	if length["maximum"] != float64(4*1024*1024) {
		t.Fatal("file reads lost their bounded schema")
	}
	length["maximum"] = 1
	delete(tools[1].InputSchema, "required")
	fresh := Catalog()[1].InputSchema
	if fresh["properties"].(map[string]any)["length"].(map[string]any)["maximum"] != float64(4*1024*1024) || fresh["required"] == nil {
		t.Fatal("an adapter mutated the shared schema")
	}
}

func TestScreenshotRemainsAnImage(t *testing.T) {
	result := map[string]any{"action": "screenshot", "mimeType": "image/png", "encoding": "base64", "content": "cG5n", "width": 12}
	blocks := Content("screen", result)
	if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Data != "cG5n" || blocks[1].MIMEType != "image/png" {
		t.Fatalf("screenshot was not a typed image: %#v", blocks)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(blocks[0].Text), &metadata); err != nil || metadata["content"] != nil || metadata["width"] != float64(12) {
		t.Fatalf("invalid screenshot metadata: %#v, %v", metadata, err)
	}
	if result["content"] != "cG5n" {
		t.Fatal("encoding mutated the Node result")
	}
	for _, tool := range []string{"file", "process"} {
		blocks := Content(tool, result)
		if len(blocks) != 1 || blocks[0].Type != "text" {
			t.Fatalf("%s data was incorrectly promoted to an image", tool)
		}
	}
}

func TestContentPreservesUnknownResultFields(t *testing.T) {
	result := map[string]any{"future": []any{"nul\x00value", true}, "output": "你好"}
	blocks := Content("process", result)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(blocks[0].Text), &decoded); err != nil || !reflect.DeepEqual(decoded, result) {
		t.Fatalf("result changed: %#v, %v", decoded, err)
	}
}
