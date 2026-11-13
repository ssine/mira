package miraserver

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/clauderuntime"
	serverchannel "github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
)

const (
	claudeUsageTTL        = 5 * time.Minute
	claudeUsageErrorTTL   = 30 * time.Second
	claudeUsageMinRefresh = 30 * time.Second
	claudeUsageMaxEntries = 256
	// The sampler reuses a browser read only when it is this recent, so each
	// stored sample describes its own five-minute slot.
	claudeUsageSampleAge   = time.Minute
	claudeUsageSampleEvery = 5 * time.Minute
)

// claudeUsageCache keeps the latest claude.ai plan usage per Claude account in
// memory. Each read starts a native Claude process on the execution Node, so
// browsers and the history sampler share one in-flight read and a five-minute
// result. The cache is disposable and never used for admission decisions;
// mira_claude_account_usage_samples holds the durable observations.
type claudeUsageCache struct {
	mu      sync.Mutex
	entries map[string]*claudeUsageEntry
}

type claudeUsageEntry struct {
	revision   int64
	usage      any
	observedAt time.Time
	message    string
	status     int
	expiresAt  time.Time
	attemptAt  time.Time
	pending    chan struct{}
}

type claudeUsageAccount struct {
	nodeID     string
	authType   string
	configured bool
	enabled    bool
	revision   int64
}

func (server *Server) claudeAccountUsage(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) error {
	account, err := server.claudeUsageAccount(ctx, id)
	if err != nil {
		return err
	}
	if !account.configured {
		return claudeError(409, "Log in to this Claude subscription account first")
	}
	maxAge := claudeUsageTTL
	if r.URL.Query().Get("refresh") == "1" {
		maxAge = claudeUsageMinRefresh
	}
	entry, err := server.readClaudeUsage(ctx, id, account, maxAge)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	if entry.usage == nil {
		// An offline Node or failed read still shows the last stored sample.
		var observed time.Time
		var usage []byte
		err = server.pool.QueryRow(ctx, `SELECT sampled_at,usage FROM mira_claude_account_usage_samples
		  WHERE node_account_id=$1 AND status='ok' ORDER BY sampled_at DESC LIMIT 1`, id).Scan(&observed, &usage)
		if errors.Is(err, pgx.ErrNoRows) {
			return claudeError(entry.status, entry.message)
		}
		if err != nil {
			return err
		}
		var value any
		if err = json.Unmarshal(usage, &value); err != nil {
			return err
		}
		entry.usage, entry.observedAt = value, observed
	}
	result := map[string]any{"nodeAccountId": id, "usage": entry.usage, "stale": entry.message != "", "observedAt": entry.observedAt.UnixMilli()}
	if entry.message != "" {
		result["message"] = entry.message
	}
	return writeJSON(w, 200, result)
}

func (server *Server) claudeUsageAccount(ctx context.Context, id string) (claudeUsageAccount, error) {
	var account claudeUsageAccount
	err := server.pool.QueryRow(ctx, `SELECT node_id::text,auth_type,configured,enabled,credential_revision
	  FROM mira_claude_accounts WHERE node_account_id=$1`, id).
		Scan(&account.nodeID, &account.authType, &account.configured, &account.enabled, &account.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return account, claudeError(404, "Claude account not found")
	}
	if err != nil {
		return account, err
	}
	if account.authType != "subscription" {
		return account, claudeError(409, "Claude API accounts do not report plan usage")
	}
	return account, nil
}

func (server *Server) readClaudeUsage(ctx context.Context, id string, account claudeUsageAccount, maxAge time.Duration) (claudeUsageEntry, error) {
	return server.claudePlanUsage.read(ctx, id, account.revision, maxAge, func(ctx context.Context) (any, error) {
		node, err := server.nodes.Get(ctx, account.nodeID, false)
		if err != nil {
			return nil, err
		}
		switch {
		case node == nil || node.ApprovalStatus != "approved":
			return nil, claudeError(409, "Claude account Node is unavailable")
		case node.Capabilities["claudeUsageV1"] != true:
			return nil, claudeError(409, "Upgrade this Node to read Claude plan usage")
		case node.Status != "online" || !server.channel.IsConnected(account.nodeID):
			return nil, claudeError(409, "Claude account Node is offline")
		}
		return server.channel.Invoke(ctx, account.nodeID, "claude", map[string]any{
			"action": "account/usage", "nodeAccountId": id,
		}, 45*time.Second)
	})
}

// claudeAccountUsageHistory returns stored weekly and five-hour remaining
// percentages. Failed samples are gaps; offline periods have no rows.
func (server *Server) claudeAccountUsageHistory(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) error {
	if _, err := server.claudeUsageAccount(ctx, id); err != nil {
		return err
	}
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "7d"
	}
	duration, ok := serverviews.HistoryRanges[rangeName]
	if !ok {
		return claudeError(400, "range must be 24h, 7d or 30d")
	}
	now := time.Now()
	from := now.Add(-duration)
	rows, err := server.pool.Query(ctx, `SELECT sampled_at,status,usage FROM mira_claude_account_usage_samples
	  WHERE node_account_id=$1 AND sampled_at BETWEEN $2 AND $3 ORDER BY sampled_at LIMIT 8642`, id, from, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	points := []map[string]any{}
	for rows.Next() {
		var at time.Time
		var status string
		var raw []byte
		if err = rows.Scan(&at, &status, &raw); err != nil {
			return err
		}
		point := map[string]any{"at": at.UnixMilli(), "status": status, "remaining": nil, "session": nil, "resetsAt": nil}
		var usage clauderuntime.PlanUsage
		if status == "ok" && json.Unmarshal(raw, &usage) == nil {
			if remaining, resetsAt := claudeRemaining(usage.SevenDay); remaining != nil {
				point["remaining"], point["resetsAt"] = *remaining, resetsAt
			}
			if remaining, _ := claudeRemaining(usage.FiveHour); remaining != nil {
				point["session"] = *remaining
			}
		}
		points = append(points, point)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, 200, map[string]any{"nodeAccountId": id, "range": rangeName, "from": from.UnixMilli(), "to": now.UnixMilli(),
		"intervalMs": claudeUsageSampleEvery.Milliseconds(), "points": points})
}

func claudeRemaining(window *clauderuntime.UsageWindow) (*float64, any) {
	if window == nil || window.Utilization == nil {
		return nil, nil
	}
	remaining := math.Max(0, math.Min(100, 100-*window.Utilization))
	var resetsAt any
	if window.ResetsAt != nil {
		resetsAt = *window.ResetsAt
	}
	return &remaining, resetsAt
}

// startClaudeUsageSampler records plan usage for enabled, logged-in Claude
// subscription accounts every five minutes, even when no browser is open.
// Only approved, connected Nodes that advertise claudeUsageV1 are read.
func (server *Server) startClaudeUsageSampler(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		delay := 5 * time.Second
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			delay = 30 * time.Second
			if err := server.sampleClaudeUsage(ctx); err != nil && ctx.Err() == nil {
				server.config.Logger.Print("Claude plan usage sampling failed")
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (server *Server) sampleClaudeUsage(ctx context.Context) error {
	rows, err := server.pool.Query(ctx, `SELECT a.node_account_id::text,a.node_id::text,a.credential_revision
	  FROM mira_claude_accounts a
	  WHERE a.auth_type='subscription' AND a.configured AND a.enabled
	    AND NOT EXISTS (SELECT 1 FROM mira_claude_account_usage_samples s
	      WHERE s.node_account_id=a.node_account_id AND s.sampled_at > now()-interval '5 minutes')
	  ORDER BY a.node_account_id LIMIT 64`)
	if err != nil {
		return err
	}
	type due struct {
		id      string
		account claudeUsageAccount
	}
	var accounts []due
	for rows.Next() {
		var value due
		if err = rows.Scan(&value.id, &value.account.nodeID, &value.account.revision); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, value)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, value := range accounts {
		node, err := server.nodes.Get(ctx, value.account.nodeID, false)
		if err != nil {
			return err
		}
		if node == nil || node.ApprovalStatus != "approved" || node.Status != "online" ||
			node.Capabilities["claudeUsageV1"] != true || !server.channel.IsConnected(node.NodeID) {
			continue
		}
		entry, err := server.readClaudeUsage(ctx, value.id, value.account, claudeUsageSampleAge)
		if err != nil {
			return err
		}
		status, at, usage := "error", time.Now(), []byte(nil)
		if entry.usage != nil && entry.message == "" {
			status, at = "ok", entry.observedAt
			if usage, err = json.Marshal(entry.usage); err != nil {
				return err
			}
		}
		_, err = server.pool.Exec(ctx, `INSERT INTO mira_claude_account_usage_samples
		  (node_account_id,sample_slot,sampled_at,credential_revision,status,usage)
		  SELECT $1,$2,$3,$4,$5,$6::jsonb WHERE EXISTS (SELECT 1 FROM mira_claude_accounts WHERE node_account_id=$1)
		  ON CONFLICT DO NOTHING`, value.id, at.UnixMilli()/claudeUsageSampleEvery.Milliseconds(), at, value.account.revision, status, usage)
		if err != nil {
			return err
		}
	}
	return nil
}

// read returns a snapshot of the account's entry, refreshing it when its last
// attempt expired or is older than maxAge.
// A failed refresh keeps the last successful value for the same credential
// revision and reports the failure beside it.
func (cache *claudeUsageCache) read(ctx context.Context, id string, revision int64, maxAge time.Duration, load func(context.Context) (any, error)) (claudeUsageEntry, error) {
	for {
		now := time.Now()
		cache.mu.Lock()
		if cache.entries == nil {
			cache.entries = map[string]*claudeUsageEntry{}
		}
		entry := cache.entries[id]
		if entry != nil && entry.revision != revision && entry.pending == nil {
			delete(cache.entries, id)
			entry = nil
		}
		if entry != nil && entry.pending != nil {
			pending := entry.pending
			cache.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return claudeUsageEntry{}, ctx.Err()
			}
		}
		fresh := entry != nil && now.Before(entry.expiresAt) && now.Sub(entry.attemptAt) < maxAge
		if fresh {
			snapshot := *entry
			cache.mu.Unlock()
			return snapshot, nil
		}
		if entry == nil {
			cache.pruneLocked(now)
			entry = &claudeUsageEntry{revision: revision}
			cache.entries[id] = entry
		}
		pending := make(chan struct{})
		entry.pending = pending
		entry.attemptAt = now
		cache.mu.Unlock()

		// The browser may disconnect; the shared read still completes so other
		// callers and the next poll reuse it.
		loadContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 50*time.Second)
		usage, err := load(loadContext)
		cancel()

		cache.mu.Lock()
		finished := time.Now()
		if err == nil && usage != nil {
			entry.usage, entry.observedAt, entry.message = usage, finished, ""
			entry.expiresAt = finished.Add(claudeUsageTTL)
		} else {
			entry.status, entry.message = claudeUsageFailure(err)
			entry.expiresAt = finished.Add(claudeUsageErrorTTL)
		}
		entry.pending = nil
		close(pending)
		snapshot := *entry
		cache.mu.Unlock()
		return snapshot, nil
	}
}

func (cache *claudeUsageCache) pruneLocked(now time.Time) {
	if len(cache.entries) < claudeUsageMaxEntries {
		return
	}
	for key, entry := range cache.entries {
		if entry.pending == nil && now.Sub(entry.attemptAt) > time.Hour {
			delete(cache.entries, key)
		}
	}
	for key, entry := range cache.entries {
		if len(cache.entries) < claudeUsageMaxEntries {
			break
		}
		if entry.pending == nil {
			delete(cache.entries, key)
		}
	}
}

func claudeUsageFailure(err error) (int, string) {
	var public *HTTPError
	if errors.As(err, &public) {
		return public.Status, public.Message
	}
	var base *foundation.HTTPError
	if errors.As(err, &base) {
		return base.Status, base.Message
	}
	var channelError *serverchannel.Error
	if errors.As(err, &channelError) {
		return channelError.Status, channelError.Message
	}
	message := "Claude plan usage is unavailable"
	if err != nil {
		message = err.Error()
	}
	if len(message) > 300 {
		message = message[:300]
	}
	return 502, message
}
