package views

import (
	"context"
	"sync"
	"time"
)

// conversationIndexer keeps the derived conversation index current in the
// background. Recently updated conversations are indexed first, so the first
// backfill after an upgrade serves current work before old history. Outline
// reads also catch up their own thread on demand.
type conversationIndexer struct {
	service *Service
	logger  interface{ Printf(string, ...any) }
	mu      sync.Mutex
	stalled map[string]time.Time
	wake    chan struct{}
}

const (
	indexerIdleInterval  = 10 * time.Second
	indexerBusyInterval  = 200 * time.Millisecond
	indexerThreadBudget  = 2 * time.Second
	indexerStallInterval = 5 * time.Minute
	indexerCleanInterval = 10 * time.Minute
)

// StartConversationIndexer runs until ctx ends or the returned stop is called.
func (service *Service) StartConversationIndexer(ctx context.Context, logger interface{ Printf(string, ...any) }) func() {
	ctx, cancel := context.WithCancel(ctx)
	indexer := &conversationIndexer{service: service, logger: logger, stalled: map[string]time.Time{}, wake: make(chan struct{}, 1)}
	service.indexerWake = indexer.wake
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastClean := time.Time{}
		for {
			busy := indexer.round(ctx)
			if time.Since(lastClean) > indexerCleanInterval {
				indexer.clean(ctx)
				lastClean = time.Now()
			}
			delay := indexerIdleInterval
			if busy {
				delay = indexerBusyInterval
			}
			select {
			case <-ctx.Done():
				return
			case <-indexer.wake:
			case <-time.After(delay):
			}
		}
	}()
	return func() { cancel(); <-done }
}

// WakeConversationIndexer asks the background indexer to look for new work.
func (service *Service) WakeConversationIndexer() {
	if service.indexerWake == nil {
		return
	}
	select {
	case service.indexerWake <- struct{}{}:
	default:
	}
}

func (indexer *conversationIndexer) skip(key string) bool {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	until, ok := indexer.stalled[key]
	if ok && time.Now().After(until) {
		delete(indexer.stalled, key)
		return false
	}
	return ok
}

func (indexer *conversationIndexer) stall(key string) {
	indexer.mu.Lock()
	indexer.stalled[key] = time.Now().Add(indexerStallInterval)
	indexer.mu.Unlock()
}

func (indexer *conversationIndexer) stalledKeys(prefix string) []string {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	keys := []string{}
	for key, until := range indexer.stalled {
		if time.Now().Before(until) && len(key) > len(prefix) && key[:len(prefix)] == prefix {
			keys = append(keys, key[len(prefix):])
		}
	}
	return keys
}

func (indexer *conversationIndexer) round(ctx context.Context) bool {
	busy := false
	pool := indexer.service.pool
	rows, err := pool.Query(ctx, `SELECT p.store_id,p.thread_id FROM codex_thread_projections p
	  LEFT JOIN mira_conversation_index_state s ON s.store_id=p.store_id AND s.thread_id=p.thread_id
	  WHERE (s.thread_id IS NULL OR s.generation<>p.active_generation OR s.indexed_count<p.item_count OR s.projector_version<>$1)
	    AND NOT (p.store_id||'/'||p.thread_id = ANY($2::text[]))
	  ORDER BY p.updated_at DESC LIMIT 8`, ConversationIndexVersion, indexer.stalledKeys("codex:"))
	if err != nil {
		if ctx.Err() == nil {
			indexer.logger.Printf("conversation index: list Codex work: %v", err)
		}
		return false
	}
	type work struct{ store, thread string }
	codex := []work{}
	for rows.Next() {
		var item work
		if rows.Scan(&item.store, &item.thread) == nil {
			codex = append(codex, item)
		}
	}
	rows.Close()
	for _, item := range codex {
		if ctx.Err() != nil {
			return false
		}
		key := "codex:" + item.store + "/" + item.thread
		if indexer.skip(key) {
			continue
		}
		complete, err := indexer.service.IndexCodexThread(ctx, item.store, item.thread, time.Now().Add(indexerThreadBudget))
		if err != nil {
			if ctx.Err() == nil {
				indexer.logger.Printf("conversation index: thread %s: %v", item.thread, err)
			}
			indexer.stall(key)
			continue
		}
		busy = true
		if !complete {
			// Out of budget, or canonical history is incomplete. A thread that
			// made no progress is retried later instead of starving others.
			var indexed, count int64
			_ = pool.QueryRow(ctx, `SELECT COALESCE(s.indexed_count,0),p.item_count FROM codex_thread_projections p
			  LEFT JOIN mira_conversation_index_state s USING(store_id,thread_id) WHERE p.store_id=$1 AND p.thread_id=$2`, item.store, item.thread).Scan(&indexed, &count)
			if indexed < count && !indexer.progressed(key, indexed) {
				indexer.stall(key)
			}
		}
	}
	claudeRows, err := pool.Query(ctx, `SELECT s.session_id::text,'' FROM mira_claude_sessions s
	  LEFT JOIN mira_claude_index_state i ON i.session_id=s.session_id AND i.subpath=''
	  WHERE i.session_id IS NULL OR i.projector_version<>$1 OR EXISTS(SELECT 1 FROM mira_claude_events e
	    WHERE e.session_id=s.session_id AND e.event_type<>'stream_event' AND e.seq>i.indexed_seq)
	  UNION ALL
	  SELECT t.session_id::text,t.subpath FROM mira_claude_transcripts t
	  LEFT JOIN mira_claude_index_state i ON i.session_id=t.session_id AND i.subpath=t.subpath
	  WHERE t.subpath<>'' AND (i.session_id IS NULL OR i.projector_version<>$1 OR i.indexed_seq<t.next_seq-1)
	  LIMIT 16`, ConversationIndexVersion)
	if err != nil {
		if ctx.Err() == nil {
			indexer.logger.Printf("conversation index: list Claude work: %v", err)
		}
		return busy
	}
	type claudeWork struct{ session, subpath string }
	claude := []claudeWork{}
	for claudeRows.Next() {
		var item claudeWork
		if claudeRows.Scan(&item.session, &item.subpath) == nil {
			claude = append(claude, item)
		}
	}
	claudeRows.Close()
	for _, item := range claude {
		key := "claude:" + item.session + "/" + item.subpath
		if ctx.Err() != nil || indexer.skip(key) {
			continue
		}
		if err := indexer.service.IndexClaudeTranscript(ctx, item.session, item.subpath); err != nil {
			if ctx.Err() == nil {
				indexer.logger.Printf("conversation index: Claude %s%s: %v", item.session, item.subpath, err)
			}
			indexer.stall(key)
			continue
		}
		busy = true
	}
	return busy
}

// progressed remembers the last observed count, so a thread that cannot move
// past a history gap is parked instead of being selected every round.
func (indexer *conversationIndexer) progressed(key string, indexed int64) bool {
	indexer.mu.Lock()
	defer indexer.mu.Unlock()
	if indexer.service.indexerProgress == nil {
		indexer.service.indexerProgress = map[string]int64{}
	}
	previous, seen := indexer.service.indexerProgress[key]
	indexer.service.indexerProgress[key] = indexed
	return !seen || previous != indexed
}

// clean removes index rows whose thread no longer has a projection, as a
// backstop for deletion paths that run outside this Server.
func (indexer *conversationIndexer) clean(ctx context.Context) {
	rows, err := indexer.service.pool.Query(ctx, `SELECT s.store_id,s.thread_id FROM mira_conversation_index_state s
	  WHERE NOT EXISTS(SELECT 1 FROM codex_thread_projections p WHERE p.store_id=s.store_id AND p.thread_id=s.thread_id) LIMIT 100`)
	if err != nil {
		return
	}
	orphans := [][2]string{}
	for rows.Next() {
		var store, thread string
		if rows.Scan(&store, &thread) == nil {
			orphans = append(orphans, [2]string{store, thread})
		}
	}
	rows.Close()
	for _, orphan := range orphans {
		if err := indexer.service.dropCodexIndex(ctx, orphan[0], orphan[1]); err != nil && ctx.Err() == nil {
			indexer.logger.Printf("conversation index: drop %s: %v", orphan[1], err)
		}
	}
}
