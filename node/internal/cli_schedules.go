package node

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

func (client *cliClient) runSchedule(ctx context.Context, args []string) (any, error) {
	action := "list"
	if len(args) > 0 {
		action = args[0]
		args = args[1:]
	}
	// Positional identity comes before flags, matching the documented interface.
	id := ""
	if action != "create" && action != "list" {
		if len(args) == 0 {
			return nil, fmt.Errorf("task UUID required")
		}
		id = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("schedule "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("file", "", "task JSON file")
	revision := flags.Int64("expected-revision", 0, "expected task revision")
	requestID := flags.String("request-id", "", "idempotency UUID for run-now")
	runID := flags.String("run-id", "", "uncertain execution UUID")
	confirmed := flags.Bool("confirm-stopped", false, "confirm prior script and external work stopped")
	limit := flags.Int("limit", 50, "page size")
	after := flags.String("after", "", "task page cursor")
	before := flags.String("before", "", "run page cursor")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected schedule arguments")
	}
	route := "/v1/schedules"
	if id != "" {
		route += "/" + url.PathEscape(id)
	}
	method := http.MethodGet
	var body map[string]any
	switch action {
	case "list":
		route += "?" + url.Values{"limit": {strconv.Itoa(*limit)}, "after": {*after}}.Encode()
	case "get":
	case "runs":
		route += "/runs?" + url.Values{"limit": {strconv.Itoa(*limit)}, "before": {*before}}.Encode()
	case "create", "update":
		if *file == "" {
			return nil, fmt.Errorf("--file is required")
		}
		f, err := os.Open(*file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 65537))
		if err != nil {
			return nil, err
		}
		if len(raw) > 65536 {
			return nil, fmt.Errorf("task JSON exceeds 64 KiB")
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		if body == nil {
			return nil, fmt.Errorf("task JSON must be an object")
		}
		if selector, ok := body["nodeId"].(string); ok {
			node, err := client.resolveNode(ctx, selector)
			if err != nil {
				return nil, err
			}
			body["nodeId"] = node["nodeId"]
		}
		if _, ok := body["enabled"]; !ok {
			body["enabled"] = true
		}
		method = http.MethodPost
		if action == "create" {
			if _, ok := body["taskId"]; !ok {
				key, err := randomID()
				if err != nil {
					return nil, err
				}
				body["taskId"] = key
			}
		} else {
			method = http.MethodPut
			body["expectedRevision"] = *revision
		}
	case "run":
		method = http.MethodPost
		route += "/run"
		key := *requestID
		if key == "" {
			var err error
			key, err = randomID()
			if err != nil {
				return nil, err
			}
		}
		body = map[string]any{"requestId": key}
	case "acknowledge":
		method = http.MethodPost
		route += "/acknowledge"
		body = map[string]any{"runId": *runID, "confirmStopped": *confirmed}
	case "pause", "resume", "delete":
		method = http.MethodPatch
		if action == "delete" {
			method = http.MethodDelete
		}
		body = map[string]any{"expectedRevision": *revision, "enabled": action == "resume"}
	default:
		return nil, fmt.Errorf("unknown schedule action %s", action)
	}
	var result any
	var input any
	if method != http.MethodGet {
		input = body
	}
	err := client.request(ctx, method, route, input, &result)
	return result, err
}
