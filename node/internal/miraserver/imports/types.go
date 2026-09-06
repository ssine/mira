// Package imports migrates Node-local Codex rollout JSONL into Mira's
// PostgreSQL ThreadStore while retaining immutable source provenance.
package imports

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const DefaultStoreID = "personal"

type Result struct {
	Status int
	Body   any
}

type Context struct {
	OnProgress func(map[string]any)
}

func (value Context) progress(update map[string]any) {
	if value.OnProgress != nil {
		value.OnProgress(update)
	}
}

type Capability interface {
	Invoke(context.Context, *foundation.Principal, string, string, map[string]any, channel.InvokeContext) (any, error)
}

type NodeRuntime struct {
	NodeID       string
	Hostname     string
	Platform     string
	NodeMode     string
	Capabilities map[string]any
	LastSeenAt   time.Time
	Connected    bool
}

type PreviousImport struct {
	ImportID         string
	SourcePath       string
	SourceSHA256     string
	Status           string
	ThreadID         string
	StoreID          string
	SourceSizeBytes  int64
	SourceModifiedAt *time.Time
	CreatedAt        time.Time
}

type Boundary struct {
	ThreadID            string `json:"thread_id"`
	EndOrdinalExclusive int64  `json:"end_ordinal_exclusive"`
	EndByteOffset       int64  `json:"end_byte_offset"`
}

type RawRecord struct {
	LineSequence int64
	Raw          json.RawMessage
	SHA256       string
	Type         string
	Payload      json.RawMessage
	Semantic     map[string]any
}

type TransferInput struct {
	StoreID      string
	NodeID       string
	Path         string
	ThreadID     string
	ModifiedAt   string
	CodexVersion *string
	Boundary     *Boundary
}

type TransferFinal struct {
	ThreadID  string
	SHA256    string
	SizeBytes int64
	ItemCount int64
	Meta      map[string]any
}

type TransferTransaction interface {
	Add(context.Context, []RawRecord) error
	Finish(context.Context, TransferFinal) (importID string, duplicate bool, err error)
	Rollback(context.Context) error
}

type Repository interface {
	ApprovedNodes(context.Context) ([]NodeRuntime, error)
	PreviousImports(context.Context, string, string, []string) (map[string]PreviousImport, error)
	ValidRuntime(context.Context, string) (string, bool, error)
	BeginTransfer(context.Context, TransferInput) (TransferTransaction, error)
	MarkImport(context.Context, string, string, *int64, *string) error
	ImportedThreadKeys(context.Context) ([]ThreadKey, error)
	FirstHistoryItem(context.Context, string, string, int64, int64) (json.RawMessage, bool, error)
}

type Staged struct {
	ImportID  string
	Meta      map[string]any
	Count     int64
	SizeBytes int64
	Duplicate bool
	Boundary  *Boundary
}

type Segment struct {
	ImportID  string    `json:"importId"`
	FirstLine int64     `json:"firstLine"`
	Count     int64     `json:"count"`
	Boundary  *Boundary `json:"boundary"`
}

type ExpandedLineage struct {
	Segments      []Segment
	Count         int64
	AncestorCount int
}

type ImportCommit struct {
	ThreadID      string
	ImportID      string
	Count         int64
	Segments      []Segment
	Created       map[string]any
	Metadata      map[string]any
	Normalize     func(json.RawMessage) (json.RawMessage, error)
	CodexVersion  string
	RuntimeNodeID *string
}

type CommitResult struct {
	Version  int64
	NoChange bool
}

type Head struct {
	Version         int64
	State           map[string]any
	HistoryManifest map[string]HistoryEntry
}

type HistoryEntry struct {
	Generation int64
	ItemCount  int64
}

type Delta struct {
	ExpectedVersion int64
	StateChanges    []map[string]any
	HistoryChanges  []map[string]any
}

// ThreadStore adapts the parent miraserver's canonical ThreadStore operations.
// CommitImported is deliberately not expressible as a sequence of public
// CommitDelta calls: its implementation must lock the thread, validate the
// immutable segment map and existing canonical prefix, publish provenance,
// history, state, projections, receipt, and the import status atomically.
type ThreadStore interface {
	AssertNotDeleted(context.Context, string, []string) error
	CommitImported(context.Context, string, ImportCommit, Context) (CommitResult, error)
	Head(context.Context, string) (Head, error)
	History(context.Context, string, string, int64, int64) ([]json.RawMessage, error)
	CommitDelta(context.Context, string, Delta, http.Header) (CommitResult, error)
}

type ThreadKey struct {
	StoreID  string
	ThreadID string
}

type ThreadListFunc func(context.Context, string, int, *string, *bool) (any, error)

type AuditFunc func(context.Context, foundation.AuditEvent) error

type Service struct {
	Repository  Repository
	Capability  Capability
	ThreadStore ThreadStore
	ThreadList  ThreadListFunc
	Audit       AuditFunc
	StoreID     string
}

func New(repository Repository, capability Capability, threadStore ThreadStore) *Service {
	return &Service{
		Repository:  repository,
		Capability:  capability,
		ThreadStore: threadStore,
		StoreID:     DefaultStoreID,
	}
}

type Request struct {
	Principal *foundation.Principal
	NodeID    string
	Body      map[string]any
	HTTP      *http.Request
	Progress  Context
}
