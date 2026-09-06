package miraserver

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
	miranodes "github.com/ssine/mira/node/internal/miraserver/nodes"
)

var (
	enrollmentPattern      = regexp.MustCompile(`(?i)^/v1/node-enrollments/([0-9a-f-]{36})$`)
	adminEnrollmentPattern = regexp.MustCompile(`(?i)^/v1/admin/enrollments/([0-9a-f-]{36})/(approve|reject)$`)
	nodePattern            = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})$`)
	heartbeatPattern       = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/heartbeat$`)
	desiredPattern         = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/desired-app-server$`)
	revokePattern          = regexp.MustCompile(`(?i)^/v1/admin/nodes/([0-9a-f-]{36})/revoke$`)
	metadataPattern        = regexp.MustCompile(`(?i)^/v1/admin/nodes/([0-9a-f-]{36})/metadata$`)
	restorePattern         = regexp.MustCompile(`(?i)^/v1/admin/nodes/([0-9a-f-]{36})/restore$`)
	capabilityNamePattern  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{0,63}$`)
	labelKeyPattern        = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)
)

func (server *Server) routeNodes(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	path := request.URL.Path
	if request.Method == http.MethodPost && path == "/v1/node-enrollments" {
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		result, err := server.nodes.CreateEnrollment(ctx, request, body)
		return true, writeNodeResult(response, result, err)
	}
	if match := enrollmentPattern.FindStringSubmatch(path); request.Method == http.MethodGet && match != nil {
		result, err := server.nodes.GetEnrollment(ctx, request, match[1])
		return true, writeNodeResult(response, result, err)
	}
	if request.Method == http.MethodGet && path == "/v1/admin/enrollments" {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		var status *string
		if value, exists := request.URL.Query()["status"]; exists {
			if len(value) != 1 || (value[0] != "pending" && value[0] != "approved" && value[0] != "rejected" && value[0] != "expired") {
				return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid enrollment status"}
			}
			status = &value[0]
		}
		data, err := server.nodes.ListEnrollments(ctx, status)
		if err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"data": data})
	}
	if match := adminEnrollmentPattern.FindStringSubmatch(path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		var result miranodes.Result
		if match[2] == "approve" {
			result, err = server.nodes.ApproveEnrollment(ctx, request, principal, match[1], body)
		} else {
			result, err = server.nodes.RejectEnrollment(ctx, request, principal, match[1], body)
		}
		return true, writeNodeResult(response, result, err)
	}
	if request.Method == http.MethodPost && path == "/v1/nodes/register" {
		principal, err := server.authorize(ctx, response, request, "node", authOptions{ClientType: "node"})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		result, err := server.nodes.RegisterNode(ctx, principal.NodeID, body)
		return true, writeNodeResult(response, result, err)
	}
	if request.Method == http.MethodGet && path == "/v1/nodes" {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "cli"})
		if err != nil || principal == nil {
			return true, err
		}
		query := request.URL.Query()
		view, capability, status := query.Get("view"), query.Get("capability"), query.Get("status")
		if view == "" {
			view = "full"
		}
		labels := query["label"]
		if (view != "full" && view != "summary") || (capability != "" && !capabilityNamePattern.MatchString(capability)) || (status != "" && status != "online" && status != "offline") || len(labels) > 16 {
			return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid Node list filter"}
		}
		labelFilters := [][2]string{}
		for _, filter := range labels {
			key, value, found := strings.Cut(filter, "=")
			if !found || key == "" || value == "" || !labelKeyPattern.MatchString(key) {
				return true, &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid Node list filter"}
			}
			labelFilters = append(labelFilters, [2]string{key, value})
		}
		items, err := server.nodes.ListNodes(ctx, principal.Kind == "admin" && query.Get("includeRevoked") == "true")
		if err != nil {
			return true, err
		}
		data := []any{}
		for _, item := range items {
			if capability != "" && item.Capabilities[capability] != true {
				continue
			}
			if status != "" && item.Status != status {
				continue
			}
			matched := true
			for _, filter := range labelFilters {
				if value, ok := item.Labels[filter[0]].(string); !ok || value != filter[1] {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if view == "summary" {
				data = append(data, miranodes.NodeSummary(item))
			} else {
				data = append(data, server.nodeView(item))
			}
		}
		return true, writeJSON(response, 200, map[string]any{"data": data})
	}
	if request.Method == http.MethodGet && path == "/v1/nodes/resolve" {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "cli"})
		if err != nil || principal == nil {
			return true, err
		}
		result, err := server.nodes.ResolveNode(ctx, request.URL.Query().Get("selector"), principal.Kind == "admin")
		return true, writeNodeResult(response, result, err)
	}
	if match := nodePattern.FindStringSubmatch(path); request.Method == http.MethodGet && match != nil {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "cli"})
		if err != nil || principal == nil {
			return true, err
		}
		node, err := server.nodes.GetNode(ctx, match[1], principal.Kind == "admin")
		if err != nil {
			return true, err
		}
		if node == nil {
			return true, foundation.WriteErrorJSON(response, 404, "Node not found", "not_found")
		}
		return true, writeJSON(response, 200, server.nodeView(*node))
	}
	if match := heartbeatPattern.FindStringSubmatch(path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "node", authOptions{ClientType: "node", NodeID: match[1]})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		result, err := server.nodes.HeartbeatNode(ctx, match[1], body)
		return true, writeNodeResult(response, result, err)
	}
	if match := desiredPattern.FindStringSubmatch(path); request.Method == http.MethodPut && match != nil {
		principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "cli"})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		result, err := server.nodes.SetDesiredAppServer(ctx, match[1], body)
		if err == nil && result.Status == 200 {
			resultBody, _ := result.Body.(map[string]any)
			server.channel.UpdateProxyDesiredAppServer(match[1], object(resultBody["desiredAppServer"]))
			_ = foundation.AppendAudit(ctx, server.pool, foundation.AuditEvent{Action: "app_server.desired.updated", Principal: principal, TargetNodeID: match[1], Request: request, Metadata: map[string]any{"running": object(resultBody["desiredAppServer"])["running"]}}, server.config.Foundation.TrustProxyHeaders)
		}
		return true, writeNodeResult(response, result, err)
	}
	if match := revokePattern.FindStringSubmatch(path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		var reason *string
		if value, ok := body["reason"].(string); ok {
			value = truncate(value, 2000)
			reason = &value
		}
		result, err := server.nodes.RevokeNode(ctx, request, principal, match[1], reason)
		if err == nil && result.Status == 200 {
			server.channel.DisconnectNode(match[1], "Node authorization revoked")
		}
		return true, writeNodeResult(response, result, err)
	}
	if match := metadataPattern.FindStringSubmatch(path); request.Method == http.MethodPut && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		result, err := server.nodes.SetNodeMetadata(ctx, request, principal, match[1], body)
		return true, writeNodeResult(response, result, err)
	}
	if match := restorePattern.FindStringSubmatch(path); request.Method == http.MethodPost && match != nil {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		body, err := server.readBody(request)
		if err != nil {
			return true, err
		}
		result, err := server.nodes.RestoreNode(ctx, request, principal, match[1], body)
		return true, writeNodeResult(response, result, err)
	}
	if request.Method == http.MethodGet && path == "/v1/admin/audit-events" {
		principal, err := server.authorize(ctx, response, request, "admin", authOptions{CSRF: true})
		if err != nil || principal == nil {
			return true, err
		}
		limit := boundedQueryInteger(request.URL.Query().Get("limit"), 100, 1, 500)
		rows, err := server.pool.Query(ctx, `SELECT json_build_object(
		  'eventId',audit_event_id,'action',action,'actorType',actor_type,'actorAdminId',actor_admin_id,'actorNodeId',actor_node_id,
		  'clientType',client_type,'targetNodeId',target_node_id,'threadId',thread_id,'requestId',request_id,'success',success,
		  'errorCode',error_code,'requestAddress',request_address,'metadata',metadata,'createdAt',created_at)
		  FROM mira_audit_events ORDER BY audit_event_id DESC LIMIT $1`, limit)
		if err != nil {
			return true, err
		}
		defer rows.Close()
		data := []any{}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return true, err
			}
			var item any
			if err := json.Unmarshal(raw, &item); err != nil {
				return true, err
			}
			data = append(data, item)
		}
		if err := rows.Err(); err != nil {
			return true, err
		}
		return true, writeJSON(response, 200, map[string]any{"data": data})
	}
	return false, nil
}

func (server *Server) nodeView(node miranodes.Node) map[string]any {
	raw, err := json.Marshal(node)
	if err != nil {
		return map[string]any{"nodeId": node.NodeID}
	}
	result, err := decodeObject(raw)
	if err != nil {
		return map[string]any{"nodeId": node.NodeID}
	}
	result["sshSessionCount"] = server.channel.SSH().SessionCount(node.NodeID)
	return result
}

func (server *Server) readBody(request *http.Request) (map[string]any, error) {
	body := map[string]any{}
	err := foundation.ReadJSON(request, &body, server.config.Foundation.MaxBodyBytes)
	return body, err
}

func writeNodeResult(response http.ResponseWriter, result miranodes.Result, err error) error {
	if err != nil {
		return err
	}
	return writeJSON(response, result.Status, result.Body)
}
