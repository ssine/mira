// Package views builds read-only Web projections from Mira's canonical
// PostgreSQL ThreadStore.
package views

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultStoreID = "personal"

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time

	costMu    sync.Mutex
	costCache map[string]cachedCost
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now, costCache: map[string]cachedCost{}}
}

type Result struct {
	Status int
	Body   map[string]any
}

type Thread struct {
	ThreadID           string         `json:"threadId"`
	ParentThreadID     *string        `json:"parentThreadId"`
	SourceKind         *string        `json:"sourceKind"`
	Title              *string        `json:"title"`
	Name               *string        `json:"name"`
	Archived           bool           `json:"archived"`
	Cwd                *string        `json:"cwd"`
	ForkedFromID       *string        `json:"forkedFromId"`
	ItemCount          int64          `json:"itemCount"`
	Generation         int64          `json:"generation"`
	TokenUsage         map[string]any `json:"tokenUsage"`
	Model              *string        `json:"model"`
	CreatedAt          *string        `json:"createdAt"`
	UpdatedAt          *string        `json:"updatedAt"`
	ImportID           *string        `json:"importId"`
	ImportedItemCount  *int64         `json:"importedItemCount"`
	SourceNodeID       *string        `json:"sourceNodeId"`
	SourceCodexVersion *string        `json:"sourceCodexVersion"`
	ImportedAt         *string        `json:"importedAt"`
	RuntimeNodeID      *string        `json:"runtimeNodeId"`
	RuntimeBoundAt     *string        `json:"runtimeBoundAt"`
	Activity           map[string]any `json:"activity"`
	ReadState          map[string]any `json:"readState"`
	CostEstimate       map[string]any `json:"costEstimate,omitempty"`
}

type TranscriptOptions struct {
	Cursor      *string
	Limit       int
	Tail        bool
	ToolDetails *bool
	Timings     map[string]time.Duration
}

type ProjectionOptions struct {
	ItemOffset                    int64
	InitialTurnID                 string
	InitialTurnStartedAt          string
	InitialTurnStartedApproximate bool
	Fragments                     bool
	RecordedAt                    map[int64]string
	TimingRecords                 []map[string]any
}
