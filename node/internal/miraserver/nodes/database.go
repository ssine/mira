package nodes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const selectNodeColumns = `
  nodes.node_id::text, nodes.node_key, nodes.hostname, nodes.platform, nodes.architecture, nodes.node_mode,
  nodes.node_version, nodes.node_build, nodes.capabilities, nodes.codex_installations, nodes.desired_app_server,
  nodes.reported_app_server, nodes.machine_status, nodes.channel_status, nodes.approval_status,
  nodes.display_name, nodes.labels, nodes.metadata_revision::text,
  COALESCE((SELECT jsonb_agg(alias.alias ORDER BY alias.alias_key)
            FROM mira_node_aliases alias WHERE alias.node_id = nodes.node_id), '[]'::jsonb) AS aliases,
  nodes.approved_at, nodes.revoked_at, nodes.registered_at, nodes.last_seen_at,
  CASE WHEN (nodes.channel_status->>'connected')::boolean IS TRUE
             AND nodes.last_seen_at > NOW() - INTERVAL '15 seconds'
       THEN 'online' ELSE 'offline' END AS status`

const enrollmentColumns = `
  enrollment_id::text, node_id::text, credential_id::text, credential_secret_hash,
  credential_fingerprint, verification_code, node_key, hostname, platform, architecture,
  node_mode, node_version, node_build, capabilities, codex_installations,
  default_desired_app_server, machine_status, requested_from, status, decision_note,
  requested_at, expires_at, approved_at, rejected_at`

type scanner interface {
	Scan(...any) error
}

type nodeRow struct {
	Node
	rank int
}

func scanNode(row scanner, withRank bool) (nodeRow, error) {
	var value nodeRow
	var nodeBuild, capabilities, installations, desired, reported, machine, channel, labels, aliases []byte
	var revision string
	var approvedAt, revokedAt *time.Time
	var registeredAt, lastSeenAt time.Time
	destinations := []any{
		&value.NodeID, &value.NodeKey, &value.Hostname, &value.Platform, &value.Architecture, &value.NodeMode,
		&value.NodeVersion, &nodeBuild, &capabilities, &installations, &desired, &reported, &machine, &channel,
		&value.ApprovalStatus, &value.DisplayName, &labels, &revision, &aliases,
		&approvedAt, &revokedAt, &registeredAt, &lastSeenAt, &value.Status,
	}
	if withRank {
		destinations = append(destinations, &value.rank)
	}
	if err := row.Scan(destinations...); err != nil {
		return nodeRow{}, err
	}
	var err error
	if value.NodeBuild, err = decodeObject(nodeBuild); err != nil {
		return nodeRow{}, fmt.Errorf("decode Node build: %w", err)
	}
	if value.Capabilities, err = decodeObject(capabilities); err != nil {
		return nodeRow{}, fmt.Errorf("decode Node capabilities: %w", err)
	}
	if err = decodeJSON(installations, &value.CodexInstallations); err != nil {
		return nodeRow{}, fmt.Errorf("decode Codex installations: %w", err)
	}
	if value.CodexInstallations == nil {
		value.CodexInstallations = []any{}
	}
	for name, source := range map[string][]byte{
		"desired App Server": desired, "reported App Server": reported,
		"machine status": machine, "channel status": channel, "labels": labels,
	} {
		decoded, decodeErr := decodeObject(source)
		if decodeErr != nil {
			return nodeRow{}, fmt.Errorf("decode Node %s: %w", name, decodeErr)
		}
		switch name {
		case "desired App Server":
			value.DesiredAppServer = decoded
		case "reported App Server":
			value.ReportedAppServer = decoded
		case "machine status":
			value.MachineStatus = decoded
		case "channel status":
			value.ChannelStatus = decoded
		case "labels":
			value.Labels = decoded
		}
	}
	if err = decodeJSON(aliases, &value.Aliases); err != nil {
		return nodeRow{}, fmt.Errorf("decode Node aliases: %w", err)
	}
	if value.Aliases == nil {
		value.Aliases = []string{}
	}
	value.MetadataRevision, err = strconv.ParseInt(revision, 10, 64)
	if err != nil {
		return nodeRow{}, fmt.Errorf("parse Node metadata revision: %w", err)
	}
	value.ApprovedAt = formatOptionalTime(approvedAt)
	value.RevokedAt = formatOptionalTime(revokedAt)
	value.RegisteredAt = formatTime(registeredAt)
	value.LastSeenAt = formatTime(lastSeenAt)
	if value.ApprovalStatus == "revoked" {
		value.Status = "revoked"
	}
	return value, nil
}

type enrollmentRow struct {
	EnrollmentID            string
	NodeID                  *string
	CredentialID            string
	CredentialSecretHash    string
	CredentialFingerprint   string
	VerificationCode        string
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
	RequestedFrom           *string
	Status                  string
	DecisionNote            *string
	RequestedAt             time.Time
	ExpiresAt               time.Time
	ApprovedAt              *time.Time
	RejectedAt              *time.Time
}

func scanEnrollment(row scanner) (enrollmentRow, error) {
	var value enrollmentRow
	var nodeBuild, capabilities, installations, desired, machine []byte
	if err := row.Scan(
		&value.EnrollmentID, &value.NodeID, &value.CredentialID, &value.CredentialSecretHash,
		&value.CredentialFingerprint, &value.VerificationCode, &value.NodeKey, &value.Hostname,
		&value.Platform, &value.Architecture, &value.NodeMode, &value.NodeVersion,
		&nodeBuild, &capabilities, &installations, &desired, &machine, &value.RequestedFrom,
		&value.Status, &value.DecisionNote, &value.RequestedAt, &value.ExpiresAt,
		&value.ApprovedAt, &value.RejectedAt,
	); err != nil {
		return enrollmentRow{}, err
	}
	var err error
	if value.NodeBuild, err = decodeObject(nodeBuild); err != nil {
		return enrollmentRow{}, fmt.Errorf("decode enrollment Node build: %w", err)
	}
	if value.Capabilities, err = decodeObject(capabilities); err != nil {
		return enrollmentRow{}, fmt.Errorf("decode enrollment capabilities: %w", err)
	}
	if err = decodeJSON(installations, &value.CodexInstallations); err != nil {
		return enrollmentRow{}, fmt.Errorf("decode enrollment Codex installations: %w", err)
	}
	if value.CodexInstallations == nil {
		value.CodexInstallations = []any{}
	}
	if value.DefaultDesiredAppServer, err = decodeObject(desired); err != nil {
		return enrollmentRow{}, fmt.Errorf("decode enrollment desired App Server: %w", err)
	}
	if value.MachineStatus, err = decodeObject(machine); err != nil {
		return enrollmentRow{}, fmt.Errorf("decode enrollment machine status: %w", err)
	}
	return value, nil
}

func enrollmentView(row enrollmentRow, includeAdminFields bool) map[string]any {
	view := map[string]any{
		"enrollmentId": row.EnrollmentID, "nodeId": nullableString(row.NodeID),
		"credentialId": row.CredentialID, "credentialFingerprint": row.CredentialFingerprint,
		"verificationCode": row.VerificationCode, "nodeKey": row.NodeKey,
		"hostname": row.Hostname, "platform": row.Platform, "architecture": row.Architecture,
		"nodeMode": row.NodeMode, "nodeVersion": row.NodeVersion, "nodeBuild": row.NodeBuild,
		"capabilities": row.Capabilities, "codexInstallations": row.CodexInstallations,
		"machineStatus": row.MachineStatus, "status": row.Status,
		"decisionNote": nullableString(row.DecisionNote), "requestedAt": formatTime(row.RequestedAt),
		"expiresAt": formatTime(row.ExpiresAt), "approvedAt": nullableTime(row.ApprovedAt),
		"rejectedAt": nullableTime(row.RejectedAt),
	}
	if includeAdminFields {
		view["requestedFrom"] = nullableString(row.RequestedFrom)
	}
	return view
}

func decodeJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(destination)
}

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := decodeJSON(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return value, nil
}

func marshalJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func formatOptionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := formatTime(*value)
	return &formatted
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}
