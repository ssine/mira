package miraserver

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	serverchannel "github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var (
	invokePattern  = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/invoke$`)
	sshRESTPattern = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/ssh/(keys|sessions)$`)
	runtimePattern = regexp.MustCompile(`(?i)^/v1/codex/runtimes/([0-9a-f-]{36})/(start|stop)$`)
)

func (server *Server) routeChannel(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	path := request.URL.Path
	if request.Method == http.MethodGet && path == "/v1/dynamic-tools" {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
		if err != nil || principal == nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"dynamicTools": serverchannel.DynamicToolSpecs()})
	}
	if request.Method == http.MethodPost && path == "/v1/dynamic-tools/call" {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		tool, toolOK := body["tool"].(string)
		arguments, argumentsOK := body["arguments"].(map[string]any)
		if !toolOK || !argumentsOK {
			return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "tool and arguments are required"}
		}
		invokeContext := channelInvokeContext(request, body["timeoutMs"])
		result, err := serverchannel.DispatchDynamicTool(ctx, server.channel.Capabilities(), principal, tool, arguments, invokeContext)
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"result": result})
	}
	if match := invokePattern.FindStringSubmatch(path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "cli"})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		capability, ok := body["capability"].(string)
		if !ok {
			return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "capability is required"}
		}
		params := map[string]any{}
		if raw := body["params"]; raw != nil {
			var paramsOK bool
			params, paramsOK = raw.(map[string]any)
			if !paramsOK {
				return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "params must be an object"}
			}
		}
		result, err := server.channel.Capabilities().Invoke(ctx, principal, match[1], capability, params, channelInvokeContext(request, body["timeoutMs"]))
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"result": result})
	}
	if match := sshRESTPattern.FindStringSubmatch(path); match != nil {
		if request.Method == http.MethodGet && match[2] == "keys" {
			principal, err := server.authorize(ctx, response, request, "node", authOptions{ClientType: "ssh"})
			if err != nil || principal == nil {
				return true, err
			}
			result, err := server.channel.SSH().Describe(ctx, match[1])
			return true, writeChannelResult(response, result, err)
		}
		if request.Method == http.MethodPost {
			auth := authOptions{ClientType: "ssh"}
			if match[2] == "keys" {
				auth.NodeID = match[1]
			}
			principal, err := server.authorize(ctx, response, request, "node", auth)
			if err != nil || principal == nil {
				return true, err
			}
			if match[2] == "keys" {
				body, err := server.readBody(request)
				if err != nil {
					return true, err
				}
				result, err := server.channel.SSH().Publish(ctx, principal, body)
				return true, writeChannelResult(response, result, err)
			}
			result, err := server.channel.SSH().Create(ctx, principal, match[1], request)
			return true, writeChannelResult(response, result, err)
		}
	}
	if match := runtimePattern.FindStringSubmatch(path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		node, err := server.nodes.Get(ctx, match[1], false)
		if err != nil {
			return true, err
		}
		if node == nil {
			return true, &HTTPError{Status: 404, Code: "not_found", Message: "approved node not found"}
		}
		selector, _ := body["nodeAccountId"].(string)
		account, err := nodes.SelectAccount(node, selector)
		if err != nil {
			return true, err
		}
		if account != nil {
			node = nodes.AccountNode(node, *account)
		}
		if enabled, _ := node.Capabilities["appServer"].(bool); !enabled {
			return true, &HTTPError{Status: 409, Code: "capability_unavailable", Message: "node cannot run Codex App Server"}
		}
		running := match[2] == "start"
		if running && !compatibleCodexRuntime(node.Capabilities, node.CodexInstallations, body["codexPath"]) {
			return true, &HTTPError{Status: 409, Code: "compatible_codex_unavailable", Message: "node has no Mira-compatible Codex with remote ThreadStore support"}
		}
		storeID := "personal"
		if value, ok := body["storeId"].(string); ok {
			storeID = value
		}
		if _, ok := safeStoreID(storeID); !ok {
			return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id"}
		}
		desired := map[string]any{"running": running, "listenUrl": desiredString(node.DesiredAppServer, "listenUrl", "ws://127.0.0.1:4510")}
		for _, key := range []string{"codexPath", "codexHome"} {
			if value, ok := body[key].(string); ok {
				desired[key] = value
			} else if value, ok := node.DesiredAppServer[key]; ok {
				desired[key] = value
			}
		}
		if running {
			desired["configOverrides"] = []any{
				`experimental_thread_store.type="remote_http"`,
				"experimental_thread_store.endpoint=" + quoteJSONString(server.config.Foundation.CodexStoreEndpoint),
				"experimental_thread_store.store_id=" + quoteJSONString(storeID),
			}
		} else {
			desired["configOverrides"] = []any{}
		}
		for _, key := range []string{"environmentFiles", "inheritEnv"} {
			if value, ok := body[key]; ok {
				desired[key] = value
			} else if value, ok := node.DesiredAppServer[key]; ok {
				desired[key] = value
			}
		}
		var result nodes.Result
		if account != nil {
			result, err = server.nodes.SetAccountDesired(ctx, match[1], account.NodeAccountID, desired)
		} else {
			result, err = server.nodes.SetDesiredAppServer(ctx, match[1], desired)
		}
		if err == nil && result.Status == 200 {
			resultBody, _ := result.Body.(map[string]any)
			stored := object(resultBody["desiredAppServer"])
			if account == nil || account.IsDefault {
				server.channel.UpdateProxyDesiredAppServer(match[1], stored)
			}
			action := "codex_runtime.stopped"
			if running {
				action = "codex_runtime.started"
			}
			err = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: action, Principal: principal, TargetNodeID: match[1], Request: request, Metadata: map[string]any{"storeId": storeID, "nodeAccountId": node.SelectedNodeAccountID}}, server.config.Foundation.TrustProxyHeaders)
		}
		return true, writeNodeResult(response, result, err)
	}
	return false, nil
}

func channelInvokeContext(request *http.Request, timeoutValue any) serverchannel.InvokeContext {
	options := serverchannel.InvokeContext{
		Request: request, RequestID: request.Header.Get("X-Request-Id"), ThreadID: request.Header.Get("X-Mira-Thread-Id"),
	}
	if milliseconds, ok := integer(timeoutValue); ok && milliseconds > 0 {
		options.Timeout = time.Duration(milliseconds) * time.Millisecond
	}
	return options
}

func writeChannelResult(response http.ResponseWriter, result serverchannel.Result, err error) error {
	if err != nil {
		return err
	}
	return writeJSON(response, result.Status, result.Body)
}

func compatibleCodexRuntime(capabilities map[string]any, installations []any, requested any) bool {
	if value, ok := requested.(string); ok && value != "" {
		return true
	}
	if value, _ := capabilities["codexRuntimeDownload"].(bool); value {
		return true
	}
	for _, raw := range installations {
		if installation, ok := raw.(map[string]any); ok {
			if supported, _ := installation["remoteThreadStoreSupported"].(bool); supported {
				return true
			}
		}
	}
	return false
}

func desiredString(values map[string]any, key, fallback string) string {
	if value, ok := values[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

func quoteJSONString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + replacer.Replace(value) + `"`
}
