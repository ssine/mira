package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type descriptor struct {
	NodeKey                 string
	Hostname                string
	Platform                string
	Architecture            string
	NodeMode                string
	NodeVersion             string
	NodeBuild               map[string]any
	Capabilities            map[string]any
	CodexInstallations      []any
	DefaultDesiredAppServer map[string]any
	MachineStatus           map[string]any
}

func normalizeDescriptor(body map[string]any) (descriptor, error) {
	nodeKey, err := requiredString("nodeKey", body["nodeKey"], 256)
	if err != nil {
		return descriptor{}, err
	}
	hostname, err := requiredString("hostname", body["hostname"], 256)
	if err != nil {
		return descriptor{}, err
	}
	platform, err := requiredString("platform", body["platform"], 64)
	if err != nil {
		return descriptor{}, err
	}
	architecture, err := requiredString("architecture", body["architecture"], 64)
	if err != nil {
		return descriptor{}, err
	}
	nodeMode, err := requiredString("nodeMode", body["nodeMode"], 64)
	if err != nil {
		return descriptor{}, err
	}
	versionValue, exists := body["nodeVersion"]
	if !exists || versionValue == nil {
		versionValue = body["agentVersion"]
	}
	nodeVersion, err := requiredString("nodeVersion", versionValue, 64)
	if err != nil {
		return descriptor{}, err
	}
	nodeBuild, err := descriptorObject(body, "nodeBuild", map[string]any{})
	if err != nil {
		return descriptor{}, err
	}
	capabilities, err := descriptorObject(body, "capabilities", map[string]any{})
	if err != nil {
		return descriptor{}, err
	}
	desired, err := descriptorObject(body, "defaultDesiredAppServer", map[string]any{"running": false})
	if err != nil {
		return descriptor{}, err
	}
	machine, err := descriptorObject(body, "machineStatus", map[string]any{})
	if err != nil {
		return descriptor{}, err
	}
	installations, ok := array(body["codexInstallations"])
	if !ok {
		installations = []any{}
	}
	if version, exists := nodeBuild["version"]; exists && version != nodeVersion {
		return descriptor{}, errors.New("nodeBuild.version must match nodeVersion")
	}
	return descriptor{
		NodeKey: nodeKey, Hostname: hostname, Platform: platform, Architecture: architecture,
		NodeMode: nodeMode, NodeVersion: nodeVersion, NodeBuild: nodeBuild,
		Capabilities: capabilities, CodexInstallations: installations,
		DefaultDesiredAppServer: desired, MachineStatus: machine,
	}, nil
}

func descriptorObject(body map[string]any, name string, fallback map[string]any) (map[string]any, error) {
	value, exists := body[name]
	if !exists || value == nil {
		return fallback, nil
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return object, nil
}

type descriptorJSON struct {
	NodeBuild, Capabilities, CodexInstallations, DesiredAppServer, MachineStatus string
}

func encodeDescriptor(value descriptor) (descriptorJSON, error) {
	var encoded descriptorJSON
	fields := []struct {
		value any
		set   func(string)
	}{
		{value.NodeBuild, func(result string) { encoded.NodeBuild = result }},
		{value.Capabilities, func(result string) { encoded.Capabilities = result }},
		{value.CodexInstallations, func(result string) { encoded.CodexInstallations = result }},
		{value.DefaultDesiredAppServer, func(result string) { encoded.DesiredAppServer = result }},
		{value.MachineStatus, func(result string) { encoded.MachineStatus = result }},
	}
	for _, field := range fields {
		result, err := marshalJSON(field.value)
		if err != nil {
			return descriptorJSON{}, err
		}
		field.set(result)
	}
	return encoded, nil
}

func descriptorFromRow(row enrollmentRow) descriptor {
	return descriptor{
		NodeBuild: row.NodeBuild, Capabilities: row.Capabilities,
		CodexInstallations:      row.CodexInstallations,
		DefaultDesiredAppServer: row.DefaultDesiredAppServer, MachineStatus: row.MachineStatus,
	}
}

func truncateNote(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	units := utf16.Encode([]rune(text))
	if len(units) > 2_000 {
		units = units[:2_000]
	}
	text = string(utf16.Decode(units))
	return &text
}

func (service *Service) expireRequests(ctx context.Context, query Querier, enrollmentID *string) error {
	var id any
	if enrollmentID != nil {
		id = *enrollmentID
	}
	_, err := query.Exec(ctx, `UPDATE mira_node_enrollment_requests SET status = 'expired'
     WHERE status = 'pending' AND expires_at <= NOW()
       AND ($1::uuid IS NULL OR enrollment_id = $1::uuid)`, id)
	return err
}

func (service *Service) authenticatedEnrollment(ctx context.Context, request *http.Request, enrollmentID string) (*enrollmentRow, error) {
	token, ok := requestNodeToken(request)
	if !ok {
		return nil, nil
	}
	if err := service.expireRequests(ctx, service.db, &enrollmentID); err != nil {
		return nil, err
	}
	row, err := scanEnrollment(service.db.QueryRow(ctx, `SELECT `+enrollmentColumns+`
     FROM mira_node_enrollment_requests
     WHERE enrollment_id = $1::uuid AND credential_id = $2::uuid`, enrollmentID, token.CredentialID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !constantTimeHashEqual(row.CredentialSecretHash, token.SecretHash) {
		return nil, nil
	}
	return &row, nil
}

// CreateEnrollment records a new pending Node credential request. Repeating an
// identical credential request is idempotent and returns the original row.
func (service *Service) CreateEnrollment(ctx context.Context, request *http.Request, body map[string]any) (Result, error) {
	descriptor, err := normalizeDescriptor(body)
	if err != nil {
		return result(400, map[string]any{"error": err.Error()}), nil
	}
	credentialID, ok := body["credentialId"].(string)
	if !ok || !credentialIDPattern.MatchString(credentialID) {
		return result(400, map[string]any{"error": "credentialId must be a UUID"}), nil
	}
	secretHash, ok := body["credentialSecretHash"].(string)
	if !ok || !secretHashPattern.MatchString(secretHash) {
		return result(400, map[string]any{"error": "credentialSecretHash must be a SHA-256 hex digest"}), nil
	}
	credentialID = strings.ToLower(credentialID)
	secretHash = strings.ToLower(secretHash)
	prior, err := scanEnrollment(service.db.QueryRow(ctx, `SELECT `+enrollmentColumns+`
     FROM mira_node_enrollment_requests
     WHERE credential_id = $1::uuid AND credential_secret_hash = $2`, credentialID, secretHash))
	if err == nil {
		return result(202, enrollmentView(prior, false)), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, err
	}
	var exists bool
	err = service.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_node_credentials WHERE credential_id = $1::uuid)`, credentialID).Scan(&exists)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return result(409, map[string]any{"error": "credential is already enrolled", "code": "enrollment_conflict"}), nil
	}
	err = service.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM codex_nodes WHERE node_key = $1 AND approval_status = 'approved')`, descriptor.NodeKey).Scan(&exists)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return result(409, map[string]any{"error": "an approved node already uses this node key", "code": "enrollment_conflict"}), nil
	}
	err = service.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_node_aliases WHERE alias_key = $1)`, NormalizeAliasKey(descriptor.NodeKey)).Scan(&exists)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return result(409, map[string]any{"error": "a Node alias already uses this node key", "code": "enrollment_conflict"}), nil
	}
	var address any
	if request != nil {
		if value := foundation.RequestAddress(request, service.trustProxyHeaders); value != "" {
			address = value
		}
	}
	var recent int
	err = service.db.QueryRow(ctx, `SELECT COUNT(*)::integer FROM mira_node_enrollment_requests
     WHERE requested_from IS NOT DISTINCT FROM $1
       AND requested_at > NOW() - INTERVAL '1 hour'
       AND status = 'pending'`, address).Scan(&recent)
	if err != nil {
		return Result{}, err
	}
	if recent >= 50 {
		return result(429, map[string]any{"error": "too many pending enrollment requests"}), nil
	}
	verificationCode, err := service.randomCode()
	if err != nil {
		return Result{}, err
	}
	encoded, err := encodeDescriptor(descriptor)
	if err != nil {
		return Result{}, err
	}
	created, err := scanEnrollment(service.db.QueryRow(ctx, `INSERT INTO mira_node_enrollment_requests (
         credential_id, credential_secret_hash, credential_fingerprint,
         node_key, verification_code, hostname, platform, architecture,
         node_mode, node_version, node_build, capabilities, codex_installations,
         default_desired_app_server, machine_status, requested_from, expires_at
       ) VALUES (
         $1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10,
         $11::jsonb, $12::jsonb, $13::jsonb, $14::jsonb, $15::jsonb, $16,
         NOW() + make_interval(mins => $17)
       ) RETURNING `+enrollmentColumns,
		credentialID, secretHash, fingerprint(secretHash), descriptor.NodeKey, verificationCode,
		descriptor.Hostname, descriptor.Platform, descriptor.Architecture, descriptor.NodeMode,
		descriptor.NodeVersion, encoded.NodeBuild, encoded.Capabilities, encoded.CodexInstallations,
		encoded.DesiredAppServer, encoded.MachineStatus,
		address, enrollmentLifetimeMinutes,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return result(409, map[string]any{"error": "credential already requested"}), nil
		}
		return Result{}, err
	}
	if err := service.audit(ctx, service.db, AuditEvent{
		Action: "node.enrollment.requested", Request: request,
		Metadata: map[string]any{
			"enrollmentId": created.EnrollmentID, "nodeKey": descriptor.NodeKey,
			"credentialFingerprint": created.CredentialFingerprint,
		},
	}); err != nil {
		return Result{}, err
	}
	return result(202, enrollmentView(created, false)), nil
}

// GetEnrollment returns one request only when the caller proves possession of
// the credential secret used to create it.
func (service *Service) GetEnrollment(ctx context.Context, request *http.Request, enrollmentID string) (Result, error) {
	row, err := service.authenticatedEnrollment(ctx, request, enrollmentID)
	if err != nil {
		return Result{}, err
	}
	if row == nil {
		return result(401, map[string]any{"error": "invalid enrollment credential"}), nil
	}
	return result(200, enrollmentView(*row, false)), nil
}

// ListEnrollments returns at most the 500 newest enrollment requests.
func (service *Service) ListEnrollments(ctx context.Context, status *string) ([]map[string]any, error) {
	if err := service.expireRequests(ctx, service.db, nil); err != nil {
		return nil, err
	}
	query := `SELECT ` + enrollmentColumns + ` FROM mira_node_enrollment_requests`
	arguments := []any{}
	if status != nil {
		query += ` WHERE status = $1`
		arguments = append(arguments, *status)
	}
	query += ` ORDER BY requested_at DESC LIMIT 500`
	rows, err := service.db.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		row, scanErr := scanEnrollment(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, enrollmentView(row, true))
	}
	return result, rows.Err()
}

// ApproveEnrollment binds a pending request to a new Node, or restores the
// existing revoked Node with the same immutable key and a fresh credential.
func (service *Service) ApproveEnrollment(ctx context.Context, request *http.Request, principal *Principal, enrollmentID string, body map[string]any) (Result, error) {
	note := truncateNote(body["note"])
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('mira_node_selector_namespace'))"); err != nil {
		return Result{}, err
	}
	if err := service.expireRequests(ctx, tx, &enrollmentID); err != nil {
		return Result{}, err
	}
	row, err := scanEnrollment(tx.QueryRow(ctx, `SELECT `+enrollmentColumns+`
     FROM mira_node_enrollment_requests WHERE enrollment_id = $1::uuid FOR UPDATE`, enrollmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return result(404, map[string]any{"error": "enrollment not found"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	if row.Status != "pending" {
		return result(409, map[string]any{"error": "enrollment is " + row.Status}), nil
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_node_aliases WHERE alias_key = $1)`, NormalizeAliasKey(row.NodeKey)).Scan(&exists)
	if err != nil {
		return Result{}, err
	}
	if exists {
		return result(409, map[string]any{"error": "a Node alias already uses this node key", "code": "enrollment_conflict"}), nil
	}
	var nodeID, approvalStatus string
	err = tx.QueryRow(ctx, `SELECT node_id::text, approval_status FROM codex_nodes WHERE node_key = $1 FOR UPDATE`, row.NodeKey).Scan(&nodeID, &approvalStatus)
	if err == nil {
		if approvalStatus != "revoked" {
			return result(409, map[string]any{"error": "an approved node already uses this node key"}), nil
		}
		encoded, err := encodeDescriptor(descriptorFromRow(row))
		if err != nil {
			return Result{}, err
		}
		_, err = tx.Exec(ctx, `UPDATE codex_nodes SET
           enrollment_id = $2::uuid, hostname = $3, platform = $4, architecture = $5,
           node_mode = $6, node_version = $7, node_build = $8::jsonb, capabilities = $9::jsonb,
           codex_installations = $10::jsonb, machine_status = $11::jsonb,
           desired_app_server = $12::jsonb, approval_status = 'approved',
           approved_at = NOW(), revoked_at = NULL, updated_at = NOW()
         WHERE node_id = $1::uuid`,
			nodeID, enrollmentID, row.Hostname, row.Platform, row.Architecture, row.NodeMode,
			row.NodeVersion, encoded.NodeBuild, encoded.Capabilities, encoded.CodexInstallations,
			encoded.MachineStatus, encoded.DesiredAppServer,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return result(409, map[string]any{"error": "credential already approved"}), nil
			}
			return Result{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE mira_node_credentials SET revoked_at = NOW()
         WHERE node_id = $1::uuid AND revoked_at IS NULL`, nodeID); err != nil {
			return Result{}, err
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		encoded, encodeErr := encodeDescriptor(descriptorFromRow(row))
		if encodeErr != nil {
			return Result{}, encodeErr
		}
		err = tx.QueryRow(ctx, `INSERT INTO codex_nodes (
           enrollment_id, node_key, hostname, platform, architecture, node_mode,
           node_version, node_build, capabilities, codex_installations, machine_status,
           desired_app_server, approval_status, approved_at
         ) VALUES (
           $1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb,
           $11::jsonb, $12::jsonb, 'approved', NOW()
         ) RETURNING node_id::text`,
			enrollmentID, row.NodeKey, row.Hostname, row.Platform, row.Architecture, row.NodeMode,
			row.NodeVersion, encoded.NodeBuild, encoded.Capabilities, encoded.CodexInstallations,
			encoded.MachineStatus, encoded.DesiredAppServer,
		).Scan(&nodeID)
		if err != nil {
			if isUniqueViolation(err) {
				return result(409, map[string]any{"error": "credential already approved"}), nil
			}
			return Result{}, err
		}
	} else {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mira_node_credentials (
         credential_id, node_id, enrollment_id, secret_hash
       ) VALUES ($1::uuid, $2::uuid, $3::uuid, $4)`, row.CredentialID, nodeID, enrollmentID, row.CredentialSecretHash); err != nil {
		if isUniqueViolation(err) {
			return result(409, map[string]any{"error": "credential already approved"}), nil
		}
		return Result{}, err
	}
	approved, err := scanEnrollment(tx.QueryRow(ctx, `UPDATE mira_node_enrollment_requests SET
         status = 'approved', decision_note = $2, approved_by = $3::uuid,
         approved_at = NOW(), node_id = $4::uuid
       WHERE enrollment_id = $1::uuid RETURNING `+enrollmentColumns,
		enrollmentID, nullableString(note), principal.SubjectID, nodeID,
	))
	if err != nil {
		return Result{}, err
	}
	if err := service.audit(ctx, tx, AuditEvent{
		Action: "node.enrollment.approved", Principal: principal, TargetNodeID: nodeID, Request: request,
		Metadata: map[string]any{
			"enrollmentId": enrollmentID, "nodeKey": row.NodeKey,
			"credentialFingerprint": row.CredentialFingerprint,
		},
	}); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result(200, enrollmentView(approved, true)), nil
}

// RejectEnrollment marks one live pending request rejected.
func (service *Service) RejectEnrollment(ctx context.Context, request *http.Request, principal *Principal, enrollmentID string, body map[string]any) (Result, error) {
	note := truncateNote(body["note"])
	tx, err := service.db.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := scanEnrollment(tx.QueryRow(ctx, `UPDATE mira_node_enrollment_requests SET
         status = 'rejected', decision_note = $2, rejected_at = NOW(), approved_by = $3::uuid
       WHERE enrollment_id = $1::uuid AND status = 'pending' AND expires_at > NOW()
       RETURNING `+enrollmentColumns, enrollmentID, nullableString(note), principal.SubjectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return result(409, map[string]any{"error": "enrollment is missing, expired, or already decided"}), nil
	}
	if err != nil {
		return Result{}, err
	}
	if err := service.audit(ctx, tx, AuditEvent{
		Action: "node.enrollment.rejected", Principal: principal, Request: request,
		Metadata: map[string]any{
			"enrollmentId": enrollmentID, "nodeKey": row.NodeKey, "hasDecisionNote": note != nil,
		},
	}); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result(200, enrollmentView(row, true)), nil
}

// Restore validates that a fresh pending enrollment belongs to the target
// revoked Node, then delegates to the same approval transaction.
func (service *Service) Restore(ctx context.Context, request *http.Request, principal *Principal, nodeID string, body map[string]any) (Result, error) {
	enrollmentID, ok := body["enrollmentId"].(string)
	if !ok {
		return result(409, map[string]any{
			"error": "restore requires a fresh pending enrollmentId so the credential is rotated",
			"code":  "fresh_enrollment_required",
		}), nil
	}
	var matches bool
	err := service.db.QueryRow(ctx, `SELECT EXISTS(
       SELECT 1 FROM mira_node_enrollment_requests requests
       JOIN codex_nodes nodes ON nodes.node_key = requests.node_key
       WHERE requests.enrollment_id = $1::uuid AND nodes.node_id = $2::uuid
         AND requests.status = 'pending' AND nodes.approval_status = 'revoked'
     )`, enrollmentID, nodeID).Scan(&matches)
	if err != nil {
		return Result{}, err
	}
	if !matches {
		return result(409, map[string]any{
			"error": "fresh pending enrollment does not match revoked Node", "code": "enrollment_conflict",
		}), nil
	}
	return service.ApproveEnrollment(ctx, request, principal, enrollmentID, body)
}
