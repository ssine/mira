package imports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

var storeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

func (service *Service) defaultStoreID() string {
	if service.StoreID != "" {
		return service.StoreID
	}
	return DefaultStoreID
}

func (service *Service) ScanCodexSessions(
	ctx context.Context,
	principal *foundation.Principal,
	nodeID string,
	request *http.Request,
	storeIDs ...string,
) (map[string]any, error) {
	if service.Repository == nil || service.Capability == nil {
		return nil, fmt.Errorf("session import dependencies are incomplete")
	}
	value, err := service.Capability.Invoke(ctx, principal, nodeID, "codexSessions", map[string]any{"action": "list"}, channel.InvokeContext{
		Request: request, Timeout: 120 * time.Second, AuditMetadata: map[string]any{"purpose": "codex_session_scan"},
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid Codex session scan result")
	}
	nodes, err := service.Repository.ApprovedNodes(ctx)
	if err != nil {
		return nil, err
	}
	var source *NodeRuntime
	for index := range nodes {
		if nodes[index].NodeID == nodeID {
			source = &nodes[index]
			break
		}
	}
	rawSessions, _ := result["sessions"].([]any)
	sessions := make([]map[string]any, 0, len(rawSessions))
	paths := make([]string, 0, len(rawSessions))
	for _, raw := range rawSessions {
		session, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		copy := make(map[string]any, len(session)+3)
		for key, item := range session {
			copy[key] = item
		}
		executionMode := inferredExecutionMode(copy, source)
		copy["executionMode"] = executionMode
		copy["storageNodeId"] = nodeID
		copy["suggestedRuntimeNodeId"] = suggestedRuntimeNodeID(executionMode, source, nodes)
		sessions = append(sessions, copy)
		paths = append(paths, stringValue(copy["path"]))
	}
	storeID := service.defaultStoreID()
	if len(storeIDs) > 1 || (len(storeIDs) == 1 && !storeIDPattern.MatchString(storeIDs[0])) {
		return nil, importError(http.StatusBadRequest, "invalid_request", "invalid store id")
	}
	if len(storeIDs) == 1 {
		storeID = storeIDs[0]
	}
	previous, err := service.Repository.PreviousImports(ctx, nodeID, storeID, paths)
	if err != nil {
		return nil, err
	}
	outputSessions := make([]any, 0, len(sessions))
	for _, session := range sessions {
		prior, found := previous[stringValue(session["path"])]
		if !found {
			session["import"] = nil
		} else {
			size, sizeOK := integer(session["sizeBytes"])
			modifiedAt, modifiedOK := parseDate(stringValue(session["modifiedAt"]))
			unchanged := sizeOK && size == prior.SourceSizeBytes && modifiedOK && prior.SourceModifiedAt != nil && modifiedAt.Equal(*prior.SourceModifiedAt)
			session["import"] = map[string]any{
				"importId": prior.ImportID, "status": prior.Status, "threadId": prior.ThreadID,
				"storeId": prior.StoreID, "sourceSha256": prior.SourceSHA256, "unchanged": unchanged,
				"importedAt": formatMilliseconds(prior.CreatedAt),
			}
		}
		outputSessions = append(outputSessions, session)
	}
	output := make(map[string]any, len(result)+1)
	for key, item := range result {
		output[key] = item
	}
	output["sessions"] = outputSessions
	return output, nil
}

func inferredExecutionMode(session map[string]any, source *NodeRuntime) string {
	if mode := stringValue(session["executionMode"]); mode != "" {
		return mode
	}
	if source != nil && source.NodeMode == "windows" {
		cwd := stringValue(session["cwd"])
		if strings.HasPrefix(cwd, "/") && !strings.HasPrefix(cwd, "//") {
			return "wsl"
		}
	}
	if source != nil && source.NodeMode != "" {
		return source.NodeMode
	}
	if source != nil && source.Platform != "" {
		return source.Platform
	}
	return "unknown"
}

func suggestedRuntimeNodeID(executionMode string, source *NodeRuntime, nodes []NodeRuntime) any {
	if executionMode != "wsl" {
		return nil
	}
	for _, node := range nodes {
		available, _ := node.Capabilities["appServer"].(bool)
		if node.NodeMode == "wsl" && available && (source == nil || strings.EqualFold(node.Hostname, source.Hostname)) {
			return node.NodeID
		}
	}
	return nil
}

func (service *Service) ImportCodexSession(ctx context.Context, request Request) (Result, error) {
	storeID := service.defaultStoreID()
	if value, found := request.Body["storeId"]; found && value != nil {
		storeID, _ = value.(string)
	}
	path, pathOK := request.Body["path"].(string)
	if !storeIDPattern.MatchString(storeID) || !pathOK || path == "" || len(path) > 32_768 {
		return Result{Status: http.StatusBadRequest, Body: map[string]any{"error": "valid path and storeId are required", "code": "invalid_request"}}, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	request.Progress.progress(map[string]any{"phase": "scanning"})
	scan, err := service.ScanCodexSessions(ctx, request.Principal, request.NodeID, request.HTTP, storeID)
	if err != nil {
		return Result{}, err
	}
	var summary map[string]any
	for _, raw := range array(scan["sessions"]) {
		candidate, _ := raw.(map[string]any)
		if stringValue(candidate["path"]) == path {
			summary = candidate
			break
		}
	}
	if summary == nil {
		return Result{Status: http.StatusNotFound, Body: map[string]any{"error": "Session was not found in a detected local Codex directory", "code": "not_found"}}, nil
	}
	threadID := stringValue(summary["threadId"])
	if !validUUID(threadID) {
		return Result{Status: http.StatusConflict, Body: map[string]any{"error": "Invalid thread id", "code": "invalid_session"}}, nil
	}
	if service.ThreadStore == nil {
		return Result{}, fmt.Errorf("session import ThreadStore is unavailable")
	}
	if err := service.ThreadStore.AssertNotDeleted(ctx, storeID, []string{threadID}); err != nil {
		return Result{}, err
	}
	runtimeNodeID, err := service.resolveRuntime(ctx, request.Body, summary)
	if err != nil {
		return Result{}, err
	}
	staged, err := service.StageSessionTransfer(ctx, request.Principal, request.NodeID, summary, storeID, request.HTTP, nil, request.Progress)
	if err != nil {
		return Result{}, err
	}
	result, importErr := service.finishImport(ctx, request, storeID, summary, staged, runtimeNodeID)
	if importErr != nil {
		code := errorCode(importErr)
		if markErr := service.Repository.MarkImport(context.WithoutCancel(ctx), staged.ImportID, "failed", nil, &code); markErr != nil {
			return Result{}, markErr
		}
		return Result{}, importErr
	}
	return result, nil
}

func (service *Service) resolveRuntime(ctx context.Context, body, summary map[string]any) (*string, error) {
	value, found := body["runtimeNodeId"]
	if !found || value == nil {
		value = summary["suggestedRuntimeNodeId"]
	}
	if value == nil || value == "" {
		return nil, nil
	}
	nodeID, ok := value.(string)
	if !ok || !validUUID(nodeID) {
		return nil, importError(http.StatusBadRequest, "invalid_request", "Invalid Codex runtime node id")
	}
	resolved, valid, err := service.Repository.ValidRuntime(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, importError(http.StatusConflict, "capability_unavailable", "Selected node cannot run Codex App Server")
	}
	return &resolved, nil
}

func (service *Service) finishImport(
	ctx context.Context,
	request Request,
	storeID string,
	summary map[string]any,
	staged Staged,
	runtimeNodeID *string,
) (Result, error) {
	expanded, err := service.StageSessionLineage(ctx, request.Principal, request.NodeID, summary, storeID, request.HTTP, staged, request.Progress)
	if err != nil {
		return Result{}, err
	}
	meta, err := cloneMap(staged.Meta)
	if err != nil {
		return Result{}, err
	}
	if meta["history_base"] != nil {
		meta["history_base"] = nil
		meta["forked_from_ordinal_exclusive"] = nil
		meta["subagent_history_start_ordinal"] = nil
	}
	childThreadID := stringValue(meta["id"])
	parent := meta["parent_thread_id"]
	if parent == nil {
		parent = object(object(object(meta["source"])["subagent"])["thread_spawn"])["parent_thread_id"]
	}
	createdMeta, err := cloneMap(meta)
	if err != nil {
		return Result{}, err
	}
	createdMeta["parent_thread_id"] = parent
	clearCopiedFork := expanded.AncestorCount > 0
	version := stringValue(summary["codexVersion"])
	if version == "" {
		version = "local-jsonl-import"
	}
	committed, err := service.ThreadStore.CommitImported(ctx, storeID, ImportCommit{
		ThreadID: childThreadID,
		ImportID: staged.ImportID,
		Count:    expanded.Count,
		Segments: expanded.Segments,
		Created:  createdThread(createdMeta, childThreadID),
		Metadata: metadataPatch(meta, summary),
		Normalize: func(raw json.RawMessage) (json.RawMessage, error) {
			return CanonicalRolloutItem(raw, clearCopiedFork, childThreadID)
		},
		CodexVersion:  version,
		RuntimeNodeID: runtimeNodeID,
	}, request.Progress)
	if err != nil {
		return Result{}, err
	}
	if service.Audit != nil {
		// The canonical history, runtime binding and import status have already
		// committed atomically. Audit storage is deliberately best-effort so an
		// audit outage cannot turn a successful import into a reported failure.
		_ = service.Audit(context.WithoutCancel(ctx), foundation.AuditEvent{
			Action: "codex_session.imported", Principal: request.Principal, TargetNodeID: request.NodeID,
			ThreadID: childThreadID, Request: request.HTTP,
			Metadata: map[string]any{
				"importId": staged.ImportID, "storeId": storeID, "itemCount": staged.Count,
				"sourceBytes": staged.SizeBytes, "executionMode": summary["executionMode"], "runtimeNodeId": runtimeNodeIDValue(runtimeNodeID),
			},
		})
	}
	return Result{Status: http.StatusOK, Body: map[string]any{
		"importId": staged.ImportID, "storeId": storeID, "threadId": childThreadID,
		"version": committed.Version, "duplicate": staged.Duplicate || committed.NoChange,
		"itemCount": expanded.Count, "ancestorCount": expanded.AncestorCount,
		"parentThreadId": parent, "runtimeNodeId": runtimeNodeIDValue(runtimeNodeID),
	}}, nil
}

func runtimeNodeIDValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func (service *Service) ListImportedThreads(ctx context.Context, storeID string, limit int, threadID *string, archived *bool) (any, error) {
	if storeID == "" {
		storeID = service.defaultStoreID()
	}
	if !storeIDPattern.MatchString(storeID) {
		return nil, importError(http.StatusBadRequest, "invalid_request", "invalid store id")
	}
	if service.ThreadList == nil {
		return nil, fmt.Errorf("thread list service is unavailable")
	}
	if limit == 0 {
		limit = 200
	}
	return service.ThreadList(ctx, storeID, limit, threadID, archived)
}

func parseDate(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func formatMilliseconds(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
