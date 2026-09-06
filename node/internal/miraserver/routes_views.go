package miraserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
)

var (
	threadViewPattern       = regexp.MustCompile(`(?i)^/v1/codex/threads/([0-9a-f-]{36})$`)
	threadManagePattern     = regexp.MustCompile(`(?i)^/v1/codex/threads/([0-9a-f-]{36})/(archive|restore)$`)
	threadReadPattern       = regexp.MustCompile(`(?i)^/v1/codex/threads/([0-9a-f-]{36})/read$`)
	threadForkTitlePattern  = regexp.MustCompile(`(?i)^/v1/codex/threads/([0-9a-f-]{36})/fork-title$`)
	threadCostsPattern      = regexp.MustCompile(`(?i)^/v1/codex/threads/([0-9a-f-]{36})/costs$`)
	threadTranscriptPattern = regexp.MustCompile(`(?i)^/v1/codex/threads/([0-9a-f-]{36})/transcript$`)
	accountHistoryPattern   = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/account-history$`)
)

func (server *Server) routeViews(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	path := request.URL.Path
	if request.Method == http.MethodGet && path == "/v1/codex/threads" {
		if principal, err := server.authorize(ctx, response, request, "admin", authOptions{}); err != nil || principal == nil {
			return true, err
		}
		storeID, err := viewStoreID(request)
		if err != nil {
			return true, err
		}
		limit := int(boundedQueryInteger(request.URL.Query().Get("limit"), 200, 1, 500))
		archived := request.URL.Query().Get("archived") == "1"
		threads, err := server.views.ListThreads(ctx, storeID, limit, nil, &archived)
		if err != nil {
			return true, err
		}
		cleanup, err := ThreadErasureStatus(ctx, server.pool, storeID)
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"storeId": storeID, "data": threads, "cleanup": cleanup})
	}
	if match := pathMatch(accountHistoryPattern, path); request.Method == http.MethodGet && match != nil {
		if principal, err := server.authorize(ctx, response, request, "admin", authOptions{}); err != nil || principal == nil {
			return true, err
		}
		node, err := server.nodes.Get(ctx, match[1], true)
		if err != nil {
			return true, err
		}
		if node == nil {
			return true, foundation.WriteErrorJSON(response, 404, "Node not found", "not_found")
		}
		history, err := server.views.AccountHistory(ctx, node, request.URL.Query().Get("range"))
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, history)
	}
	if match := pathMatch(threadManagePattern, path); request.Method == http.MethodPost && match != nil {
		return true, server.manageThreadRequest(ctx, response, request, match[1], match[2])
	}
	if match := pathMatch(threadViewPattern, path); request.Method == http.MethodDelete && match != nil {
		return true, server.manageThreadRequest(ctx, response, request, match[1], "delete")
	}
	if match := pathMatch(threadReadPattern, path); request.Method == http.MethodPost && match != nil {
		return true, server.mutateThread(ctx, response, request, match[1], func(storeID string, body map[string]any) (operationResponse, error) {
			return MarkThreadRead(ctx, server.pool, storeID, match[1], body)
		}, false)
	}
	if match := pathMatch(threadForkTitlePattern, path); request.Method == http.MethodPost && match != nil {
		return true, server.mutateThread(ctx, response, request, match[1], func(storeID string, body map[string]any) (operationResponse, error) {
			return NameForkThread(ctx, server.pool, storeID, match[1], body)
		}, true)
	}
	if match := pathMatch(threadViewPattern, path); request.Method == http.MethodPatch && match != nil {
		return true, server.mutateThread(ctx, response, request, match[1], func(storeID string, body map[string]any) (operationResponse, error) {
			return RenameThread(ctx, server.pool, storeID, match[1], body)
		}, true)
	}
	if match := pathMatch(threadCostsPattern, path); request.Method == http.MethodGet && match != nil {
		return true, server.threadCosts(ctx, response, request, match[1])
	}
	if match := pathMatch(threadViewPattern, path); request.Method == http.MethodGet && match != nil {
		return true, server.threadView(ctx, response, request, match[1])
	}
	if match := pathMatch(threadTranscriptPattern, path); request.Method == http.MethodGet && match != nil {
		return true, server.threadTranscript(ctx, response, request, match[1])
	}
	return false, nil
}

func viewStoreID(request *http.Request) (string, error) {
	value := request.URL.Query().Get("storeId")
	if value == "" {
		value = serverviews.DefaultStoreID
	}
	return requireStoreID(value)
}

func (server *Server) readAdminMutation(ctx context.Context, response http.ResponseWriter, request *http.Request) (string, map[string]any, bool, error) {
	if principal, err := server.authorize(ctx, response, request, "admin", authOptions{}); err != nil || principal == nil {
		return "", nil, false, err
	}
	storeID, err := viewStoreID(request)
	if err != nil {
		return "", nil, false, err
	}
	body := map[string]any{}
	if err := foundation.ReadJSON(request, &body, server.config.Foundation.MaxBodyBytes); err != nil {
		return "", nil, false, err
	}
	return storeID, body, true, nil
}

func (server *Server) manageThreadRequest(ctx context.Context, response http.ResponseWriter, request *http.Request, threadID, action string) error {
	storeID, body, ok, err := server.readAdminMutation(ctx, response, request)
	if err != nil || !ok {
		return err
	}
	result, err := ManageThread(ctx, server.pool, storeID, threadID, action, body)
	if err != nil {
		return err
	}
	return writeJSON(response, result.Status, result.Body)
}

func (server *Server) mutateThread(ctx context.Context, response http.ResponseWriter, request *http.Request, threadID string, operation func(string, map[string]any) (operationResponse, error), returnThread bool) error {
	storeID, body, ok, err := server.readAdminMutation(ctx, response, request)
	if err != nil || !ok {
		return err
	}
	result, err := operation(storeID, body)
	if err != nil {
		return err
	}
	if result.Status != 200 || !returnThread {
		return writeJSON(response, result.Status, result.Body)
	}
	threads, err := server.views.ListThreads(ctx, storeID, 1, &threadID, nil)
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		return foundation.WriteErrorJSON(response, 404, "会话不存在或已不可访问", "not_found")
	}
	return writeJSON(response, 200, threads[0])
}

func (server *Server) authorizeAdminRead(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	principal, err := server.authorize(ctx, response, request, "admin", authOptions{})
	return principal != nil, err
}

func (server *Server) threadCosts(ctx context.Context, response http.ResponseWriter, request *http.Request, threadID string) error {
	if ok, err := server.authorizeAdminRead(ctx, response, request); err != nil || !ok {
		return err
	}
	storeID, err := viewStoreID(request)
	if err != nil {
		return err
	}
	turnIDs := request.URL.Query()["turnId"]
	seen := map[string]bool{}
	unique := turnIDs[:0]
	for _, id := range turnIDs {
		if id == "" || len(id) > 128 || strings.IndexFunc(id, func(r rune) bool { return r < 0x20 }) >= 0 {
			return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id or turn IDs"}
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) > 60 {
		return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id or turn IDs"}
	}
	threads, err := server.views.ListThreads(ctx, storeID, 1, &threadID, nil)
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		return foundation.WriteErrorJSON(response, 404, "会话不存在或已不可访问", "not_found")
	}
	cost, err := server.views.GetThreadCost(ctx, storeID, threads[0])
	if err != nil {
		return err
	}
	turnCosts := map[string]map[string]any{}
	if len(unique) > 0 {
		turnCosts, err = server.views.GetTurnCosts(ctx, storeID, threads[0], unique)
		if err != nil {
			return err
		}
	}
	return writeJSON(response, 200, map[string]any{
		"threadId": threadID, "generation": threads[0].Generation, "itemCount": threads[0].ItemCount,
		"costEstimate": cost, "turnCostEstimates": turnCosts,
	})
}

func (server *Server) threadView(ctx context.Context, response http.ResponseWriter, request *http.Request, threadID string) error {
	if ok, err := server.authorizeAdminRead(ctx, response, request); err != nil || !ok {
		return err
	}
	storeID, err := viewStoreID(request)
	if err != nil {
		return err
	}
	threads, err := server.views.ListThreads(ctx, storeID, 1, &threadID, nil)
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		return foundation.WriteErrorJSON(response, 404, "会话不存在或已不可访问", "not_found")
	}
	if request.URL.Query().Get("includeCost") == "1" {
		threads[0].CostEstimate, err = server.views.GetThreadCost(ctx, storeID, threads[0])
		if err != nil {
			return err
		}
	}
	return writeJSON(response, 200, threads[0])
}

func (server *Server) threadTranscript(ctx context.Context, response http.ResponseWriter, request *http.Request, threadID string) error {
	started := time.Now()
	if ok, err := server.authorizeAdminRead(ctx, response, request); err != nil || !ok {
		return err
	}
	storeID, err := viewStoreID(request)
	if err != nil {
		return err
	}
	query := request.URL.Query()
	tail := query.Get("tail") == "1"
	var cursor *string
	if values, exists := query["cursor"]; exists {
		if len(values) != 1 || values[0] == "" {
			return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id or transcript cursor"}
		}
		value := values[0]
		if !tail {
			parsed, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr != nil || parsed < 0 || parsed > 9_007_199_254_740_991 {
				return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id or transcript cursor"}
			}
		}
		cursor = &value
	}
	var toolDetails *bool
	if values, exists := query["toolDetails"]; exists {
		if len(values) != 1 || (values[0] != "0" && values[0] != "1") {
			return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id or transcript cursor"}
		}
		value := values[0] == "1"
		toolDetails = &value
	}
	limit := int(boundedQueryInteger(query.Get("limit"), 60, 10, 200))
	timings := map[string]time.Duration{}
	result, err := server.views.GetTranscript(ctx, storeID, threadID, serverviews.TranscriptOptions{
		Cursor: cursor, Limit: limit, Tail: tail, ToolDetails: toolDetails, Timings: timings,
	})
	if err != nil {
		return err
	}
	timings["handler"] = time.Since(started)
	return writeTimedTranscriptJSON(response, result.Status, result.Body, timings, started)
}

func writeTimedTranscriptJSON(response http.ResponseWriter, status int, value any, timings map[string]time.Duration, started time.Time) error {
	serializeStarted := time.Now()
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("serialize JSON response: %w", err)
	}
	timings["serialize"] = time.Since(serializeStarted)
	timings["total"] = time.Since(started)
	names := make([]string, 0, len(timings))
	for name := range timings {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s;dur=%.1f", name, float64(timings[name])/float64(time.Millisecond)))
	}
	response.Header().Set("Server-Timing", strings.Join(parts, ", "))
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if _, err := response.Write(payload); err != nil {
		return fmt.Errorf("write JSON response: %w", err)
	}
	return nil
}
