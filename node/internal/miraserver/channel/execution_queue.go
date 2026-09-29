package channel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type executionWaiter struct {
	gate chan struct{}
	refs int
}

type executionQueue struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	slots    chan struct{}
	families map[string]*executionWaiter
	waiters  int
	closed   bool
}

func (queue *executionQueue) init(db Database) {
	if queue.ctx != nil {
		return
	}
	queue.ctx, queue.cancel = context.WithCancel(context.Background())
	limit := 4
	if pool, ok := db.(interface{ Config() *pgxpool.Config }); ok {
		limit = max(1, min(8, int(pool.Config().MaxConns)/2))
	}
	queue.slots = make(chan struct{}, limit)
	queue.families = map[string]*executionWaiter{}
}

func (queue *executionQueue) close() {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.closed = true
	if queue.cancel != nil {
		queue.cancel()
	}
}

// QueueExecution serializes a family before it starts a database transaction.
// A short ancestor lookup releases its connection before waiting. A second
// bounded gate reserves pool capacity for persistence, authentication and
// heartbeats, including while an actual account handoff waits on a Node.
// PostgreSQL gates and in-transaction membership checks remain authoritative.
func (channel *Channel) QueueExecution(parent context.Context, storeID, threadID string) (context.Context, string, func(), error) {
	queue := &channel.executionQueue
	queue.mu.Lock()
	queue.init(channel.db)
	if queue.closed || queue.waiters >= 1024 || queue.ctx.Err() != nil {
		queue.mu.Unlock()
		return nil, "", nil, errors.New("会话恢复队列繁忙，请稍后重试")
	}
	queue.waiters++
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	stop := context.AfterFunc(queue.ctx, cancel)
	queue.mu.Unlock()
	finish := func() {
		stop()
		cancel()
		queue.mu.Lock()
		queue.waiters--
		queue.mu.Unlock()
	}
	acquire := func(gate chan struct{}) error {
		select {
		case gate <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := acquire(queue.slots); err != nil {
		finish()
		return nil, "", nil, err
	}
	root, err := readExecutionRoot(ctx, channel.db, storeID, threadID)
	<-queue.slots
	if err != nil {
		finish()
		return nil, "", nil, err
	}
	key := storeID + ":" + root
	queue.mu.Lock()
	waiter := queue.families[key]
	if waiter == nil {
		waiter = &executionWaiter{gate: make(chan struct{}, 1)}
		queue.families[key] = waiter
	}
	waiter.refs++
	queue.mu.Unlock()
	unref := func() {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		waiter.refs--
		if waiter.refs == 0 {
			delete(queue.families, key)
		}
	}
	if err := acquire(waiter.gate); err != nil {
		unref()
		finish()
		return nil, "", nil, err
	}
	if err := acquire(queue.slots); err != nil {
		<-waiter.gate
		unref()
		finish()
		return nil, "", nil, err
	}
	var once sync.Once
	release := func() { once.Do(func() { <-queue.slots; <-waiter.gate; unref(); finish() }) }
	return ctx, root, release, nil
}

// Follow only ancestors using indexed lookups; never load all siblings just
// to decide which execution queue a request belongs to. Stale generation edges
// suppress the legacy fallback exactly as in executionFamilySQL.
const executionParentColumns = `p.thread_id, CASE WHEN e.child_thread_id IS NULL THEN legacy.thread_id
 WHEN e.child_generation=p.active_generation AND e.parent_generation=parent.active_generation THEN parent.thread_id END`
const executionParentJoins = ` LEFT JOIN mira_agent_graph_edges e ON e.store_id=p.store_id AND e.child_thread_id=p.thread_id
 LEFT JOIN codex_thread_projections parent ON parent.store_id=e.store_id AND parent.thread_id=e.parent_thread_id
 LEFT JOIN codex_thread_projections legacy ON e.child_thread_id IS NULL AND legacy.store_id=p.store_id AND legacy.thread_id=p.parent_thread_id `
const executionRootSQL = `WITH RECURSIVE ancestors(id,parent) AS (
 SELECT ` + executionParentColumns + ` FROM codex_thread_projections p ` + executionParentJoins + ` WHERE p.store_id=$1 AND p.thread_id=$2
 UNION SELECT ` + executionParentColumns + ` FROM ancestors a JOIN codex_thread_projections p ON p.store_id=$1 AND p.thread_id=a.parent ` + executionParentJoins + `
) SELECT count(*) FILTER(WHERE parent IS NULL),coalesce(min(id) FILTER(WHERE parent IS NULL),'') FROM ancestors`

func readExecutionRoot(ctx context.Context, query Database, storeID, threadID string) (string, error) {
	var count int
	var root string
	if err := query.QueryRow(ctx, executionRootSQL, storeID, threadID).Scan(&count, &root); err != nil {
		return "", err
	}
	if count != 1 {
		return "", errors.New("对话尚未完成持久化或父子关系存在循环，请稍后重试")
	}
	return root, nil
}

func (channel *Channel) executionNode(ctx context.Context, tx pgx.Tx, id string) (*nodes.Node, error) {
	if registry, ok := channel.nodes.(interface {
		GetWithQuery(context.Context, nodes.Querier, string, bool) (*nodes.Node, error)
	}); ok {
		return registry.GetWithQuery(ctx, tx, id, false)
	}
	// In-memory registries used by embedded consumers have no pool dependency.
	return channel.nodes.Get(ctx, id, false)
}
