package channel

import (
	"encoding/json"
	"fmt"
)

const DynamicToolNamespace = "home_nodes"

const dynamicToolSpecsJSON = `[{"type":"namespace","name":"home_nodes","description":"Inspect and operate trusted computers connected to the home control server.","tools":[{"type":"function","name":"status","description":"List connected nodes or refresh detailed status for one node.","inputSchema":{"type":"object","properties":{"action":{"type":"string","enum":["list","get"]},"nodeId":{"type":"string","description":"Target Node UUID, exact nodeKey, or user-defined alias from home_nodes.status(action=list)."}},"required":["action"],"additionalProperties":false}},{"type":"function","name":"file","description":"List, inspect, read, write, create, move, or remove files inside a node's configured roots. Windows Nodes default to the active interactive user; use executionContext=system only for operations that require the LocalSystem service identity.","inputSchema":{"type":"object","properties":{"nodeId":{"type":"string","description":"Target Node UUID, exact nodeKey, or user-defined alias from home_nodes.status(action=list)."},"action":{"type":"string","enum":["roots","stat","list","read","write","mkdir","move","remove"]},"path":{"type":"string"},"destination":{"type":"string"},"content":{"type":"string"},"encoding":{"type":"string","enum":["utf8","base64"]},"offset":{"type":"integer","minimum":0},"length":{"type":"integer","minimum":1,"maximum":4194304},"recursive":{"type":"boolean"},"overwrite":{"type":"boolean"},"append":{"type":"boolean","description":"Append a file chunk only when offset equals the current file size; updated Nodes only."},"executionContext":{"type":"string","enum":["user","system"],"description":"Windows execution identity. Defaults to the active interactive user; system is the LocalSystem Node service identity."},"userSessionId":{"type":"integer","minimum":0,"maximum":4294967295,"description":"Optional Windows session ID when more than one interactive user is active."}},"required":["nodeId","action"],"additionalProperties":false}},{"type":"function","name":"process","description":"Count or list system and managed processes, start a process, poll bounded output, or signal it. Windows process starts default to the active interactive user; use executionContext=system only for operations that require the LocalSystem service identity.","inputSchema":{"type":"object","properties":{"nodeId":{"type":"string","description":"Target Node UUID, exact nodeKey, or user-defined alias from home_nodes.status(action=list)."},"action":{"type":"string","enum":["count","list","start","poll","signal"]},"processId":{"type":"string"},"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"},"maxItems":128},"cwd":{"type":"string"},"env":{"type":"object","additionalProperties":{"type":"string"}},"cursor":{"type":"integer","minimum":0},"signal":{"type":"string","enum":["SIGINT","SIGTERM","SIGKILL"]},"system":{"type":"boolean"},"executionContext":{"type":"string","enum":["user","system"],"description":"Windows execution identity for action=start. Defaults to the active interactive user; system is the LocalSystem Node service identity."},"userSessionId":{"type":"integer","minimum":0,"maximum":4294967295,"description":"Optional Windows session ID when more than one interactive user is active."}},"required":["nodeId","action"],"additionalProperties":false}},{"type":"function","name":"pty","description":"Open an interactive terminal session, write input, poll output, resize it, or close the session.","inputSchema":{"type":"object","properties":{"nodeId":{"type":"string","description":"Target Node UUID, exact nodeKey, or user-defined alias from home_nodes.status(action=list)."},"action":{"type":"string","enum":["open","write","poll","resize","close","list"]},"sessionId":{"type":"string"},"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"},"maxItems":128},"cwd":{"type":"string"},"input":{"type":"string"},"cursor":{"type":"integer","minimum":0},"rows":{"type":"integer","minimum":1,"maximum":500},"cols":{"type":"integer","minimum":1,"maximum":1000}},"required":["nodeId","action"],"additionalProperties":false}},{"type":"function","name":"screen","description":"Inspect and control an Android display through a trusted Mira Node. Screenshots are returned to the model as images.","inputSchema":{"type":"object","properties":{"nodeId":{"type":"string","description":"Target Node UUID, exact nodeKey, or user-defined alias from home_nodes.status(action=list)."},"action":{"type":"string","enum":["display","screenshot","hierarchy","tap","swipe","key","text"]},"x":{"type":"integer","minimum":0},"y":{"type":"integer","minimum":0},"startX":{"type":"integer","minimum":0},"startY":{"type":"integer","minimum":0},"endX":{"type":"integer","minimum":0},"endY":{"type":"integer","minimum":0},"durationMs":{"type":"integer","minimum":1,"maximum":60000},"keyCode":{"oneOf":[{"type":"integer","minimum":0,"maximum":999},{"type":"string","pattern":"^KEYCODE_[A-Z0-9_]+$"}]},"text":{"type":"string","minLength":1,"maxLength":4096}},"required":["nodeId","action"],"additionalProperties":false}}]}]`

func DynamicToolSpecs() []any {
	var specs []any
	if err := json.Unmarshal([]byte(dynamicToolSpecsJSON), &specs); err != nil {
		panic(err)
	}
	return specs
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

func DynamicToolContentItems(tool string, result any) []map[string]any {
	record, _ := result.(map[string]any)
	if tool == "screen" && stringValue(record["action"]) == "screenshot" &&
		stringValue(record["mimeType"]) == "image/png" && stringValue(record["encoding"]) == "base64" {
		if content, ok := record["content"].(string); ok {
			metadata := make(map[string]any, len(record)-1)
			for name, value := range record {
				if name != "content" {
					metadata[name] = value
				}
			}
			encoded, _ := json.Marshal(metadata)
			return []map[string]any{
				{"type": "inputText", "text": string(encoded)},
				{"type": "inputImage", "imageUrl": "data:image/png;base64," + content},
			}
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		encoded = []byte(fmt.Sprint(result))
	}
	return []map[string]any{{"type": "inputText", "text": string(encoded)}}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
