// Package nodes implements Mira Node registration, enrollment, metadata, and
// desired-state persistence for Mira Server.
package nodes

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// Querier is implemented by pgx pools and transactions.
type Querier interface {
	foundation.DBTX
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Database is the subset of pgxpool.Pool used by Service.
type Database interface {
	Querier
	Begin(context.Context) (pgx.Tx, error)
}

type Principal = foundation.Principal
type AuditEvent = foundation.AuditEvent

// AuditFunc appends an event using the supplied pool or transaction. A
// transaction-scoped callback must use query rather than a separate pool so an
// audit record commits atomically with the change it describes.
type AuditFunc func(ctx context.Context, query Querier, event AuditEvent) error

// Options controls request metadata and injected persistence collaborators.
type Options struct {
	Audit             AuditFunc
	TrustProxyHeaders bool
	RandomCode        func() (string, error)
	Now               func() int64
}

// Service owns Node registry and enrollment operations.
type Service struct {
	db                Database
	audit             AuditFunc
	trustProxyHeaders bool
	randomCode        func() (string, error)
	now               func() int64
}

// Result is an HTTP-shaped result. Infrastructure errors are returned
// separately; validation and domain conflicts are represented here.
type Result struct {
	Status int
	Body   any
}

// Node is the public representation returned by the legacy Server API.
type Node struct {
	NodeID             string         `json:"nodeId"`
	NodeKey            string         `json:"nodeKey"`
	Hostname           string         `json:"hostname"`
	Platform           string         `json:"platform"`
	Architecture       string         `json:"architecture"`
	NodeMode           string         `json:"nodeMode"`
	NodeVersion        string         `json:"nodeVersion"`
	NodeBuild          map[string]any `json:"nodeBuild"`
	Capabilities       map[string]any `json:"capabilities"`
	CodexInstallations []any          `json:"codexInstallations"`
	DesiredAppServer   map[string]any `json:"desiredAppServer"`
	ReportedAppServer  map[string]any `json:"reportedAppServer"`
	MachineStatus      map[string]any `json:"machineStatus"`
	ChannelStatus      map[string]any `json:"channelStatus"`
	DisplayName        *string        `json:"displayName"`
	Aliases            []string       `json:"aliases"`
	Labels             map[string]any `json:"labels"`
	MetadataRevision   int64          `json:"metadataRevision"`
	ApprovalStatus     string         `json:"approvalStatus"`
	ApprovedAt         *string        `json:"approvedAt"`
	RevokedAt          *string        `json:"revokedAt"`
	RegisteredAt       string         `json:"registeredAt"`
	LastSeenAt         string         `json:"lastSeenAt"`
	Status             string         `json:"status"`
}

// New creates a Node registry and enrollment service.
func New(db Database, options Options) *Service {
	audit := options.Audit
	if audit == nil {
		audit = func(ctx context.Context, query Querier, event AuditEvent) error {
			return foundation.AppendAudit(ctx, query, event, options.TrustProxyHeaders)
		}
	}
	randomCode := options.RandomCode
	if randomCode == nil {
		randomCode = secureVerificationCode
	}
	now := options.Now
	if now == nil {
		now = unixMilliseconds
	}
	return &Service{
		db: db, audit: audit, trustProxyHeaders: options.TrustProxyHeaders,
		randomCode: randomCode, now: now,
	}
}

func result(status int, body map[string]any) Result {
	return Result{Status: status, Body: body}
}
