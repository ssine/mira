package views

import (
	"reflect"
	"testing"
)

func TestToolItemViewCommandAndFileChange(t *testing.T) {
	command := ToolItemView(map[string]any{"type": "CommandExecution", "command": []any{"cat", "config.go"}, "cwd": "/project", "parsed_cmd": []any{map[string]any{"type": "read", "path": "/project/config.go"}}, "status": "completed", "aggregated_output": "package main", "exit_code": 0, "duration": map[string]any{"secs": 15, "nanos": 200000000}})
	activity := object(command["activity"])
	actions := array(activity["actions"])
	if command["title"] != "Shell" || object(actions[0])["kind"] != "read" || activity["durationMs"] != float64(15200) {
		t.Fatalf("command: %#v", command)
	}
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1 +1,2 @@\n-old\n+new\n+++ added line\n"
	file := ToolItemView(map[string]any{"type": "FileChange", "changes": []any{map[string]any{"path": "main.go", "kind": map[string]any{"type": "update", "movePath": "new.go"}, "diff": diff}}})
	action := object(array(object(file["activity"])["actions"])[0])
	if !reflect.DeepEqual(action, map[string]any{"kind": "edit", "label": "main.go → new.go", "added": 2, "removed": 1}) {
		t.Fatalf("file action: %#v", action)
	}
}

func TestResponseToolViewOnlyInterpretsKnownTools(t *testing.T) {
	shell := ResponseToolView(map[string]any{"name": "exec_command", "arguments": `{"cmd":"go test ./...","workdir":"/project"}`}, "completed")
	if shell == nil || object(array(object(shell["activity"])["actions"])[0])["label"] != "go test ./..." {
		t.Fatalf("shell: %#v", shell)
	}
	if ResponseToolView(map[string]any{"name": "exec", "input": "text(await tools.exec_command(...))"}, "completed") != nil {
		t.Fatal("code-mode wrapper must remain opaque")
	}
	patch := ResponseToolView(map[string]any{"name": "apply_patch", "input": "*** Begin Patch\n*** Add File: a.go\n+package a\n*** Update File: b.go\n@@\n-old\n+new\n*** End Patch"}, "completed")
	actions := array(object(patch["activity"])["actions"])
	if len(actions) != 2 || object(actions[0])["kind"] != "create" || object(actions[1])["kind"] != "edit" {
		t.Fatalf("patch: %#v", patch)
	}
}

func TestImageProjectionRejectsExternalURLs(t *testing.T) {
	data := "data:image/png;base64,AAAA"
	images := historyImages(record("response_item", map[string]any{"type": "function_call_output", "output": []any{
		map[string]any{"type": "input_image", "image_url": data}, map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"},
	}}))
	if !reflect.DeepEqual(images, []historyImage{{0, data}}) {
		t.Fatalf("images: %#v", images)
	}

	for _, candidate := range []string{"relative.png", "https://example.test/x.png", "file://user:pass@localhost/tmp/x.png"} {
		if ImagePath(candidate) != "" {
			t.Fatalf("unsafe path accepted: %s", candidate)
		}
	}
	if ImagePath("file:///tmp/chart.png") != "/tmp/chart.png" || ImagePath(`C:\images\plot.png`) != `C:\images\plot.png` {
		t.Fatal("local image path rejected")
	}
}
