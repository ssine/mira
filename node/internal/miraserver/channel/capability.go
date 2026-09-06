package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"
	"unicode/utf16"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var capabilityActions = map[string]map[string]bool{
	"status":        {"get": true},
	"file":          {"roots": true, "stat": true, "list": true, "read": true, "write": true, "mkdir": true, "move": true, "remove": true},
	"process":       {"count": true, "list": true, "start": true, "poll": true, "signal": true},
	"pty":           {"list": true, "open": true, "write": true, "poll": true, "resize": true, "close": true},
	"screen":        {"display": true, "screenshot": true, "hierarchy": true, "tap": true, "swipe": true, "key": true, "text": true},
	"codexSessions": {"list": true, "read": true, "resolve": true},
}

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	rolloutIDPattern       = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type CapabilityInvoker interface {
	IsConnected(string) bool
	Invoke(context.Context, string, string, map[string]any, time.Duration) (any, error)
}

type CapabilityService struct {
	nodes   NodeRegistry
	channel CapabilityInvoker
	audit   AuditFunc
}

type InvokeContext struct {
	Timeout       time.Duration
	ThreadID      string
	RequestID     string
	Request       *http.Request
	AuditMetadata map[string]any
}

func NewCapabilityService(registry NodeRegistry, channel CapabilityInvoker, audit AuditFunc) *CapabilityService {
	return &CapabilityService{nodes: registry, channel: channel, audit: audit}
}

func (service *CapabilityService) validateActor(ctx context.Context, actor *foundation.Principal) error {
	if actor == nil || actor.Revoked || (actor.Kind != "admin" && actor.Kind != "node") {
		return channelError("approved Node or administrator identity required", http.StatusForbidden, "actor_forbidden")
	}
	if actor.Kind == "node" {
		node, err := service.nodes.Get(ctx, actor.NodeID, false)
		if err != nil {
			return err
		}
		if node == nil {
			return channelError("actor Node is revoked", http.StatusForbidden, "actor_forbidden")
		}
	}
	return nil
}

func (service *CapabilityService) List(ctx context.Context, actor *foundation.Principal) ([]map[string]any, error) {
	if err := service.validateActor(ctx, actor); err != nil {
		return nil, err
	}
	values, err := service.nodes.List(ctx, actor.Kind == "admin")
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		result = append(result, nodes.Summary(value))
	}
	return result, nil
}

func (service *CapabilityService) Invoke(ctx context.Context, actor *foundation.Principal, selector, capability string, params map[string]any, options InvokeContext) (any, error) {
	if err := service.validateActor(ctx, actor); err != nil {
		return nil, err
	}
	if selector == "" {
		return nil, channelError("target Node selector is required", http.StatusBadRequest, "invalid_request")
	}
	if _, ok := capabilityActions[capability]; !ok {
		return nil, channelError("unknown capability", http.StatusBadRequest, "invalid_request")
	}
	validated, err := validateCapabilityParams(capability, params)
	if err != nil {
		return nil, err
	}
	resolution, err := service.nodes.Resolve(ctx, selector, false)
	if err != nil {
		return nil, err
	}
	if resolution.Status != http.StatusOK {
		body, _ := resolution.Body.(map[string]any)
		return nil, channelError(stringValue(body["error"]), resolution.Status, stringValue(body["code"]))
	}
	body, _ := resolution.Body.(map[string]any)
	target, ok := body["node"].(nodes.Node)
	if !ok {
		if pointer, pointerOK := body["node"].(*nodes.Node); pointerOK && pointer != nil {
			target = *pointer
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("Node resolver returned an invalid result")
	}
	if !advertised(target, capability) {
		return nil, channelError("target Node does not advertise "+capability, http.StatusConflict, "capability_unavailable")
	}
	if !service.channel.IsConnected(target.NodeID) {
		return nil, channelError("target Node is offline", http.StatusServiceUnavailable, "node_offline")
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = DefaultCapabilityTimeout
	}
	if timeout < 100*time.Millisecond {
		timeout = 100 * time.Millisecond
	}
	if timeout > MaximumCapabilityTimeout {
		timeout = MaximumCapabilityTimeout
	}
	result, invokeErr := service.channel.Invoke(ctx, target.NodeID, capability, validated, timeout)
	metadata := map[string]any{"capability": capability, "action": "get"}
	if action, ok := validated["action"].(string); ok {
		metadata["action"] = action
	}
	for name, value := range options.AuditMetadata {
		metadata[name] = value
	}
	if service.audit != nil {
		event := foundation.AuditEvent{
			Action: "capability.invoked", Principal: actor, TargetNodeID: target.NodeID,
			ThreadID: options.ThreadID, RequestID: options.RequestID, Request: options.Request,
			Metadata: metadata,
		}
		if invokeErr != nil {
			event.Action = "capability.failed"
			event.Failed = true
			event.ErrorCode = errorCode(invokeErr)
		}
		if auditErr := service.audit(ctx, event); auditErr != nil {
			if invokeErr == nil {
				invokeErr = auditErr
			}
		}
	}
	if invokeErr != nil {
		return nil, invokeErr
	}
	return result, nil
}

func DispatchDynamicTool(ctx context.Context, service *CapabilityService, actor *foundation.Principal, tool string, args map[string]any, options InvokeContext) (any, error) {
	if tool == "status" && stringValue(args["action"]) == "list" {
		nodeList, err := service.List(ctx, actor)
		if err != nil {
			return nil, err
		}
		return map[string]any{"nodes": nodeList}, nil
	}
	selector, ok := args["nodeId"].(string)
	if !ok {
		return nil, fmt.Errorf("nodeId is required")
	}
	if tool == "status" {
		return service.Invoke(ctx, actor, selector, "status", map[string]any{}, options)
	}
	if tool != "file" && tool != "process" && tool != "pty" && tool != "screen" {
		return nil, fmt.Errorf("unknown %s tool: %s", DynamicToolNamespace, tool)
	}
	params := make(map[string]any, len(args)-1)
	for name, value := range args {
		if name != "nodeId" {
			params[name] = value
		}
	}
	return service.Invoke(ctx, actor, selector, tool, params, options)
}

func advertised(node nodes.Node, capability string) bool {
	if capability == "status" {
		return true
	}
	keys := map[string][]string{
		"file": {"files"}, "process": {"processes"}, "pty": {"pty"},
		"screen": {"screen", "input"}, "codexSessions": {"codexSessions"},
	}
	for _, key := range keys[capability] {
		if value, _ := node.Capabilities[key].(bool); value {
			return true
		}
	}
	return false
}

func validateCapabilityParams(capability string, params map[string]any) (map[string]any, error) {
	if params == nil {
		return nil, channelError("params must be an object", 400, "invalid_request")
	}
	if capability == "status" {
		return map[string]any{}, nil
	}
	action, ok := params["action"].(string)
	if !ok || !capabilityActions[capability][action] {
		return nil, channelError("unsupported "+capability+" action", 400, "invalid_request")
	}
	var err error
	if capability == "file" {
		if action != "roots" {
			err = validateText(params, "path", true, 32768)
		}
		if err == nil {
			err = validateText(params, "destination", false, 32768)
		}
		if err == nil {
			err = validateText(params, "content", false, 6*1024*1024)
		}
		if err == nil {
			err = validateInteger(params, "offset", 0, maxSafeInteger)
		}
		if err == nil {
			err = validateInteger(params, "length", 1, 4*1024*1024)
		}
	}
	if (capability == "process" || capability == "pty") && err == nil {
		for _, field := range []struct {
			name    string
			maximum int
		}{
			{"command", 4096}, {"cwd", 32768}, {"processId", 256}, {"sessionId", 256}, {"input", 1024 * 1024},
		} {
			if err = validateText(params, field.name, false, field.maximum); err != nil {
				break
			}
		}
		if err == nil {
			err = validateInteger(params, "cursor", 0, maxSafeInteger)
		}
		if err == nil {
			if value, exists := params["args"]; exists {
				err = validateArgs(value)
			}
		}
		if err == nil {
			if value, exists := params["env"]; exists {
				err = validateEnvironment(value)
			}
		}
		if capability == "pty" && err == nil {
			err = validateInteger(params, "rows", 1, 500)
			if err == nil {
				err = validateInteger(params, "cols", 1, 1000)
			}
			if err == nil && action == "resize" {
				if _, rows := params["rows"]; !rows {
					err = channelError("PTY resize requires rows and cols", 400, "invalid_request")
				}
				if _, cols := params["cols"]; !cols {
					err = channelError("PTY resize requires rows and cols", 400, "invalid_request")
				}
			}
		}
	}
	if capability == "screen" && err == nil {
		for _, name := range []string{"x", "y", "startX", "startY", "endX", "endY"} {
			if err = validateInteger(params, name, 0, 100000); err != nil {
				break
			}
		}
		if err == nil {
			err = validateInteger(params, "durationMs", 1, 60000)
		}
		if err == nil {
			err = validateText(params, "text", false, 4096)
		}
	}
	if capability == "codexSessions" && err == nil {
		err = validateText(params, "path", false, 32768)
		if err == nil {
			err = validateInteger(params, "cursor", 0, maxSafeInteger)
		}
		if err == nil {
			err = validateInteger(params, "limit", 1, 8*1024*1024)
		}
		if err == nil && (action == "read" || action == "resolve") {
			if _, ok := params["path"].(string); !ok {
				err = channelError("path is required", 400, "invalid_request")
			}
		}
		if err == nil && action == "resolve" && !rolloutIDPattern.MatchString(stringValue(params["rolloutId"])) {
			err = channelError("valid rolloutId is required", 400, "invalid_request")
		}
	}
	return params, err
}

const maxSafeInteger = 9_007_199_254_740_991

func validateText(params map[string]any, name string, required bool, maximum int) error {
	value, exists := params[name]
	if !exists && !required {
		return nil
	}
	text, ok := value.(string)
	if !ok || (required && text == "") || len(utf16.Encode([]rune(text))) > maximum || containsNUL(text) {
		return channelError(name+" is invalid", 400, "invalid_request")
	}
	return nil
}

func validateInteger(params map[string]any, name string, minimum, maximum int64) error {
	value, exists := params[name]
	if !exists {
		return nil
	}
	number, ok := integerValue(value)
	if !ok || number < minimum || number > maximum {
		return channelError(fmt.Sprintf("%s must be an integer between %d and %d", name, minimum, maximum), 400, "invalid_request")
	}
	return nil
}

func integerValue(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(string(number), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || math.Trunc(parsed) != parsed || parsed < math.MinInt64 || parsed > math.MaxInt64 {
			return 0, false
		}
		return int64(parsed), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
			return 0, false
		}
		return int64(number), number >= math.MinInt64 && number <= math.MaxInt64
	case int:
		return int64(number), true
	case int64:
		return number, true
	default:
		return 0, false
	}
}

func validateArgs(value any) error {
	if value == nil {
		return channelError("args must contain at most 128 strings", 400, "invalid_request")
	}
	args, ok := value.([]any)
	if !ok || len(args) > 128 {
		return channelError("args must contain at most 128 strings", 400, "invalid_request")
	}
	for _, item := range args {
		text, ok := item.(string)
		if !ok || len(utf16.Encode([]rune(text))) > 32768 {
			return channelError("args must contain at most 128 strings", 400, "invalid_request")
		}
	}
	return nil
}

func validateEnvironment(value any) error {
	if value == nil {
		return channelError("env is invalid", 400, "invalid_request")
	}
	environment, ok := value.(map[string]any)
	if !ok || len(environment) > 256 {
		return channelError("env is invalid", 400, "invalid_request")
	}
	for name, raw := range environment {
		text, ok := raw.(string)
		if !environmentNamePattern.MatchString(name) || !ok || len(utf16.Encode([]rune(text))) > 32768 {
			return channelError("env is invalid", 400, "invalid_request")
		}
	}
	return nil
}

func containsNUL(value string) bool {
	for _, character := range value {
		if character == 0 {
			return true
		}
	}
	return false
}

func errorCode(err error) string {
	if typed, ok := err.(*Error); ok {
		if typed.Code != "" {
			return typed.Code
		}
		if typed.Status == 504 {
			return "capability_timeout"
		}
	}
	return "node_error"
}
