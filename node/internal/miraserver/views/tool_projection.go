package views

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var imageDataPattern = regexp.MustCompile(`(?i)^data:(?:image/(?:png|jpe?g|webp|gif|bmp|avif|tiff)|application/octet-stream);base64,[a-z0-9+/=\r\n]+$`)
var windowsPathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
var patchFilePattern = regexp.MustCompile(`^\*\*\* (Add|Update|Delete) File: (.+)$`)

func valueText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		parts := []string{}
		for _, entry := range value {
			if text := valueText(entry); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "inputText", "input_text", "outputText", "output_text", "message"} {
			if text, ok := value[key].(string); ok {
				return text
			}
		}
		if value["content"] != nil {
			return valueText(value["content"])
		}
		if value["output"] != nil {
			return valueText(value["output"])
		}
	}
	return ""
}

func parseJSONString(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || trimmed[0] != '{' && trimmed[0] != '[' {
		return value
	}
	var parsed any
	if decodeJSON([]byte(trimmed), &parsed) != nil {
		return value
	}
	return parsed
}

func projectedErrorMessage(value any, depth int) string {
	if depth > 6 || value == nil {
		return ""
	}
	parsed := parseJSONString(value)
	if text, ok := parsed.(string); ok {
		return text
	}
	valueObject := object(parsed)
	if valueObject == nil {
		return ""
	}
	for _, key := range []string{"error", "message", "detail"} {
		if result := projectedErrorMessage(valueObject[key], depth+1); result != "" {
			return result
		}
	}
	return ""
}

func ImagePath(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(text), "file:") {
		parsed, err := url.Parse(text)
		if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return ""
		}
		path, err := url.PathUnescape(parsed.EscapedPath())
		if err != nil {
			return ""
		}
		if parsed.Host != "" && parsed.Host != "localhost" {
			return `\\` + parsed.Host + strings.ReplaceAll(path, "/", `\`)
		}
		if len(path) >= 3 && path[0] == '/' && windowsPathPattern.MatchString(path[1:]) {
			return path[1:]
		}
		return path
	}
	if strings.HasPrefix(text, "/") || strings.HasPrefix(text, `\\`) || windowsPathPattern.MatchString(text) {
		return text
	}
	return ""
}

func imageDataURL(value any) string {
	text, ok := value.(string)
	if ok && imageDataPattern.MatchString(text) {
		return text
	}
	return ""
}

func OutputImages(value any, depth int) []map[string]any {
	if depth > 8 || value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || trimmed[0] != '[' && trimmed[0] != '{' {
			return nil
		}
		var parsed any
		if decodeJSON([]byte(trimmed), &parsed) != nil {
			return nil
		}
		return OutputImages(parsed, depth+1)
	}
	if values := array(value); values != nil {
		result := []map[string]any{}
		for _, entry := range values {
			result = append(result, OutputImages(entry, depth+1)...)
		}
		return result
	}
	valueObject := object(value)
	if valueObject == nil {
		return nil
	}
	imageURL := imageDataURL(first(valueObject["image_url"], valueObject["imageUrl"]))
	if imageURL == "" && stringValue(valueObject["type"]) == "image" {
		if data, ok := valueObject["data"].(string); ok {
			imageURL = imageDataURL("data:" + stringValue(valueObject["mimeType"]) + ";base64," + data)
		}
	}
	if imageURL != "" {
		return []map[string]any{{"url": imageURL}}
	}
	for _, key := range []string{"content", "contentItems", "content_items", "output", "body", "result"} {
		if valueObject[key] != nil {
			return OutputImages(valueObject[key], depth+1)
		}
	}
	return nil
}

func MergeImages(previous, next []map[string]any) []map[string]any {
	if len(previous) == 0 {
		return next
	}
	if len(next) == 0 {
		return previous
	}
	if len(previous) == 1 && len(next) == 1 {
		if stringValue(previous[0]["path"]) != "" && stringValue(next[0]["url"]) != "" {
			return []map[string]any{{"path": previous[0]["path"], "url": next[0]["url"]}}
		}
		if stringValue(previous[0]["url"]) != "" && stringValue(next[0]["path"]) != "" {
			return []map[string]any{{"path": next[0]["path"], "url": previous[0]["url"]}}
		}
	}
	result := []map[string]any{}
	seen := map[string]bool{}
	for _, image := range append(append([]map[string]any{}, previous...), next...) {
		key := "p:" + stringValue(image["path"])
		if image["url"] != nil {
			key = "u:" + stringValue(image["url"])
		}
		if !seen[key] {
			result = append(result, image)
			seen[key] = true
		}
	}
	return result
}

func sanitizeImages(value any, key string) any {
	if imageDataURL(value) != "" {
		return "[图片单独显示]"
	}
	if text, ok := value.(string); ok && includes([]string{"result", "output", "content"}, key) {
		trimmed := strings.TrimSpace(text)
		if trimmed != "" && (trimmed[0] == '[' || trimmed[0] == '{') {
			var parsed any
			if decodeJSON([]byte(trimmed), &parsed) == nil {
				return sanitizeImages(parsed, key)
			}
		}
	}
	if values := array(value); values != nil {
		result := make([]any, len(values))
		for index, entry := range values {
			result[index] = sanitizeImages(entry, "")
		}
		return result
	}
	if valueObject := object(value); valueObject != nil {
		result := map[string]any{}
		for field, entry := range valueObject {
			if stringValue(valueObject["type"]) == "image" && field == "data" {
				result[field] = "[图片单独显示]"
			} else {
				result[field] = sanitizeImages(entry, field)
			}
		}
		return result
	}
	return value
}

func printable(value any) string {
	parsed := parseJSONString(value)
	if text := valueText(parsed); text != "" {
		return text
	}
	if text, ok := parsed.(string); ok {
		return text
	}
	if parsed == nil {
		return ""
	}
	payload, err := json.MarshalIndent(sanitizeImages(parsed, ""), "", "  ")
	if err != nil {
		return ""
	}
	return string(payload)
}

func boundedText(value any) string {
	text := fmt.Sprint(value)
	if value == nil {
		text = ""
	}
	if len([]byte(text)) <= 1024*1024 {
		return text
	}
	end := min(jsUTF16Length(text), 1024*1024)
	for len([]byte(jsSlice(text, end))) > 1024*1024 {
		end = int(float64(end) * .9)
	}
	return jsSlice(text, end) + "\n\n[网页投影已截断；完整事件仍保存在 PostgreSQL]"
}

func compact(value any, limit int) string {
	text := strings.Join(strings.Fields(fmt.Sprint(value)), " ")
	if jsUTF16Length(text) > limit {
		return jsSlice(text, limit) + "…"
	}
	return text
}

func activityStatus(value any, exitCode any) string {
	status := normalizedType(value)
	if includes([]string{"failed", "error", "失败"}, status) {
		return "failed"
	}
	if code, ok := number(exitCode); ok && code != 0 {
		return "failed"
	}
	if includes([]string{"declined", "denied", "拒绝"}, status) {
		return "declined"
	}
	if includes([]string{"interrupted", "cancelled", "canceled", "aborted", "中断"}, status) {
		return "interrupted"
	}
	if includes([]string{"inprogress", "running", "运行", "运行中", "等待处理"}, status) {
		return "running"
	}
	return "completed"
}

func durationMS(item map[string]any) any {
	if value, ok := number(first(item["durationMs"], item["duration_ms"])); ok && value >= 0 {
		return value
	}
	duration := object(item["duration"])
	seconds, secondsOK := number(duration["secs"])
	nanos, nanosOK := number(duration["nanos"])
	if secondsOK && nanosOK {
		return seconds*1000 + nanos/1e6
	}
	return nil
}

func commandActions(item map[string]any, command string) []map[string]any {
	values := array(first(item["commandActions"], item["command_actions"], item["parsed_cmd"]))
	if len(values) == 0 {
		label := compact(command, 160)
		if label == "" {
			label = "命令"
		}
		return []map[string]any{{"kind": "run", "label": label}}
	}
	result := []map[string]any{}
	for _, value := range values {
		action := object(value)
		actionType := normalizedType(action["type"])
		switch actionType {
		case "read":
			label := compact(first(action["path"], action["name"], "文件"), 160)
			result = append(result, map[string]any{"kind": "read", "label": label})
		case "listfiles":
			label := compact(first(action["path"], item["cwd"], "目录"), 160)
			result = append(result, map[string]any{"kind": "list", "label": label})
		case "search":
			label := "内容"
			if action["query"] != nil {
				label = "“" + compact(action["query"], 160) + "”"
			}
			if action["path"] != nil {
				label += "（" + compact(action["path"], 160) + "）"
			}
			result = append(result, map[string]any{"kind": "search", "label": label})
		default:
			label := compact(first(action["command"], action["cmd"], command), 160)
			if label == "" {
				label = "命令"
			}
			result = append(result, map[string]any{"kind": "run", "label": label})
		}
	}
	return result
}

func diffStats(diff string) (int, int) {
	added, removed, inHunk := 0, 0, false
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@") {
			inHunk = true
			continue
		}
		if !inHunk && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ")) {
			continue
		}
		if strings.HasPrefix(line, "+") {
			added++
		}
		if strings.HasPrefix(line, "-") {
			removed++
		}
	}
	return added, removed
}

type fileChange struct {
	action map[string]any
	detail string
}

func fileChanges(value any) []fileChange {
	values := array(value)
	if values == nil {
		for path, raw := range object(value) {
			change := cloneMap(object(raw))
			change["path"] = path
			values = append(values, change)
		}
	}
	result := []fileChange{}
	for _, raw := range values {
		change := object(raw)
		kindObject := object(change["kind"])
		kind := normalizedType(first(kindObject["type"], change["kind"], change["type"]))
		actionKind := "edit"
		if kind == "add" {
			actionKind = "create"
		}
		if kind == "delete" {
			actionKind = "delete"
		}
		path := compact(first(change["path"], "文件"), 160)
		action := map[string]any{"kind": actionKind, "label": path}
		move := stringValue(first(kindObject["movePath"], change["movePath"], change["move_path"]))
		if move != "" {
			action["label"] = path + " → " + compact(move, 160)
		}
		whole := kind == "add" || kind == "delete"
		var diff, content any
		if !whole {
			diff = first(change["diff"], change["unified_diff"])
		}
		content = change["content"]
		if whole && content == nil {
			content = change["diff"]
		}
		if text, ok := diff.(string); ok {
			added, removed := diffStats(text)
			action["added"], action["removed"] = added, removed
		} else if text, ok := content.(string); ok {
			lines := 0
			if text != "" {
				lines = len(strings.Split(text, "\n"))
				if strings.HasSuffix(text, "\n") {
					lines--
				}
			}
			action["added"], action["removed"] = 0, 0
			if kind == "add" {
				action["added"] = lines
			}
			if kind == "delete" {
				action["removed"] = lines
			}
		}
		detailParts := []string{stringValue(change["path"])}
		if move != "" {
			detailParts = append(detailParts, "→ "+move)
		}
		if diff != nil {
			detailParts = append(detailParts, fmt.Sprint(diff))
		} else if content != nil {
			detailParts = append(detailParts, fmt.Sprint(content))
		}
		filtered := []string{}
		for _, part := range detailParts {
			if part != "" {
				filtered = append(filtered, part)
			}
		}
		result = append(result, fileChange{action, strings.Join(filtered, "\n")})
	}
	return result
}

// ToolItemView mirrors the browser's stable activity projection.
func ToolItemView(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	typeName := normalizedType(item["type"])
	exitCode := first(item["exitCode"], item["exit_code"])
	activity := map[string]any{"status": activityStatus(item["status"], exitCode), "durationMs": durationMS(item), "exitCode": exitCode, "actions": []map[string]any{}}
	var title, body string
	var images []map[string]any
	switch typeName {
	case "imageview":
		path := ImagePath(item["path"])
		if path != "" {
			images = []map[string]any{{"path": path}}
		}
		activity["actions"] = []map[string]any{{"kind": "tool", "label": "查看图片"}}
		return map[string]any{"kind": "tool", "title": "查看图片", "body": path, "markdown": false, "images": images, "activity": activity}
	case "commandexecution":
		title = "Shell"
		if commands := array(item["command"]); commands != nil {
			parts := []string{}
			for _, command := range commands {
				parts = append(parts, fmt.Sprint(command))
			}
			body = strings.Join(parts, " ")
		} else {
			body = valueText(item["command"])
		}
		activity["actions"] = commandActions(item, body)
		parts := []string{body}
		if item["cwd"] != nil {
			parts = append(parts, "cwd: "+fmt.Sprint(item["cwd"]))
		}
		output := first(item["aggregatedOutput"], item["aggregated_output"], item["formatted_output"], item["output"])
		if output == nil {
			output = valueText([]any{item["stdout"], item["stderr"]})
		}
		if fmt.Sprint(output) != "" && output != nil {
			parts = append(parts, fmt.Sprint(output))
		}
		if exitCode != nil {
			parts = append(parts, "exit code: "+fmt.Sprint(exitCode))
		}
		body = strings.Join(parts, "\n\n")
	case "filechange":
		title = "文件修改"
		changes := fileChanges(item["changes"])
		actions := []map[string]any{}
		details := []string{}
		for _, change := range changes {
			actions = append(actions, change.action)
			details = append(details, change.detail)
		}
		if len(actions) == 0 {
			actions = append(actions, map[string]any{"kind": "edit", "label": "文件"})
		}
		activity["actions"] = actions
		body = strings.Join(details, "\n\n")
		if output := valueText([]any{item["stdout"], item["stderr"]}); output != "" {
			if body != "" {
				body += "\n\n"
			}
			body += output
		}
	case "mcptoolcall", "dynamictoolcall", "toolcall", "collabagenttoolcall", "collabtoolcall":
		name := first(item["tool"], item["name"], typeName)
		namespace := first(item["server"], item["namespace"])
		title = fmt.Sprint(name)
		if namespace != nil {
			title = fmt.Sprint(namespace) + " · " + title
		}
		activity["actions"] = []map[string]any{{"kind": "tool", "label": compact(title, 160)}}
		payload := map[string]any{}
		if arguments := first(item["arguments"], item["input"], item["prompt"]); arguments != nil {
			payload["arguments"] = arguments
		}
		if result := first(item["result"], item["contentItems"], item["content_items"], item["output"]); result != nil {
			payload["result"] = result
		}
		if value, exists := item["error"]; exists {
			payload["error"] = value
		}
		if value, exists := item["success"]; exists {
			payload["success"] = value
		}
		encoded, _ := json.MarshalIndent(sanitizeImages(payload, ""), "", "  ")
		body = string(encoded)
		if success, ok := item["success"].(bool); ok && !success || item["error"] != nil {
			activity["status"] = "failed"
		}
	default:
		return nil
	}
	images = OutputImages(first(item["result"], item["contentItems"], item["content_items"], item["output"]), 0)
	result := map[string]any{"kind": "tool", "title": title, "body": body, "activity": activity, "markdown": false}
	if len(images) > 0 {
		result["images"] = images
	}
	return result
}

func normalizedToolTitle(item map[string]any) string {
	name := fmt.Sprint(first(item["name"], item["tool"], item["action"], item["type"], "Tool"))
	if name == "exec" {
		return "functions.exec"
	}
	if name == "apply_patch" {
		return "functions.apply_patch"
	}
	if namespace := first(item["namespace"], item["server"]); namespace != nil {
		return fmt.Sprint(namespace) + " · " + name
	}
	return name
}

func ResponseToolView(payload map[string]any, status string) map[string]any {
	input := first(payload["arguments"], payload["input"], payload["command"])
	if text, ok := input.(string); ok {
		parsed := parseJSONString(text)
		input = parsed
	}
	name := fmt.Sprint(first(payload["name"], payload["tool"], ""))
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	if includes([]string{"exec_command", "shell_command", "shell", "local_shell"}, name) || stringValue(payload["type"]) == "local_shell_call" {
		inputObject := object(input)
		return ToolItemView(map[string]any{"type": "commandExecution", "status": status, "command": first(inputObject["cmd"], inputObject["command"], payload["command"]), "cwd": first(inputObject["workdir"], inputObject["cwd"]), "commandActions": first(payload["commandActions"], payload["parsed_cmd"])})
	}
	text, ok := input.(string)
	if name != "apply_patch" || !ok {
		return nil
	}
	changes := []map[string]any{}
	for _, line := range strings.Split(text, "\n") {
		if match := patchFilePattern.FindStringSubmatch(line); match != nil {
			changes = append(changes, map[string]any{"path": match[2], "kind": strings.ToLower(match[1]), "diff": ""})
		} else if len(changes) > 0 && !strings.HasPrefix(line, "***") {
			changes[len(changes)-1]["diff"] = stringValue(changes[len(changes)-1]["diff"]) + line + "\n"
		}
	}
	if len(changes) == 0 {
		return nil
	}
	for _, change := range changes {
		if change["kind"] == "add" {
			lines := []string{}
			for _, line := range strings.Split(stringValue(change["diff"]), "\n") {
				if strings.HasPrefix(line, "+") {
					lines = append(lines, line[1:])
				}
			}
			change["content"] = strings.Join(lines, "\n")
		}
		if change["kind"] == "delete" {
			delete(change, "diff")
		}
	}
	return ToolItemView(map[string]any{"type": "fileChange", "status": status, "changes": anySliceMaps(changes)})
}

func anySliceMaps(values []map[string]any) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}
