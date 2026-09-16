package views

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const accountCostPredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(session_meta|turn_context|thread_settings_applied|token_count)"'`
const accountCostPageSize = 1024

// Bump the algorithm version when parsing, attribution inputs or timestamp
// restoration changes. Prices are fingerprinted automatically. No raw history
// or model credentials are stored in the projection.
var accountCostRevision = func() string {
	keys := make([]string, 0, len(modelPrices))
	for key := range modelPrices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var data strings.Builder
	data.WriteString("account-cost-v1/" + PricingDate)
	for _, key := range keys {
		fmt.Fprintf(&data, "/%s:%+v", key, modelPrices[key])
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(data.String())))
}()

// Imports publish immutable provenance. A newly published import invalidates a
// previous native-timestamp projection even if item_count did not change.
const accountCostSourcesSQL = `SELECT p.store_id,p.thread_id,p.active_generation,p.item_count,
 coalesce(p.state#>>'{createdThread,forked_from_id}','') AS fork,
 jsonb_build_array(coalesce(p.state#>>'{createdThread,forked_from_id}',''),i.import_id)::text AS source_key
 FROM codex_thread_projections p LEFT JOIN LATERAL (
 SELECT import_id FROM mira_codex_session_imports WHERE store_id=p.store_id AND thread_id=p.thread_id AND status='imported'
 ORDER BY store_event_seq DESC,created_at DESC LIMIT 1) i ON true`

type accountCostSource struct {
	store, thread     string
	generation, count int64
	fork, key         string
}

// Explicit versioned serialization: CostProjection itself has private fields
// and deliberately bounded per-turn state, which the account ledger does not need.
type accountCostNumbers struct {
	Observed                       bool
	Priced, Unpriced, LongRequests int64
	Models, Reasons                []string
	Input, Cached, Write, Output   string
}

func costNumbers(total costTotals) accountCostNumbers {
	return accountCostNumbers{total.observed, total.priced, total.unpriced, total.longRequests,
		append([]string{}, total.models...), append([]string{}, total.reasons...),
		total.input.String(), total.cached.String(), total.write.String(), total.output.String()}
}

func (value accountCostNumbers) totals() (costTotals, error) {
	result := costTotals{observed: value.Observed, priced: value.Priced, unpriced: value.Unpriced,
		longRequests: value.LongRequests, models: value.Models, reasons: value.Reasons}
	if _, ok := result.input.SetString(value.Input, 10); !ok {
		return result, errors.New("invalid cost checkpoint input")
	}
	if _, ok := result.cached.SetString(value.Cached, 10); !ok {
		return result, errors.New("invalid cost checkpoint cached input")
	}
	if _, ok := result.write.SetString(value.Write, 10); !ok {
		return result, errors.New("invalid cost checkpoint cache write")
	}
	if _, ok := result.output.SetString(value.Output, 10); !ok {
		return result, errors.New("invalid cost checkpoint output")
	}
	return result, nil
}

type accountCostCheckpoint struct {
	Totals                             accountCostNumbers
	Model, Turn, Provider              string
	Last, ForkBaseline                 [4]int64
	AwaitFork, ScopeStarted, ForkReset bool
}

func checkpointCost(state *CostProjection, provider string) accountCostCheckpoint {
	return accountCostCheckpoint{costNumbers(state.costTotals), state.model, state.turnID, provider, state.last,
		state.forkUsageBaseline, state.awaitForkBoundary, state.scopeStarted, state.forkUsageReset}
}

func (value accountCostCheckpoint) restore() (*CostProjection, error) {
	state := NewCostProjection(value.AwaitFork, []string{})
	var err error
	state.costTotals, err = value.Totals.totals()
	state.model, state.turnID, state.last = value.Model, value.Turn, value.Last
	state.scopeStarted, state.forkUsageBaseline, state.forkUsageReset = value.ScopeStarted, value.ForkBaseline, value.ForkReset
	return state, err
}

// StartAccountCostProjector owns one bounded background reader. Every page and
// its cursor commit together. Client cancellation, shutdown or a lost commit
// response can never force a replay of the entire account's history.
func (service *Service) StartAccountCostProjector(ctx context.Context, logger *log.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			work, err := service.projectAccountCosts(ctx)
			if err != nil && ctx.Err() == nil {
				logger.Printf("account cost projection batch failed: %v", err)
			}
			delay := 2 * time.Second
			if work > 0 {
				delay = 25 * time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); wg.Wait() }
}

func (service *Service) projectAccountCosts(ctx context.Context) (int, error) {
	// Oldest serviced first: a growing conversation cannot starve another
	// conversation's backfill. Errors back off per thread, not for the whole queue.
	rows, err := service.pool.Query(ctx, `WITH sources AS (`+accountCostSourcesSQL+`)
 SELECT s.store_id,s.thread_id,s.active_generation,s.item_count,s.fork,s.source_key FROM sources s
 LEFT JOIN mira_account_cost_checkpoints c USING(store_id,thread_id)
 WHERE (c.thread_id IS NULL OR c.generation<>s.active_generation OR c.revision<>$1 OR c.source_key<>s.source_key OR c.item_seq<>s.item_count)
 AND (c.retry_at IS NULL OR c.retry_at<=now()) ORDER BY c.updated_at NULLS FIRST,s.store_id,s.thread_id LIMIT 16`, accountCostRevision)
	if err != nil {
		return 0, err
	}
	sources := []accountCostSource{}
	for rows.Next() {
		var source accountCostSource
		if err := rows.Scan(&source.store, &source.thread, &source.generation, &source.count, &source.fork, &source.key); err != nil {
			rows.Close()
			return 0, err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var result error
	for _, source := range sources {
		pageCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := service.projectAccountCostPage(pageCtx, source)
		cancel()
		if err != nil {
			result = errors.Join(result, err)
			if ctx.Err() != nil {
				return len(sources), result
			}
			// Store only a stable diagnostic code, never raw records/errors.
			_, recordErr := service.pool.Exec(ctx, `INSERT INTO mira_account_cost_checkpoints
 (store_id,thread_id,generation,revision,source_key,retry_at,error_code) SELECT $1,$2,$3,$4,$5,now()+interval '30 seconds','projection_failed'
 WHERE EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2)
 ON CONFLICT(store_id,thread_id) DO UPDATE SET retry_at=now()+interval '30 seconds',error_code='projection_failed'
 WHERE mira_account_cost_checkpoints.generation=$3 AND mira_account_cost_checkpoints.revision=$4
 AND mira_account_cost_checkpoints.source_key=$5 AND mira_account_cost_checkpoints.item_seq<$6`,
				source.store, source.thread, source.generation, accountCostRevision, source.key, source.count)
			result = errors.Join(result, recordErr)
		}
	}
	return len(sources), result
}

func (service *Service) projectAccountCostPage(ctx context.Context, source accountCostSource) error {
	tx, err := service.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	// Only competing projection writers share this lock; canonical writers do not.
	var acquired bool
	err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,37))`, source.store+"/"+source.thread).Scan(&acquired)
	if err != nil || !acquired {
		return err
	}
	var generation, cursor int64
	var revision, key string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT generation,revision,source_key,item_seq,checkpoint FROM mira_account_cost_checkpoints WHERE store_id=$1 AND thread_id=$2`, source.store, source.thread).
		Scan(&generation, &revision, &key, &cursor, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	reset := errors.Is(err, pgx.ErrNoRows) || generation != source.generation || revision != accountCostRevision || key != source.key
	if !reset && cursor > source.count {
		return nil
	} // A concurrent page already passed this target.
	state, provider := NewCostProjection(source.fork != "", []string{}), ""
	if reset {
		cursor = 0
		if _, err := tx.Exec(ctx, `DELETE FROM mira_account_cost_checkpoints WHERE store_id=$1 AND thread_id=$2`, source.store, source.thread); err != nil {
			return err
		}
	} else if cursor > 0 {
		var saved accountCostCheckpoint
		if err := json.Unmarshal(raw, &saved); err != nil {
			return err
		}
		state, err = saved.restore()
		provider = saved.Provider
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO mira_account_cost_checkpoints(store_id,thread_id,generation,revision,source_key)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, source.store, source.thread, source.generation, accountCostRevision, source.key); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT item_seq,payload,created_at FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 AND generation=$3
 AND item_seq>$4 AND item_seq<=$5 AND `+accountCostPredicate+` ORDER BY item_seq LIMIT $6`, source.store, source.thread, source.generation, cursor, source.count, accountCostPageSize)
	if err != nil {
		return err
	}
	page := []transcriptRow{}
	for rows.Next() {
		var seq int64
		var payload []byte
		var created time.Time
		if err := rows.Scan(&seq, &payload, &created); err != nil {
			rows.Close()
			return err
		}
		record, err := decodeObject(payload)
		if err != nil {
			rows.Close()
			return err
		}
		page = append(page, transcriptRow{seq, record, &created})
		cursor = seq
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	recorded, err := restoreImportedTimestampsFrom(ctx, tx, source.store, source.thread, page)
	if err != nil {
		return err
	}
	entries := [][]any{}
	for _, row := range page {
		record, payload := row.payload, object(row.payload["payload"])
		if stringValue(record["type"]) == "session_meta" {
			provider = stringValue(payload["model_provider"])
		}
		if stringValue(record["type"]) == "event_msg" && stringValue(payload["type"]) == "thread_settings_applied" {
			if value := stringValue(object(payload["thread_settings"])["model_provider_id"]); value != "" {
				provider = value
			}
		}
		before := cloneCostTotals(state.costTotals)
		ApplyCostRecord(state, record, source.thread)
		if !state.scopeStarted {
			continue
		}
		delta := accountCostDelta(before, state.costTotals)
		if !delta.observed && len(delta.reasons) == 0 {
			continue
		}
		stamp := recordTimestamp(record)
		if stamp == "" {
			stamp = recorded[row.sequence]
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		value := costNumbers(delta)
		entries = append(entries, []any{source.store, source.thread, row.sequence, at, state.turnID, provider, value.Observed, value.Priced, value.Unpriced,
			value.LongRequests, value.Input, value.Cached, value.Write, value.Output, value.Models, value.Reasons})
	}
	if len(entries) > 0 {
		// Numeric text is sent through INSERT casts; batched statements keep the
		// wire bounded without pgx binary NUMERIC coercion or floating point money.
		batch := &pgx.Batch{}
		for _, row := range entries {
			batch.Queue(`INSERT INTO mira_account_cost_entries (store_id,thread_id,item_seq,happened_at,turn_id,provider,observed,priced,unpriced,long_requests,input,cached,write,output,models,reasons) VALUES
 ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::text::numeric,$12::text::numeric,$13::text::numeric,$14::text::numeric,$15,$16)`, row...)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
	}
	if len(page) < accountCostPageSize {
		cursor = source.count
	}
	encoded, err := json.Marshal(checkpointCost(state, provider))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE mira_account_cost_checkpoints SET item_seq=$3,checkpoint=$4,updated_at=now(),retry_at=NULL,error_code=NULL
 WHERE store_id=$1 AND thread_id=$2`, source.store, source.thread, cursor, encoded)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
