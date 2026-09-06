package channel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var clientRequestIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func proxyActorKey(caller *foundation.Principal) string {
	subject := caller.SubjectID
	if subject == "" {
		subject = caller.NodeID
	}
	if subject == "" {
		subject = "unknown"
	}
	return caller.Kind + ":" + subject
}

func (channel *Channel) attachProxy(targetNodeID string, connection *socket, caller *foundation.Principal, storeID string, target *nodes.Node) {
	if !channel.IsConnected(targetNodeID) {
		connection.close(websocket.CloseTryAgainLater, "node capability channel is offline")
		return
	}
	sessionID, err := randomUUID()
	if err != nil {
		connection.close(websocket.CloseInternalServerErr, "could not create proxy session")
		return
	}
	proxy := &proxy{
		targetNodeID: targetNodeID, actorKey: proxyActorKey(caller), sessionID: sessionID,
		socket: connection, storeID: storeID, target: target,
		threadRequestBindings: map[string]*string{}, idempotentThreadStarts: map[string]string{},
		boundThreadIDs: map[string]bool{}, ephemeralStartRequests: map[string]bool{}, ephemeralThreadIDs: map[string]bool{},
		toolFreeStartRequests: map[string]bool{}, toolFreeThreadIDs: map[string]bool{},
	}
	if caller.Kind == "node" {
		proxy.callerNodeID = caller.NodeID
	}
	channel.mu.Lock()
	channel.proxies[sessionID] = proxy
	channel.mu.Unlock()
	if !channel.TrySendToNode(targetNodeID, map[string]any{"type": "appserver.open", "sessionId": sessionID}) {
		channel.mu.Lock()
		delete(channel.proxies, sessionID)
		channel.mu.Unlock()
		connection.close(websocket.CloseTryAgainLater, "node capability channel is offline")
		return
	}
	go channel.readProxy(proxy)
}

func (channel *Channel) readProxy(proxy *proxy) {
	defer channel.markProxyClientClosed(proxy, false)
	for {
		_, payload, err := proxy.socket.connection.ReadMessage()
		if err != nil {
			return
		}
		if err := channel.forwardProxyClientMessage(context.Background(), proxy, payload); err != nil {
			channel.logger.Error("App Server client message failed", "error", err)
			channel.sendProxyError(proxy, nil, "Mira could not process the App Server request", -32603)
		}
	}
}

func (channel *Channel) markProxyClientClosed(proxy *proxy, immediate bool) {
	proxy.mu.Lock()
	if proxy.clientClosed {
		startAbandon := immediate && len(proxy.idempotentThreadStarts) > 0 && !proxy.abandoning
		if startAbandon {
			proxy.abandoning = true
			if proxy.detachTimer != nil {
				proxy.detachTimer.Stop()
				proxy.detachTimer = nil
			}
		}
		proxy.mu.Unlock()
		if startAbandon {
			go channel.runAbandonProxyThreadStarts(proxy)
		}
		return
	}
	proxy.clientClosed = true
	hasStarts := len(proxy.idempotentThreadStarts) > 0
	if hasStarts && immediate {
		proxy.abandoning = true
	}
	if hasStarts && !immediate {
		proxy.detachTimer = time.AfterFunc(channel.detachGrace, func() {
			proxy.mu.Lock()
			if proxy.abandoning {
				proxy.mu.Unlock()
				return
			}
			proxy.abandoning = true
			proxy.mu.Unlock()
			channel.runAbandonProxyThreadStarts(proxy)
		})
	}
	proxy.mu.Unlock()
	if hasStarts {
		if immediate {
			go channel.runAbandonProxyThreadStarts(proxy)
		}
		return
	}
	channel.cleanupProxy(proxy)
}

func (channel *Channel) runAbandonProxyThreadStarts(proxy *proxy) {
	if err := channel.abandonProxyThreadStarts(context.Background(), proxy); err != nil {
		channel.logger.Error("failed to abandon thread/start", "error", err)
		channel.cleanupProxy(proxy)
	}
}

func (channel *Channel) cleanupProxy(proxy *proxy) {
	channel.mu.Lock()
	if channel.proxies[proxy.sessionID] != proxy {
		channel.mu.Unlock()
		return
	}
	delete(channel.proxies, proxy.sessionID)
	channel.mu.Unlock()
	proxy.mu.Lock()
	if proxy.detachTimer != nil {
		proxy.detachTimer.Stop()
		proxy.detachTimer = nil
	}
	proxy.mu.Unlock()
	channel.TrySendToNode(proxy.targetNodeID, map[string]any{"type": "appserver.close", "sessionId": proxy.sessionID})
}

func (channel *Channel) sendProxyResult(proxy *proxy, id, result any) {
	_ = proxy.socket.writeJSON(map[string]any{"id": id, "result": result})
}

func (channel *Channel) sendProxyError(proxy *proxy, id any, message string, code int) {
	_ = proxy.socket.writeJSON(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
}

func rpcKey(id any) string {
	encoded, err := json.Marshal(id)
	if err != nil {
		return fmt.Sprint(id)
	}
	if text, ok := id.(string); ok {
		return text
	}
	return string(encoded)
}

func threadStartKey(proxy *proxy, requestID string) string {
	return proxy.storeID + "\n" + proxy.actorKey + "\n" + requestID
}

func (channel *Channel) reserveThreadStart(ctx context.Context, proxy *proxy, message map[string]any) (bool, error) {
	params, _ := message["params"].(map[string]any)
	requestValue, supplied := params["miraRequestId"]
	if !supplied {
		return false, nil
	}
	delete(params, "miraRequestId")
	id, hasID := message["id"]
	requestID, valid := requestValue.(string)
	if !hasID || !valid || !clientRequestIDPattern.MatchString(requestID) {
		if !hasID {
			id = nil
		}
		channel.sendProxyError(proxy, id, "miraRequestId must be a UUID on a thread/start request", -32602)
		return true, nil
	}
	digestValue := any(params)
	if stringValue(message["method"]) == "thread/fork" {
		digestValue = map[string]any{"method": message["method"], "params": params}
	}
	digest, err := requestDigest(digestValue)
	if err != nil {
		return false, err
	}
	key := threadStartKey(proxy, requestID)
	var returned string
	err = channel.db.QueryRow(ctx, `INSERT INTO mira_appserver_thread_start_requests (
	       store_id, actor_key, client_request_id, target_node_id, request_sha256, status
	     ) VALUES ($1, $2, $3::uuid, $4::uuid, $5, 'pending')
	     ON CONFLICT (store_id, actor_key, client_request_id) DO NOTHING
	     RETURNING client_request_id::text`, proxy.storeID, proxy.actorKey, requestID, proxy.targetNodeID, digest).Scan(&returned)
	if err == nil {
		channel.mu.Lock()
		channel.threadStarts[key] = &activeThreadStart{owner: proxy}
		channel.mu.Unlock()
		proxy.mu.Lock()
		proxy.idempotentThreadStarts[rpcKey(id)] = key
		proxy.mu.Unlock()
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var existingDigest, status string
	var threadID *string
	var response []byte
	err = channel.db.QueryRow(ctx, `SELECT request_sha256, status, thread_id, response
	     FROM mira_appserver_thread_start_requests
	     WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3::uuid`,
		proxy.storeID, proxy.actorKey, requestID).Scan(&existingDigest, &status, &threadID, &response)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && existingDigest != digest) {
		channel.sendProxyError(proxy, id, "miraRequestId was reused with different thread/start parameters", -32602)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if status == "completed" {
		var saved map[string]any
		if err := json.Unmarshal(response, &saved); err != nil {
			return false, err
		}
		if deleted, _ := saved["deleted"].(bool); deleted {
			channel.sendProxyError(proxy, id, "此会话已永久删除。", -32004)
			return true, nil
		}
		if threadID != nil {
			channel.bindProxyThread(proxy, *threadID, true)
		}
		channel.sendProxyResult(proxy, id, saved)
		return true, nil
	}
	channel.mu.Lock()
	active := channel.threadStarts[key]
	if status == "pending" && active != nil {
		active.waiters = append(active.waiters, threadStartWaiter{proxy: proxy, id: id})
		channel.mu.Unlock()
		return true, nil
	}
	channel.mu.Unlock()
	tag, err := channel.db.Exec(ctx, `UPDATE mira_appserver_thread_start_requests
	     SET target_node_id = $4::uuid, status = 'pending', thread_id = NULL, response = NULL, updated_at = NOW()
	     WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3::uuid
	       AND (status = 'failed' OR updated_at < NOW() - INTERVAL '30 seconds')`,
		proxy.storeID, proxy.actorKey, requestID, proxy.targetNodeID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		channel.sendProxyError(proxy, id, "thread/start with this miraRequestId is still in progress", -32001)
		return true, nil
	}
	channel.mu.Lock()
	channel.threadStarts[key] = &activeThreadStart{owner: proxy}
	channel.mu.Unlock()
	proxy.mu.Lock()
	proxy.idempotentThreadStarts[rpcKey(id)] = key
	proxy.mu.Unlock()
	return false, nil
}

func (channel *Channel) finishThreadStart(ctx context.Context, proxy *proxy, message map[string]any) error {
	id, exists := message["id"]
	if !exists {
		return nil
	}
	keyID := rpcKey(id)
	proxy.mu.Lock()
	key := proxy.idempotentThreadStarts[keyID]
	if key != "" {
		delete(proxy.idempotentThreadStarts, keyID)
	}
	proxy.mu.Unlock()
	if key == "" {
		return nil
	}
	parts := strings.Split(key, "\n")
	if len(parts) != 3 {
		return fmt.Errorf("invalid thread start key")
	}
	var threadID string
	if message["error"] == nil {
		result, _ := message["result"].(map[string]any)
		thread, _ := result["thread"].(map[string]any)
		threadID, _ = thread["id"].(string)
	}
	if threadID != "" {
		encoded, err := json.Marshal(message["result"])
		if err != nil {
			return err
		}
		_, err = channel.db.Exec(ctx, `UPDATE mira_appserver_thread_start_requests
		 SET status = 'completed', thread_id = $4,
		     response = CASE WHEN EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$4 AND action='delete')
		       THEN '{"deleted":true}'::jsonb ELSE $5::jsonb END, updated_at = NOW()
		 WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3::uuid`, parts[0], parts[1], parts[2], threadID, string(encoded))
		if err != nil {
			return err
		}
	} else {
		if _, err := channel.db.Exec(ctx, `UPDATE mira_appserver_thread_start_requests
		 SET status = 'failed', thread_id = NULL, response = NULL, updated_at = NOW()
		 WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3::uuid`, parts[0], parts[1], parts[2]); err != nil {
			return err
		}
	}
	channel.mu.Lock()
	active := channel.threadStarts[key]
	delete(channel.threadStarts, key)
	channel.mu.Unlock()
	if active != nil {
		for _, waiter := range active.waiters {
			if threadID != "" {
				channel.bindProxyThread(waiter.proxy, threadID, true)
			}
			replay := make(map[string]any, len(message))
			for name, value := range message {
				replay[name] = value
			}
			replay["id"] = waiter.id
			_ = waiter.proxy.socket.writeJSON(replay)
		}
	}
	proxy.mu.Lock()
	closed, remaining := proxy.clientClosed, len(proxy.idempotentThreadStarts)
	proxy.mu.Unlock()
	if closed && remaining == 0 {
		channel.cleanupProxy(proxy)
	}
	return nil
}

func (channel *Channel) abandonProxyThreadStarts(ctx context.Context, proxy *proxy) error {
	proxy.mu.Lock()
	keys := make([]string, 0, len(proxy.idempotentThreadStarts))
	for _, key := range proxy.idempotentThreadStarts {
		keys = append(keys, key)
	}
	clear(proxy.idempotentThreadStarts)
	proxy.mu.Unlock()
	for _, key := range keys {
		parts := strings.Split(key, "\n")
		if len(parts) != 3 {
			continue
		}
		if _, err := channel.db.Exec(ctx, `UPDATE mira_appserver_thread_start_requests
		 SET status = 'failed', thread_id = NULL, response = NULL, updated_at = NOW()
		 WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3::uuid AND status = 'pending'`, parts[0], parts[1], parts[2]); err != nil {
			return err
		}
		channel.mu.Lock()
		active := channel.threadStarts[key]
		delete(channel.threadStarts, key)
		channel.mu.Unlock()
		if active != nil {
			for _, waiter := range active.waiters {
				channel.sendProxyError(waiter.proxy, waiter.id, "original thread/start connection was lost", -32002)
			}
		}
	}
	channel.cleanupProxy(proxy)
	return nil
}

func (channel *Channel) forwardAppServerMessage(ctx context.Context, proxy *proxy, payload []byte) error {
	message, err := decodeObject(payload)
	if err != nil {
		return proxy.socket.writeText(payload)
	}
	if err := channel.finishThreadStart(ctx, proxy, message); err != nil {
		return err
	}
	params, _ := message["params"].(map[string]any)
	result, _ := message["result"].(map[string]any)
	reportedThread, _ := params["thread"].(map[string]any)
	if reportedThread == nil {
		reportedThread, _ = result["thread"].(map[string]any)
	}
	if ephemeral, _ := reportedThread["ephemeral"].(bool); ephemeral {
		if id, ok := reportedThread["id"].(string); ok {
			proxy.mu.Lock()
			proxy.ephemeralThreadIDs[id] = true
			proxy.mu.Unlock()
		}
	}
	observedID, _ := params["threadId"].(string)
	if observedID == "" {
		if thread, _ := params["thread"].(map[string]any); thread != nil {
			observedID, _ = thread["id"].(string)
		}
	}
	method, _ := message["method"].(string)
	if (strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "item/")) && observedID != "" {
		channel.bindProxyThread(proxy, observedID, false)
	}
	if id, exists := message["id"]; exists {
		key := rpcKey(id)
		proxy.mu.Lock()
		requested, bound := proxy.threadRequestBindings[key]
		if bound {
			delete(proxy.threadRequestBindings, key)
		}
		wasEphemeral := proxy.ephemeralStartRequests[key]
		delete(proxy.ephemeralStartRequests, key)
		wasToolFree := proxy.toolFreeStartRequests[key]
		delete(proxy.toolFreeStartRequests, key)
		proxy.mu.Unlock()
		if bound {
			threadID := ""
			if message["error"] == nil {
				if thread, _ := result["thread"].(map[string]any); thread != nil {
					threadID, _ = thread["id"].(string)
				}
				if threadID == "" && requested != nil {
					threadID = *requested
				}
			}
			if threadID != "" {
				proxy.mu.Lock()
				if wasEphemeral {
					proxy.ephemeralThreadIDs[threadID] = true
				}
				if wasToolFree {
					proxy.toolFreeThreadIDs[threadID] = true
				}
				proxy.mu.Unlock()
				channel.bindProxyThread(proxy, threadID, true)
			}
		}
	}
	if method == "item/tool/call" && stringValue(params["namespace"]) == DynamicToolNamespace {
		id, exists := message["id"]
		if !exists {
			return nil
		}
		threadID, _ := params["threadId"].(string)
		proxy.mu.Lock()
		disabled := proxy.toolFreeThreadIDs[threadID]
		primary := proxy.threadID
		proxy.mu.Unlock()
		if disabled {
			channel.TrySendToNode(proxy.targetNodeID, map[string]any{"type": "appserver.message", "sessionId": proxy.sessionID,
				"payload": mustJSON(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Tools are disabled for this temporary thread"}})})
			return nil
		}
		arguments, _ := params["arguments"].(map[string]any)
		actor := &foundation.Principal{Kind: "node", NodeID: proxy.targetNodeID, ClientType: "app-server", Transport: "internal"}
		metadata := map[string]any{"source": "app-server"}
		if value, ok := params["parentThreadId"].(string); ok {
			metadata["parentThreadId"] = value
		}
		if value, ok := params["subagentThreadId"].(string); ok {
			metadata["subagentThreadId"] = value
		}
		value, callErr := DispatchDynamicTool(ctx, channel.capabilities, actor, stringValue(params["tool"]), arguments,
			InvokeContext{RequestID: rpcKey(id), ThreadID: firstNonempty(threadID, primary), AuditMetadata: metadata})
		var content []map[string]any
		success := callErr == nil
		if callErr != nil {
			content = []map[string]any{{"type": "inputText", "text": callErr.Error()}}
		} else {
			content = DynamicToolContentItems(stringValue(params["tool"]), value)
		}
		channel.TrySendToNode(proxy.targetNodeID, map[string]any{"type": "appserver.message", "sessionId": proxy.sessionID,
			"payload": mustJSON(map[string]any{"id": id, "result": map[string]any{"contentItems": content, "success": success}})})
		return nil
	}
	return proxy.socket.writeText(payload)
}

func (channel *Channel) bindProxyThread(proxy *proxy, threadID string, primary bool) {
	proxy.mu.Lock()
	if proxy.ephemeralThreadIDs[threadID] {
		proxy.mu.Unlock()
		return
	}
	if primary || proxy.threadID == "" {
		proxy.threadID = threadID
	}
	if proxy.boundThreadIDs[threadID] {
		proxy.mu.Unlock()
		return
	}
	proxy.boundThreadIDs[threadID] = true
	storeID, nodeID := proxy.storeID, proxy.targetNodeID
	proxy.mu.Unlock()
	go func() {
		_, err := channel.db.Exec(context.Background(), `INSERT INTO mira_codex_thread_runtimes (store_id, thread_id, node_id, bound_at)
		 SELECT $1, $2, $3::uuid, NOW() WHERE NOT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete')
		 ON CONFLICT (store_id, thread_id) DO UPDATE SET node_id = EXCLUDED.node_id, bound_at = EXCLUDED.bound_at`, storeID, threadID, nodeID)
		if err != nil {
			channel.logger.Error("thread runtime binding failed", "error", err)
		}
	}()
}

func (channel *Channel) forwardProxyClientMessage(ctx context.Context, proxy *proxy, payload []byte) error {
	message, err := decodeObject(payload)
	if err != nil {
		if !channel.TrySendToNode(proxy.targetNodeID, map[string]any{"type": "appserver.message", "sessionId": proxy.sessionID, "payload": string(payload)}) {
			proxy.socket.close(websocket.CloseInternalServerErr, "node disconnected")
		}
		return nil
	}
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
		message["params"] = params
	}
	if threadID, ok := params["threadId"].(string); ok && methodUsesExistingThread(method) {
		if err := channel.assertThreadsNotDeleted(ctx, proxy.storeID, []string{threadID}); err != nil {
			if typed, ok := err.(*Error); ok && typed.Code == "thread_deleted" {
				id := message["id"]
				channel.sendProxyError(proxy, id, typed.Message, -32004)
				return nil
			}
			return err
		}
	}
	if method == "initialize" {
		capabilities, _ := params["capabilities"].(map[string]any)
		if capabilities == nil {
			capabilities = map[string]any{}
			params["capabilities"] = capabilities
		}
		capabilities["experimentalApi"] = true
	}
	if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
		proxy.mu.Lock()
		target := proxy.target
		proxy.mu.Unlock()
		if method == "thread/start" || method == "thread/fork" {
			if _, supplied := params["miraRequestId"]; supplied {
				handled, err := channel.reserveThreadStart(ctx, proxy, message)
				if err != nil || handled {
					return err
				}
			}
		}
		if params["approvalPolicy"] == nil {
			params["approvalPolicy"] = "never"
		}
		if params["sandbox"] == nil {
			params["sandbox"] = "danger-full-access"
		}
		dynamicTools, isArray := params["dynamicTools"].([]any)
		toolFree := method == "thread/start" && params["ephemeral"] == true && isArray && len(dynamicTools) == 0
		if method != "thread/fork" && !toolFree {
			params["dynamicTools"] = mergeDynamicTools(params["dynamicTools"])
		}
		if method == "thread/start" {
			cwd, ok := params["cwd"].(string)
			if !ok || strings.TrimSpace(cwd) == "" {
				if fallback := targetDefaultCWD(target); fallback != "" {
					params["cwd"] = fallback
				}
			}
		}
		if !toolFree {
			configured, err := channel.nodeDeveloperInstructions(ctx, proxy, message["id"])
			if err != nil {
				response := map[string]any{"id": message["id"], "error": map[string]any{"code": -32005, "message": err.Error()}}
				if finishErr := channel.finishThreadStart(ctx, proxy, response); finishErr != nil {
					return finishErr
				}
				channel.sendProxyError(proxy, message["id"], err.Error(), -32005)
				return nil
			}
			if merged, ok := mergeDeveloperInstructions(params["developerInstructions"], miraCLIInstructions(target), configured); ok {
				params["developerInstructions"] = merged
			} else {
				delete(params, "developerInstructions")
			}
		}
		if id, exists := message["id"]; exists {
			key := rpcKey(id)
			proxy.mu.Lock()
			if params["ephemeral"] == true {
				proxy.ephemeralStartRequests[key] = true
			}
			if toolFree {
				proxy.toolFreeStartRequests[key] = true
			}
			proxy.mu.Unlock()
		}
	}
	if id, exists := message["id"]; exists && (method == "thread/start" || method == "thread/resume" || method == "thread/fork" || method == "turn/start") {
		var thread *string
		if value, ok := params["threadId"].(string); ok {
			thread = &value
		}
		proxy.mu.Lock()
		proxy.threadRequestBindings[rpcKey(id)] = thread
		proxy.mu.Unlock()
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if !channel.TrySendToNode(proxy.targetNodeID, map[string]any{"type": "appserver.message", "sessionId": proxy.sessionID, "payload": string(encoded)}) {
		proxy.socket.close(websocket.CloseInternalServerErr, "node disconnected")
	}
	return nil
}

func (channel *Channel) nodeDeveloperInstructions(ctx context.Context, proxy *proxy, requestID any) (string, error) {
	proxy.mu.Lock()
	target := proxy.target
	proxy.mu.Unlock()
	path, err := targetDeveloperInstructionsFile(target)
	if err != nil || path == "" {
		return "", err
	}
	files, _ := target.Capabilities["files"].(bool)
	if !files || channel.capabilities == nil {
		return "", channelError("the configured Node Developer instructions file cannot be read because file access is unavailable", 409, "developer_instructions_file_unavailable")
	}
	actor := &foundation.Principal{Kind: "node", NodeID: proxy.targetNodeID, ClientType: "app-server", Transport: "internal"}
	requestKey := ""
	if requestID != nil {
		requestKey = rpcKey(requestID)
	}
	result, err := channel.capabilities.Invoke(ctx, actor, proxy.targetNodeID, "file", map[string]any{
		"action": "read", "path": path, "offset": int64(0), "length": int64(MaxDeveloperInstructionsFile + 1), "encoding": "base64",
	}, InvokeContext{RequestID: requestKey, Timeout: 5 * time.Second, AuditMetadata: map[string]any{"source": "app-server-developer-instructions"}})
	if err != nil {
		status := 500
		if typed, ok := err.(*Error); ok {
			status = typed.Status
		}
		return "", channelError("could not read the configured Node Developer instructions file: "+err.Error(), status, "developer_instructions_file_read_failed")
	}
	record, ok := result.(map[string]any)
	if !ok {
		return "", invalidDeveloperInstructionsResponse()
	}
	content, contentOK := record["content"].(string)
	encoding, encodingOK := record["encoding"].(string)
	bytesRead, integerOK := integerValue(record["bytesRead"])
	eof, eofOK := record["eof"].(bool)
	decoded, decodeErr := base64.StdEncoding.DecodeString(content)
	if !contentOK || !encodingOK || encoding != "base64" || !integerOK || bytesRead < 0 || !eofOK || decodeErr != nil || base64.StdEncoding.EncodeToString(decoded) != content {
		return "", invalidDeveloperInstructionsResponse()
	}
	if int64(len(decoded)) != bytesRead {
		return "", channelError("the Node returned an inconsistent Developer instructions file length", 502, "invalid_developer_instructions_file_response")
	}
	if len(decoded) > MaxDeveloperInstructionsFile || !eof {
		return "", channelError(fmt.Sprintf("the configured Node Developer instructions file exceeds %d bytes", MaxDeveloperInstructionsFile), 413, "developer_instructions_file_too_large")
	}
	if !utf8.Valid(decoded) {
		return "", channelError("the configured Node Developer instructions file is not valid UTF-8", 400, "invalid_developer_instructions_file_encoding")
	}
	contentText := string(decoded)
	for _, marker := range []string{miraCLIInstructionsBegin, miraCLIInstructionsEnd, nodeDeveloperInstructionsBegin, nodeDeveloperInstructionsEnd} {
		if strings.Contains(contentText, marker) {
			return "", channelError("the configured Node Developer instructions file contains reserved Mira content", 400, "invalid_developer_instructions_file_content")
		}
	}
	if strings.ContainsRune(contentText, 0) {
		return "", channelError("the configured Node Developer instructions file contains reserved Mira content", 400, "invalid_developer_instructions_file_content")
	}
	if contentText == "" {
		return "", nil
	}
	return strings.Join([]string{nodeDeveloperInstructionsBegin, "The following persistent Developer instructions were loaded by Mira for this execution Node.", contentText, nodeDeveloperInstructionsEnd}, "\n"), nil
}

func invalidDeveloperInstructionsResponse() error {
	return channelError("the Node returned an invalid Developer instructions file response", 502, "invalid_developer_instructions_file_response")
}

func (channel *Channel) assertThreadsNotDeleted(ctx context.Context, storeID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var found string
	err := channel.db.QueryRow(ctx, `SELECT thread_id FROM mira_thread_actions WHERE store_id=$1 AND action='delete' AND thread_id=ANY($2::text[]) LIMIT 1`, storeID, ids).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return channelError("此会话已永久删除，不能继续写入或恢复。", http.StatusGone, "thread_deleted")
}

func methodUsesExistingThread(method string) bool {
	switch method {
	case "thread/resume", "thread/fork", "thread/read", "thread/turns/list", "thread/items/list", "turn/start", "turn/steer":
		return true
	}
	return false
}

func mustJSON(value any) string { encoded, _ := json.Marshal(value); return string(encoded) }
func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
