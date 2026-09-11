// Package accountsampler records privacy-scoped Codex quota history in the
// background. PostgreSQL advisory locks and durable five-minute slots make it
// safe for more than one Mira Server process to run the sampler.
package accountsampler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

const SampleInterval = 5 * time.Minute

type NodeRegistry interface {
	List(context.Context, bool) ([]nodes.Node, error)
	Get(context.Context, string, bool) (*nodes.Node, error)
}

type Connectivity interface {
	IsConnected(string) bool
}

type AccountReader interface {
	Read(context.Context, string) (channel.AccountSnapshot, error)
}

type Options struct {
	TickInterval time.Duration
	InitialDelay time.Duration
	Workers      int
	Now          func() time.Time
	Logger       *slog.Logger

	// Sample is a test seam for the scheduling loop. Production callers leave
	// it nil and the durable Sample method is used.
	Sample func(context.Context, *nodes.Node) (bool, error)
}

type Sampler struct {
	pool         *pgxpool.Pool
	nodes        NodeRegistry
	connectivity Connectivity
	accounts     AccountReader
	tickInterval time.Duration
	initialDelay time.Duration
	workers      int
	now          func() time.Time
	logger       *slog.Logger
	sample       func(context.Context, *nodes.Node) (bool, error)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func New(pool *pgxpool.Pool, registry NodeRegistry, connectivity Connectivity, accounts AccountReader, options Options) *Sampler {
	if options.TickInterval <= 0 {
		options.TickInterval = 30 * time.Second
	}
	if options.InitialDelay <= 0 {
		options.InitialDelay = time.Second
	}
	if options.Workers <= 0 {
		options.Workers = 2
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	sampler := &Sampler{
		pool: pool, nodes: registry, connectivity: connectivity, accounts: accounts,
		tickInterval: options.TickInterval, initialDelay: options.InitialDelay,
		workers: options.Workers, now: options.Now, logger: options.Logger,
	}
	if options.Sample != nil {
		sampler.sample = options.Sample
	} else {
		sampler.sample = sampler.Sample
	}
	return sampler
}

// Start begins the non-overlapping sampling loop. Repeated calls while the
// sampler is running are harmless.
func (sampler *Sampler) Start(parent context.Context) {
	sampler.mu.Lock()
	if sampler.cancel != nil {
		sampler.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	sampler.cancel = cancel
	sampler.done = done
	sampler.mu.Unlock()
	go sampler.run(ctx, done)
}

// Close cancels account reads, waits for in-flight workers, and is idempotent.
func (sampler *Sampler) Close() {
	sampler.mu.Lock()
	cancel, done := sampler.cancel, sampler.done
	sampler.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	sampler.mu.Lock()
	if sampler.done == done {
		sampler.cancel = nil
		sampler.done = nil
	}
	sampler.mu.Unlock()
}

func (sampler *Sampler) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	if !wait(ctx, sampler.initialDelay) {
		return
	}
	for {
		sampler.tick(ctx)
		if !wait(ctx, sampler.tickInterval) {
			return
		}
	}
}

func (sampler *Sampler) tick(ctx context.Context) {
	listed, err := sampler.nodes.List(ctx, false)
	if err != nil {
		if ctx.Err() == nil {
			sampler.logger.Error("account history sampling failed")
		}
		return
	}
	eligible := make([]*nodes.Node, 0, len(listed))
	for index := range listed {
		if len(listed[index].CodexAccounts) > 0 {
			for _, account := range listed[index].CodexAccounts {
				candidate := nodes.AccountNode(&listed[index], account)
				if account.Enabled && Eligible(candidate, sampler.connectivity) {
					eligible = append(eligible, candidate)
				}
			}
			continue
		}
		if Eligible(&listed[index], sampler.connectivity) {
			eligible = append(eligible, &listed[index])
		}
	}
	workerCount := min(sampler.workers, len(eligible))
	if workerCount == 0 {
		return
	}
	jobs := make(chan *nodes.Node, len(eligible))
	errorsFound := make(chan struct{}, workerCount)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for node := range jobs {
				if _, err := sampler.sample(ctx, node); err != nil && ctx.Err() == nil {
					select {
					case errorsFound <- struct{}{}:
					default:
					}
					return
				}
			}
		}()
	}
	for _, node := range eligible {
		select {
		case jobs <- node:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
	if len(errorsFound) > 0 {
		sampler.logger.Error("account history sampling failed")
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Sample records one Node if its durable five-minute interval is due. Account
// read errors create a gap, whereas cancellation and runtime replacement discard
// the in-flight observation.
func (sampler *Sampler) Sample(ctx context.Context, node *nodes.Node) (bool, error) {
	if node != nil && node.SelectedNodeAccountID != "" {
		return sampler.SampleAccount(ctx, node)
	}
	if ctx.Err() != nil || !Eligible(node, sampler.connectivity) {
		return false, nil
	}
	if sampler.pool == nil || sampler.nodes == nil || sampler.accounts == nil {
		return false, errors.New("account sampler is not configured")
	}
	key := RuntimeKey(node)
	connection, err := sampler.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire account sampler connection: %w", err)
	}
	defer connection.Release()
	lockKey := "mira-account:" + node.NodeID + ":" + key
	locked := false
	defer func() {
		if !locked {
			return
		}
		unlockContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = connection.Exec(unlockContext, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lockKey)
	}()
	if err := connection.QueryRow(ctx,
		"SELECT pg_try_advisory_lock(hashtextextended($1, 0))", lockKey,
	).Scan(&locked); err != nil {
		return false, fmt.Errorf("acquire account sampler advisory lock: %w", err)
	}
	if !locked {
		return false, nil
	}

	var last time.Time
	err = connection.QueryRow(ctx, `SELECT sampled_at FROM mira_account_quota_samples
		WHERE node_id=$1 AND runtime_key=$2 ORDER BY sampled_at DESC LIMIT 1`, node.NodeID, key).Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read latest account sample: %w", err)
	}
	if err == nil && sampler.now().Sub(last) < SampleInterval {
		return false, nil
	}

	status := "ok"
	var account any
	quota := Quota{}
	snapshot, readErr := sampler.accounts.Read(ctx, node.NodeID)
	if readErr != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		status = "error"
	} else {
		account = snapshot.Account
		if accountType, _ := accountField(account, "type").(string); accountType == "chatgpt" {
			quota = ProjectWeeklyQuota(snapshot.Limits)
		}
	}

	current, err := sampler.nodes.Get(ctx, node.NodeID, false)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, fmt.Errorf("refresh Node before account sample: %w", err)
	}
	if ctx.Err() != nil || current == nil || !Eligible(current, sampler.connectivity) || RuntimeKey(current) != key {
		return false, nil
	}

	timestamp := sampler.now()
	var resetsAt any
	if quota.ResetsAtMillis != nil {
		resetsAt = time.UnixMilli(*quota.ResetsAtMillis)
	}
	_, err = connection.Exec(ctx, `INSERT INTO mira_account_quota_samples
		(node_id,runtime_key,sampled_at,sample_slot,status,account_type,email,plan_type,remaining,resets_at,reset_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`,
		node.NodeID, key, timestamp, timestamp.UnixMilli()/SampleInterval.Milliseconds(), status,
		safeText(accountField(account, "type")), safeText(accountField(account, "email")),
		safeText(accountField(account, "planType")), quota.Remaining, resetsAt, quota.ResetCount)
	if err != nil {
		return false, fmt.Errorf("insert account sample: %w", err)
	}
	return true, nil
}

// Eligible excludes Nodes which cannot provide a stable App Server account
// snapshot before any database connection is acquired.
func Eligible(node *nodes.Node, connectivity Connectivity) bool {
	if node == nil || connectivity == nil || node.ApprovalStatus != "approved" || node.Status != "online" {
		return false
	}
	appServer, _ := node.Capabilities["appServer"].(bool)
	status, _ := node.ReportedAppServer["status"].(string)
	return appServer && status == "running" && connectivity.IsConnected(node.NodeID)
}

// RuntimeKey scopes samples to one Node-side Codex installation. Its encoding
// intentionally matches JSON.stringify([codexHome ?? null, codexPath ?? null]).
func RuntimeKey(node *nodes.Node) string {
	var codexHome, codexPath any
	if node != nil {
		if value, ok := node.ReportedAppServer["codexHome"].(string); ok {
			codexHome = value
		}
		if value, ok := node.ReportedAppServer["codexPath"].(string); ok {
			codexPath = value
		}
	}
	payload, _ := json.Marshal([]any{codexHome, codexPath})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func accountField(account any, field string) any {
	record, _ := account.(map[string]any)
	if record == nil {
		return nil
	}
	return record[field]
}

func safeText(value any) any {
	text, ok := value.(string)
	if !ok || len(utf16.Encode([]rune(text))) > 512 {
		return nil
	}
	for _, character := range text {
		if character <= 0x1f {
			return nil
		}
	}
	return text
}
