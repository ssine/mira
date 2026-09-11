package nodes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Register updates the descriptor of an approved Node after credential
// authentication has bound the request to nodeID.
func (service *Service) Register(ctx context.Context, nodeID string, body map[string]any) (Result, error) {
	nodeKey, err := requiredString("nodeKey", body["nodeKey"], 256)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	hostname, err := requiredString("hostname", body["hostname"], 256)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	platform, err := requiredString("platform", body["platform"], 64)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	architecture, err := requiredString("architecture", body["architecture"], 64)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	nodeMode, err := requiredString("nodeMode", body["nodeMode"], 64)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	versionValue, exists := body["nodeVersion"]
	if !exists || versionValue == nil {
		versionValue = body["agentVersion"]
	}
	nodeVersion, err := requiredString("nodeVersion", versionValue, 64)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	capabilities := object(body["capabilities"], nil)
	nodeBuild := object(body["nodeBuild"], nil)
	if buildVersion, exists := nodeBuild["version"]; exists && buildVersion != nodeVersion {
		return result(400, map[string]any{"error": "nodeBuild.version must match nodeVersion", "code": "invalid_request"}), nil
	}
	installations, ok := array(body["codexInstallations"])
	if !ok {
		installations = []any{}
	}
	buildJSON, err := marshalJSON(nodeBuild)
	if err != nil {
		return Result{}, err
	}
	capabilitiesJSON, err := marshalJSON(capabilities)
	if err != nil {
		return Result{}, err
	}
	installationsJSON, err := marshalJSON(installations)
	if err != nil {
		return Result{}, err
	}
	var returnedNodeID string
	var registeredAt, lastSeenAt time.Time
	var desiredRaw []byte
	err = service.db.QueryRow(ctx, `UPDATE codex_nodes SET
       hostname = $3, platform = $4, architecture = $5, node_mode = $6,
       node_version = $7, node_build = $8::jsonb, capabilities = $9::jsonb,
       codex_installations = $10::jsonb, last_seen_at = NOW(), updated_at = NOW()
     WHERE node_id = $1::uuid AND node_key = $2 AND approval_status = 'approved'
	     RETURNING node_id::text, desired_app_server, registered_at, last_seen_at`,
		nodeID, nodeKey, hostname, platform, architecture, nodeMode,
		nodeVersion, buildJSON, capabilitiesJSON, installationsJSON,
	).Scan(&returnedNodeID, &desiredRaw, &registeredAt, &lastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return result(403, map[string]any{"error": "node identity is revoked or does not match", "code": "node_forbidden"}), nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("register Node: %w", err)
	}
	desired, err := decodeObject(desiredRaw)
	if err != nil {
		return Result{}, err
	}
	if err := service.ensureDefaultAccount(ctx, nodeID); err != nil {
		return Result{}, err
	}
	accounts, err := service.DesiredAccounts(ctx, nodeID)
	if err != nil {
		return Result{}, err
	}
	return result(200, map[string]any{
		"nodeId": returnedNodeID, "desiredAppServer": desired,
		"codexAccounts":            accounts,
		"heartbeatIntervalSeconds": 3, "registeredAt": formatTime(registeredAt),
		"lastSeenAt": formatTime(lastSeenAt),
	}), nil
}

// Heartbeat records live Node state and returns the latest desired state.
func (service *Service) Heartbeat(ctx context.Context, nodeID string, body map[string]any) (Result, error) {
	reported := object(body["reportedAppServer"], map[string]any{"status": "unknown"})
	reportedJSON, err := marshalJSON(reported)
	if err != nil {
		return Result{}, err
	}
	optionalJSON := func(name string, objectOnly bool) (any, error) {
		value, exists := body[name]
		if !exists {
			return nil, nil
		}
		if objectOnly {
			value = object(value, nil)
		}
		encoded, encodeErr := marshalJSON(value)
		return encoded, encodeErr
	}
	installations, err := optionalJSON("codexInstallations", false)
	if err != nil {
		return Result{}, err
	}
	capabilities, err := optionalJSON("capabilities", true)
	if err != nil {
		return Result{}, err
	}
	machine, err := optionalJSON("machineStatus", true)
	if err != nil {
		return Result{}, err
	}
	var desiredRaw []byte
	var serverTime time.Time
	err = service.db.QueryRow(ctx, `UPDATE codex_nodes SET
       reported_app_server = $2::jsonb,
       codex_installations = COALESCE($3::jsonb, codex_installations),
       capabilities = COALESCE($4::jsonb, capabilities),
       machine_status = COALESCE($5::jsonb, machine_status),
       last_seen_at = NOW(), updated_at = NOW()
     WHERE node_id = $1::uuid AND approval_status = 'approved'
	     RETURNING desired_app_server, last_seen_at`,
		nodeID, reportedJSON, installations, capabilities, machine,
	).Scan(&desiredRaw, &serverTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return result(403, map[string]any{"error": "node is revoked", "code": "node_forbidden"}), nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("record Node heartbeat: %w", err)
	}
	desired, err := decodeObject(desiredRaw)
	if err != nil {
		return Result{}, err
	}
	if err := service.ensureDefaultAccount(ctx, nodeID); err != nil {
		return Result{}, err
	}
	if err := service.ReportAccounts(ctx, nodeID, body["codexAccounts"]); err != nil {
		return Result{}, err
	}
	accounts, err := service.DesiredAccounts(ctx, nodeID)
	if err != nil {
		return Result{}, err
	}
	return result(200, map[string]any{"desiredAppServer": desired, "codexAccounts": accounts, "serverTime": formatTime(serverTime)}), nil
}

// List returns approved Nodes unless includeRevoked is true.
func (service *Service) List(ctx context.Context, includeRevoked bool) ([]Node, error) {
	rows, err := service.db.Query(ctx, `SELECT `+selectNodeColumns+` FROM codex_nodes nodes
     WHERE ($1::boolean OR nodes.approval_status = 'approved') ORDER BY nodes.hostname, nodes.node_key`, includeRevoked)
	if err != nil {
		return nil, fmt.Errorf("list Nodes: %w", err)
	}
	defer rows.Close()
	result := []Node{}
	for rows.Next() {
		row, scanErr := scanNode(rows, false)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, row.Node)
	}
	return result, rows.Err()
}

// Get returns a Node by immutable ID.
func (service *Service) Get(ctx context.Context, nodeID string, includeRevoked bool) (*Node, error) {
	row, err := scanNode(service.db.QueryRow(ctx, `SELECT `+selectNodeColumns+` FROM codex_nodes nodes
     WHERE nodes.node_id = $1::uuid AND ($2::boolean OR nodes.approval_status = 'approved')`, nodeID, includeRevoked), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get Node: %w", err)
	}
	return &row.Node, nil
}

// Summary is the compact Node representation returned to dynamic tools.
func Summary(node Node) map[string]any {
	capabilities := map[string]any{}
	for name, enabled := range node.Capabilities {
		if value, ok := enabled.(bool); ok && value {
			capabilities[name] = true
		}
	}
	appServerStatus := "unsupported"
	if enabled, ok := node.Capabilities["appServer"].(bool); ok && enabled {
		appServerStatus = "unknown"
		if status, ok := node.ReportedAppServer["status"].(string); ok {
			appServerStatus = status
		}
	}
	return map[string]any{
		"nodeId": node.NodeID, "nodeKey": node.NodeKey, "displayName": nullableString(node.DisplayName),
		"aliases": node.Aliases, "labels": node.Labels, "hostname": node.Hostname,
		"platform": node.Platform, "architecture": node.Architecture, "nodeMode": node.NodeMode,
		"status": node.Status, "capabilities": capabilities, "appServerStatus": appServerStatus,
		"lastSeenAt": node.LastSeenAt,
	}
}

// Resolve finds a Node by ID, key, alias, or hostname, preserving that
// precedence and rejecting ambiguous matches at the strongest matching rank.
func (service *Service) Resolve(ctx context.Context, selector string, includeRevoked bool) (Result, error) {
	if selector == "" || jsStringLength(selector) > 256 || containsControl(selector, false) {
		return result(400, map[string]any{"error": "invalid Node selector", "code": "invalid_selector"}), nil
	}
	rows, err := service.db.Query(ctx, `SELECT `+selectNodeColumns+`,
       CASE
         WHEN nodes.node_id::text = LOWER($1) THEN 1
         WHEN nodes.node_key = $1 THEN 2
         WHEN selected_alias.alias_key IS NOT NULL THEN 3
         ELSE 4
       END AS selector_rank
     FROM codex_nodes nodes
     LEFT JOIN mira_node_aliases selected_alias
       ON selected_alias.node_id = nodes.node_id AND selected_alias.alias_key = $2
     WHERE ($3::boolean OR nodes.approval_status = 'approved')
       AND (nodes.node_id::text = LOWER($1) OR nodes.node_key = $1
            OR selected_alias.alias_key IS NOT NULL OR nodes.hostname = $1)
     ORDER BY selector_rank, nodes.hostname, nodes.node_key`, selector, NormalizeAliasKey(selector), includeRevoked)
	if err != nil {
		return Result{}, fmt.Errorf("resolve Node: %w", err)
	}
	defer rows.Close()
	matches := []nodeRow{}
	for rows.Next() {
		row, scanErr := scanNode(rows, true)
		if scanErr != nil {
			return Result{}, scanErr
		}
		matches = append(matches, row)
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	if len(matches) == 0 {
		return result(404, map[string]any{"error": "no Node matches selector " + selector, "code": "not_found"}), nil
	}
	rank := matches[0].rank
	strongest := matches[:0]
	for _, match := range matches {
		if match.rank == rank {
			strongest = append(strongest, match)
		}
	}
	if len(strongest) != 1 {
		return result(409, map[string]any{"error": "Node selector is ambiguous: " + selector, "code": "ambiguous_selector"}), nil
	}
	matchedBy := []string{"", "nodeId", "nodeKey", "alias", "hostname"}[rank]
	return result(200, map[string]any{"node": strongest[0].Node, "matchedBy": matchedBy}), nil
}

// SetMetadata atomically replaces user metadata under an optimistic revision.
func (service *Service) SetMetadata(ctx context.Context, request *http.Request, principal *Principal, nodeID string, body map[string]any) (Result, error) {
	value, err := normalizedMetadata(body)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('mira_node_selector_namespace'))"); err != nil {
		return Result{}, err
	}
	var nodeKey, revisionText string
	err = tx.QueryRow(ctx, `SELECT node_key, metadata_revision::text FROM codex_nodes WHERE node_id = $1::uuid FOR UPDATE`, nodeID).Scan(&nodeKey, &revisionText)
	if errors.Is(err, pgx.ErrNoRows) {
		return result(404, map[string]any{"error": "Node not found", "code": "not_found"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	currentRevision, err := strconv.ParseInt(revisionText, 10, 64)
	if err != nil {
		return Result{}, err
	}
	if currentRevision != value.ExpectedRevision {
		return result(409, map[string]any{
			"error": "Node metadata changed; reload it before saving", "code": "metadata_conflict",
			"currentRevision": currentRevision,
		}), nil
	}
	nodeKeys, err := tx.Query(ctx, "SELECT node_key FROM codex_nodes")
	if err != nil {
		return Result{}, err
	}
	reserved := map[string]bool{}
	for nodeKeys.Next() {
		var key string
		if err := nodeKeys.Scan(&key); err != nil {
			nodeKeys.Close()
			return Result{}, err
		}
		reserved[NormalizeAliasKey(key)] = true
	}
	err = nodeKeys.Err()
	nodeKeys.Close()
	if err != nil {
		return Result{}, err
	}
	for _, candidate := range value.Aliases {
		if reserved[candidate.Key] {
			return result(409, map[string]any{
				"error": "alias conflicts with a Node key: " + candidate.Value, "code": "alias_conflict",
			}), nil
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM mira_node_aliases WHERE node_id = $1::uuid", nodeID); err != nil {
		return Result{}, err
	}
	for _, candidate := range value.Aliases {
		if _, err := tx.Exec(ctx, `INSERT INTO mira_node_aliases(alias_key, alias, node_id) VALUES ($1, $2, $3::uuid)`, candidate.Key, candidate.Value, nodeID); err != nil {
			if isUniqueViolation(err) {
				return result(409, map[string]any{"error": "alias is already assigned to another Node", "code": "alias_conflict"}), nil
			}
			return Result{}, err
		}
	}
	labelsJSON, err := marshalJSON(value.Labels)
	if err != nil {
		return Result{}, err
	}
	var revision int64
	err = tx.QueryRow(ctx, `UPDATE codex_nodes SET display_name = $2, labels = $3::jsonb,
         metadata_revision = metadata_revision + 1, updated_at = NOW()
       WHERE node_id = $1::uuid RETURNING metadata_revision`, nodeID, value.DisplayName, labelsJSON).Scan(&revision)
	if err != nil {
		return Result{}, err
	}
	if err := service.audit(ctx, tx, AuditEvent{
		Action: "node.metadata.updated", Principal: principal, TargetNodeID: nodeID, Request: request,
		Metadata: map[string]any{
			"aliasCount": len(value.Aliases), "labelCount": len(value.Labels),
			"hasDisplayName": value.DisplayName != nil, "revision": revision,
		},
	}); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	aliases := make([]string, len(value.Aliases))
	for index := range value.Aliases {
		aliases[index] = value.Aliases[index].Value
	}
	return result(200, map[string]any{
		"nodeId": nodeID, "displayName": nullableString(value.DisplayName), "aliases": aliases,
		"labels": value.Labels, "metadataRevision": revision,
	}), nil
}

// SetChannelStatus updates the Server-observed reverse-channel state.
func (service *Service) SetChannelStatus(ctx context.Context, nodeID string, status any) error {
	encoded, err := marshalJSON(status)
	if err != nil {
		return err
	}
	_, err = service.db.Exec(ctx, `UPDATE codex_nodes SET channel_status = $2::jsonb, updated_at = NOW() WHERE node_id = $1::uuid`, nodeID, encoded)
	return err
}

type desiredState struct {
	JSON                      map[string]any
	DefaultCwd                optionalPath
	DeveloperInstructionsFile optionalPath
}

func (service *Service) normalizedDesiredState(body map[string]any) (desiredState, error) {
	running, ok := body["running"].(bool)
	if !ok {
		return desiredState{}, errors.New("running must be a boolean")
	}
	rawOverrides, providedOverrides := body["configOverrides"]
	var overrides []any
	if rawOverrides == nil {
		overrides = []any{}
	} else {
		overrides, ok = array(rawOverrides)
		if !ok {
			return desiredState{}, errors.New("configOverrides must be non-secret and contain at most 20 strings")
		}
	}
	if len(overrides) > 20 {
		return desiredState{}, errors.New("configOverrides must be non-secret and contain at most 20 strings")
	}
	for _, raw := range overrides {
		value, ok := raw.(string)
		if !ok || value == "" || jsStringLength(value) > 2048 || secretOverride.MatchString(value) {
			return desiredState{}, errors.New("configOverrides must be non-secret and contain at most 20 strings")
		}
	}
	defaultCwd, err := optionalAbsolutePath("defaultCwd", body)
	if err != nil {
		return desiredState{}, err
	}
	instructions, err := optionalAbsolutePath("developerInstructionsFile", body)
	if err != nil {
		return desiredState{}, err
	}
	desired := map[string]any{"running": running, "revision": service.now()}
	for _, key := range []string{"environmentFiles", "inheritEnv"} {
		if raw, present := body[key]; present {
			values, ok := array(raw)
			maximum := 128
			if key == "environmentFiles" {
				maximum = 8
			}
			if !ok || len(values) > maximum {
				return desiredState{}, fmt.Errorf("invalid %s", key)
			}
			for _, value := range values {
				text, ok := value.(string)
				if !ok || text == "" || len(text) > 4096 || containsControl(text, false) {
					return desiredState{}, fmt.Errorf("invalid %s", key)
				}
				if key == "inheritEnv" && !validAccountEnvironmentName(text) {
					return desiredState{}, errors.New("invalid or reserved environment name")
				}
			}
			desired[key] = values
		}
	}
	for _, field := range []string{"listenUrl", "codexPath", "codexHome"} {
		if value, exists := body[field]; exists {
			if value != nil {
				text, ok := value.(string)
				if !ok || len(text) > 4096 || containsControl(text, false) {
					return desiredState{}, fmt.Errorf("invalid %s", field)
				}
				if field == "listenUrl" && text != "" {
					u, err := url.Parse(text)
					if err != nil {
						return desiredState{}, errors.New("listenUrl must be a loopback WebSocket URL")
					}
					ip := net.ParseIP(u.Hostname())
					port, err := strconv.Atoi(u.Port())
					if u.Scheme != "ws" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) || err != nil || port < 0 || port > 65535 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
						return desiredState{}, errors.New("listenUrl must be a loopback WebSocket URL")
					}
				}
			}
			desired[field] = value
		}
	}
	if providedOverrides {
		desired["configOverrides"] = overrides
	}
	if defaultCwd.Present {
		desired["defaultCwd"] = nullableString(defaultCwd.Value)
	}
	if instructions.Present {
		desired["developerInstructionsFile"] = nullableString(instructions.Value)
	}
	return desiredState{JSON: desired, DefaultCwd: defaultCwd, DeveloperInstructionsFile: instructions}, nil
}

// SetDesiredAppServer merges validated desired App Server fields into the
// currently stored desired state.
func (service *Service) SetDesiredAppServer(ctx context.Context, nodeID string, body map[string]any) (Result, error) {
	desired, err := service.normalizedDesiredState(body)
	if err != nil {
		return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
	}
	if (desired.DefaultCwd.Present && desired.DefaultCwd.Value != nil) ||
		(desired.DeveloperInstructionsFile.Present && desired.DeveloperInstructionsFile.Value != nil) || body["environmentFiles"] != nil || body["codexHome"] != nil || body["codexPath"] != nil {
		var platform string
		err := service.db.QueryRow(ctx, `SELECT platform FROM codex_nodes WHERE node_id = $1::uuid AND approval_status = 'approved'`, nodeID).Scan(&platform)
		if errors.Is(err, pgx.ErrNoRows) {
			return result(404, map[string]any{"error": "approved node not found", "code": "not_found"}), nil
		}
		if err != nil {
			return Result{}, err
		}
		if err := validateAccountDesiredPaths(desired.JSON, platform); err != nil {
			return result(400, map[string]any{"error": err.Error(), "code": "invalid_request"}), nil
		}
		for _, field := range []struct {
			name string
			path optionalPath
		}{{"defaultCwd", desired.DefaultCwd}, {"developerInstructionsFile", desired.DeveloperInstructionsFile}} {
			if field.path.Present && field.path.Value != nil && !nativeAbsolutePath(*field.path.Value, platform) {
				return result(400, map[string]any{
					"error": field.name + " must be an absolute " + platform + " path", "code": "invalid_request",
				}), nil
			}
		}
	}
	encoded, err := marshalJSON(desired.JSON)
	if err != nil {
		return Result{}, err
	}
	var storedRaw []byte
	err = service.db.QueryRow(ctx, `WITH changed AS (UPDATE codex_nodes SET desired_app_server = desired_app_server || $2::jsonb, updated_at = NOW()
     WHERE node_id = $1::uuid AND approval_status = 'approved' RETURNING desired_app_server), bumped AS (
     UPDATE mira_node_codex_accounts SET revision=revision+1 WHERE node_id=$1::uuid AND is_default AND EXISTS(SELECT 1 FROM changed))
     SELECT desired_app_server FROM changed`, nodeID, encoded).Scan(&storedRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result(404, map[string]any{"error": "approved node not found", "code": "not_found"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	stored, err := decodeObject(storedRaw)
	if err != nil {
		return Result{}, err
	}
	return result(200, map[string]any{"nodeId": nodeID, "desiredAppServer": stored}), nil
}

// Revoke disables a Node and every active credential in one transaction.
func (service *Service) Revoke(ctx context.Context, request *http.Request, principal *Principal, nodeID string, reason *string) (Result, error) {
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var nodeKey string
	err = tx.QueryRow(ctx, `UPDATE codex_nodes SET approval_status = 'revoked', revoked_at = NOW(),
         desired_app_server = jsonb_set(desired_app_server, '{running}', 'false'::jsonb),
         channel_status = '{"connected":false,"reason":"revoked"}'::jsonb, updated_at = NOW()
       WHERE node_id = $1::uuid AND approval_status = 'approved' RETURNING node_key`, nodeID).Scan(&nodeKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return result(404, map[string]any{"error": "approved node not found", "code": "not_found"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE mira_node_credentials SET revoked_at = NOW() WHERE node_id = $1::uuid AND revoked_at IS NULL`, nodeID); err != nil {
		return Result{}, err
	}
	if err := service.audit(ctx, tx, AuditEvent{
		Action: "node.revoked", Principal: principal, TargetNodeID: nodeID, Request: request,
		Metadata: map[string]any{"nodeKey": nodeKey, "hasReason": reason != nil},
	}); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result(200, map[string]any{"nodeId": nodeID, "status": "revoked"}), nil
}
