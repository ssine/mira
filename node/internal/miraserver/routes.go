package miraserver

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func (server *Server) route(ctx context.Context, response http.ResponseWriter, request *http.Request) error {
	path := request.URL.Path
	if request.Method == http.MethodGet && path == "/healthz" {
		if _, err := server.pool.Exec(ctx, "SELECT 1"); err != nil {
			return err
		}
		adminConfigured, err := server.auth.AdminConfigured(ctx)
		if err != nil {
			return err
		}
		return writeJSON(response, 200, map[string]any{
			"status": "ok", "backend": "postgresql", "databaseIsSourceOfTruth": true,
			"version": server.config.Version, "schemaVersion": foundation.CurrentSchemaVersion(), "adminConfigured": adminConfigured,
		})
	}
	if request.Method == http.MethodGet && path == "/v1/auth/config" {
		adminConfigured, err := server.auth.AdminConfigured(ctx)
		if err != nil {
			return err
		}
		return writeJSON(response, 200, map[string]any{"adminConfigured": adminConfigured, "nodeEnrollment": true, "nodeApprovalRequired": true, "passwordSession": true, "identities": []string{"admin", "node"}})
	}
	if request.Method == http.MethodPost && path == "/v1/admin/login" {
		body := map[string]any{}
		if err := foundation.ReadJSON(request, &body, server.config.Foundation.MaxBodyBytes); err != nil {
			return err
		}
		username, _ := body["username"].(string)
		password, _ := body["password"].(string)
		login, err := server.auth.Login(ctx, request, username, password)
		if err != nil {
			return err
		}
		if login == nil {
			_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "admin.login.failed", Request: request, Failed: true, ErrorCode: "invalid_credentials", Metadata: map[string]any{"username": truncate(username, 128)}}, server.config.Foundation.TrustProxyHeaders)
			return foundation.WriteErrorJSON(response, 401, "invalid username or password", "invalid_credentials")
		}
		if err := foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "admin.login.succeeded", Principal: login.Principal, Request: request}, server.config.Foundation.TrustProxyHeaders); err != nil {
			return err
		}
		headers := http.Header{"Set-Cookie": []string{login.Cookie}}
		return foundation.WriteJSON(response, 200, map[string]any{"user": map[string]any{"username": login.Principal.Username}, "csrfToken": login.CSRFToken, "expiresAt": login.Principal.ExpiresAt.UTC().Format(time.RFC3339Nano)}, headers)
	}
	if request.Method == http.MethodGet && path == "/v1/admin/session" {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return err
		}
		csrf, err := server.auth.RefreshCSRF(ctx, principal)
		if err != nil {
			return err
		}
		return writeJSON(response, 200, map[string]any{"user": map[string]any{"username": principal.Username}, "csrfToken": csrf, "expiresAt": principal.ExpiresAt.UTC().Format(time.RFC3339Nano)})
	}
	if request.Method == http.MethodPost && path == "/v1/admin/logout" {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return err
		}
		cookie, err := server.auth.Logout(ctx, principal)
		if err != nil {
			return err
		}
		if err := foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "admin.logout", Principal: principal, Request: request}, server.config.Foundation.TrustProxyHeaders); err != nil {
			return err
		}
		return foundation.WriteJSON(response, 200, map[string]any{"status": "logged_out"}, http.Header{"Set-Cookie": []string{cookie}})
	}
	if handled, err := server.routeNodes(ctx, response, request); handled || err != nil {
		return err
	}
	if handled, err := server.routeChannel(ctx, response, request); handled || err != nil {
		return err
	}
	if handled, err := server.routeImports(ctx, response, request); handled || err != nil {
		return err
	}
	if handled, err := server.routeViews(ctx, response, request); handled || err != nil {
		return err
	}
	if request.Method == http.MethodGet && path == "/v1/capabilities" {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "cli"})
		if err != nil || principal == nil {
			return err
		}
		return writeJSON(response, 200, map[string]any{
			"storageModel": "postgresql-event-log", "eventFormatVersion": 1, "adapterProtocolVersion": 2,
			"snapshotProjection": true, "nodeRegistry": true, "nodeUserMetadata": true, "nodeCapabilityChannel": true,
			"appServerProxy": true, "dynamicTools": true, "androidNodeApp": true, "imageToolResults": true,
			"databaseIsSourceOfTruth": true, "authenticationVersion": 1, "identities": []string{"admin", "node"},
			"nodeApprovalRequired": true, "websocketQueryCredentials": false,
		})
	}
	if match := pathMatch(v1StorePattern, path); match != nil {
		storeID, err := requireStoreID(match[1])
		if err != nil {
			return err
		}
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
		if err != nil || principal == nil {
			return err
		}
		switch request.Method {
		case http.MethodGet:
			value, err := GetSnapshot(ctx, server.pool, storeID)
			if err != nil {
				return err
			}
			return writeJSON(response, 200, value)
		case http.MethodPut:
			body := map[string]any{}
			if err := foundation.ReadJSON(request, &body, server.config.Foundation.MaxBodyBytes); err != nil {
				return err
			}
			result, err := PutSnapshot(ctx, server.pool, storeID, body, request.Header)
			if err != nil {
				return err
			}
			return writeJSON(response, result.Status, result.Body)
		}
	}
	if request.Method == http.MethodGet {
		if match := pathMatch(v2StorePattern, path); match != nil {
			principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
			if err != nil || principal == nil {
				return err
			}
			storeID, err := requireStoreID(match[1])
			if err != nil {
				return err
			}
			var ids []string
			if threadID, exists := request.URL.Query()["threadId"]; exists {
				if len(threadID) != 1 || threadID[0] == "" || len(threadID[0]) > 256 {
					return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid thread id"}
				}
				ids = threadID
			}
			value, err := GetStoreHead(ctx, server.pool, storeID, ids)
			if err != nil {
				return err
			}
			return writeJSON(response, 200, value)
		}
	}
	if request.Method == http.MethodGet {
		if match := pathMatch(v2HistoryPattern, path); match != nil {
			principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
			if err != nil || principal == nil {
				return err
			}
			storeID, err := requireStoreID(match[1])
			if err != nil {
				return err
			}
			threadID, err := url.PathUnescape(match[2])
			if err != nil || threadID == "" || len(threadID) > 256 {
				return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store, thread id, generation, or version"}
			}
			generation, validGeneration := parseOptionalInt(request.URL.Query().Get("generation"), 1)
			through, validThrough := parseOptionalInt(request.URL.Query().Get("throughVersion"), 0)
			if !validGeneration || !validThrough {
				return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store, thread id, generation, or version"}
			}
			result, err := GetThreadHistory(ctx, server.pool, storeID, threadID, generation, through)
			if err != nil {
				return err
			}
			return writeJSON(response, result.Status, result.Body)
		}
	}
	if request.Method == http.MethodPost {
		if match := pathMatch(v2CommitPattern, path); match != nil {
			principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
			if err != nil || principal == nil {
				return err
			}
			storeID, err := requireStoreID(match[1])
			if err != nil {
				return err
			}
			body := map[string]any{}
			if err := foundation.ReadJSON(request, &body, server.config.Foundation.MaxBodyBytes); err != nil {
				return err
			}
			result, err := CommitDelta(ctx, server.pool, storeID, body, request.Header)
			if err != nil {
				return err
			}
			return writeJSON(response, result.Status, result.Body)
		}
	}
	if request.Method == http.MethodGet {
		if match := pathMatch(v1StoreEventsPattern, path); match != nil {
			principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
			if err != nil || principal == nil {
				return err
			}
			storeID, err := requireStoreID(match[1])
			if err != nil {
				return err
			}
			after := boundedQueryInteger(request.URL.Query().Get("after"), 0, 0, 1<<53-1)
			limit := boundedQueryInteger(request.URL.Query().Get("limit"), 100, 1, 1000)
			data, err := ListStoreEvents(ctx, server.pool, storeID, after, int(limit))
			if err != nil {
				return err
			}
			return writeJSON(response, 200, map[string]any{"data": data})
		}
	}
	if request.Method == http.MethodGet {
		if match := pathMatch(v1ThreadEventsPattern, path); match != nil {
			principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
			if err != nil || principal == nil {
				return err
			}
			storeID, err := requireStoreID(match[1])
			if err != nil {
				return err
			}
			threadID, err := url.PathUnescape(match[2])
			if err != nil || threadID == "" || len(threadID) > 256 {
				return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store or thread id"}
			}
			generation, valid := parseOptionalInt(request.URL.Query().Get("generation"), 1)
			if !valid {
				return &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store or thread id"}
			}
			after := boundedQueryInteger(request.URL.Query().Get("after"), 0, 0, 1<<53-1)
			limit := boundedQueryInteger(request.URL.Query().Get("limit"), 100, 1, 1000)
			data, err := ListThreadEvents(ctx, server.pool, storeID, threadID, generation, after, int(limit))
			if err != nil {
				return err
			}
			return writeJSON(response, 200, map[string]any{"data": data})
		}
	}
	if request.Method == http.MethodPost {
		if match := pathMatch(v1RebuildPattern, path); match != nil {
			principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
			if err != nil || principal == nil {
				return err
			}
			storeID, err := requireStoreID(match[1])
			if err != nil {
				return err
			}
			result, err := RebuildSnapshot(ctx, server.pool, storeID)
			if err != nil {
				return err
			}
			return writeJSON(response, result.Status, result.Body)
		}
	}
	return foundation.WriteErrorJSON(response, 404, "not found", "not_found")
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}

func queryInt(query map[string][]string, name string) (int64, bool) {
	values := query[name]
	if len(values) != 1 {
		return 0, false
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	return value, err == nil
}
