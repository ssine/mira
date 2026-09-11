package miraserver

import (
	"context"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

var agentGraphPattern = regexp.MustCompile(`^/v2/stores/([^/]+)/agent-graph$`)

type agentGraphRequest struct {
	Operation string `json:"operation"`
	ParentID  string `json:"parentThreadId,omitempty"`
	ChildID   string `json:"childThreadId,omitempty"`
	Status    string `json:"status,omitempty"`
}

// Current generations are part of the edge identity. Recreating either thread
// cannot attach the replacement to a previous generation's graph.
const activeAgentEdges = `SELECT e.* FROM mira_agent_graph_edges e
 JOIN codex_thread_projections p ON p.store_id=e.store_id AND p.thread_id=e.parent_thread_id AND p.active_generation=e.parent_generation
 JOIN codex_thread_projections c ON c.store_id=e.store_id AND c.thread_id=e.child_thread_id AND c.active_generation=e.child_generation
 WHERE e.store_id=$1`

func (server *Server) routeAgentGraph(ctx context.Context, response http.ResponseWriter, request *http.Request) (bool, error) {
	match := pathMatch(agentGraphPattern, request.URL.Path)
	if match == nil {
		return false, nil
	}
	if request.Method != http.MethodPost {
		return true, &HTTPError{Status: 405, Code: "method_not_allowed", Message: "use POST"}
	}
	principal, err := server.authorize(ctx, response, request, "trusted", authOptions{ClientType: "codex"})
	if err != nil || principal == nil {
		return true, err
	}
	storeID, err := requireStoreID(match[1])
	if err != nil {
		return true, err
	}
	var body agentGraphRequest
	if err := foundation.ReadJSON(request, &body, 4096); err != nil {
		return true, err
	}
	if err := server.observeAccountProtocol(ctx, request, principal); err != nil {
		return true, err
	}
	result, err := agentGraphOperation(withCodexWriter(ctx, request, principal), server.pool, storeID, body, request.Header.Get("X-Codex-Operation-Id"))
	if err != nil {
		return true, err
	}
	return true, writeJSON(response, 200, result)
}

func agentGraphOperation(ctx context.Context, pool *pgxpool.Pool, storeID string, body agentGraphRequest, operationID string) (map[string]any, error) {
	invalid := func(message string) (map[string]any, error) {
		return nil, &HTTPError{Status: 400, Code: "invalid_agent_graph", Message: message}
	}
	validID := func(value string) bool { return operationIDPattern.MatchString(value) }
	if body.Status != "" && body.Status != "open" && body.Status != "closed" {
		return invalid("invalid edge status")
	}
	if body.Operation == "children" || body.Operation == "descendants" {
		if !validID(body.ParentID) {
			return invalid("invalid parent thread id")
		}
		query := `WITH RECURSIVE edges AS (` + activeAgentEdges + `), tree AS (
 SELECT child_thread_id,1 AS depth,ARRAY[parent_thread_id,child_thread_id] AS path FROM edges
 WHERE parent_thread_id=$2 AND ($3='' OR status=$3)
 UNION ALL SELECT e.child_thread_id,t.depth+1,t.path || e.child_thread_id FROM tree t JOIN edges e ON e.parent_thread_id=t.child_thread_id
 WHERE $4 AND ($3='' OR e.status=$3) AND NOT e.child_thread_id=ANY(t.path)
 ) SELECT child_thread_id FROM tree GROUP BY child_thread_id ORDER BY min(depth),child_thread_id`
		rows, err := pool.Query(ctx, query, storeID, body.ParentID, body.Status, body.Operation == "descendants")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return map[string]any{"threadIds": ids}, rows.Err()
	}
	if (body.Operation != "upsert" && body.Operation != "status") || !validID(body.ChildID) || body.Status == "" || !validID(operationID) {
		return invalid("invalid graph mutation or operation UUID")
	}
	if body.Operation == "upsert" && (!validID(body.ParentID) || body.ParentID == body.ChildID) {
		return invalid("invalid parent thread id")
	}
	digest, err := digestJSON(body)
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// Graph changes are infrequent. The store gate serializes cycle checks and
	// aligns generation/writer checks with history replacement and account handoff.
	if err := lockScope(ctx, tx, storeID, nil); err != nil {
		return nil, err
	}
	var previous string
	err = tx.QueryRow(ctx, `SELECT request_sha256 FROM mira_agent_graph_events WHERE store_id=$1 AND operation_id=$2::uuid`, storeID, operationID).Scan(&previous)
	if err == nil {
		if previous != digest {
			return nil, &HTTPError{Status: 409, Code: "operation_conflict", Message: "operation UUID was already used for a different graph request"}
		}
		return map[string]any{"duplicate": true}, nil
	}
	if err != pgx.ErrNoRows {
		return nil, err
	}
	parent := body.ParentID
	var parentGeneration, childGeneration int64
	if body.Operation == "status" {
		err = tx.QueryRow(ctx, `WITH edges AS (`+activeAgentEdges+`) SELECT parent_thread_id,parent_generation,child_generation FROM edges WHERE child_thread_id=$2`, storeID, body.ChildID).Scan(&parent, &parentGeneration, &childGeneration)
		if err != nil && err != pgx.ErrNoRows {
			return nil, err
		}
		if err == pgx.ErrNoRows {
			parent = ""
		}
	} else {
		err = tx.QueryRow(ctx, `SELECT p.active_generation,c.active_generation FROM codex_thread_projections p JOIN codex_thread_projections c USING(store_id)
 WHERE p.store_id=$1 AND p.thread_id=$2 AND c.thread_id=$3`, storeID, parent, body.ChildID).Scan(&parentGeneration, &childGeneration)
		if err == pgx.ErrNoRows {
			return nil, &HTTPError{Status: 409, Code: "thread_not_persisted", Message: "persist both threads before their graph edge"}
		}
		if err != nil {
			return nil, err
		}
		var cycle bool
		err = tx.QueryRow(ctx, `WITH RECURSIVE edges AS (`+activeAgentEdges+`), ancestors(id) AS (
 SELECT $2::text UNION SELECT e.parent_thread_id FROM edges e JOIN ancestors a ON e.child_thread_id=a.id
 ) SELECT EXISTS(SELECT 1 FROM ancestors WHERE id=$3)`, storeID, parent, body.ChildID).Scan(&cycle)
		if err != nil {
			return nil, err
		}
		if cycle {
			return invalid("thread-spawn cycle")
		}
	}
	ids := []string{body.ChildID}
	if parent != "" {
		ids = append(ids, parent)
	}
	if err := checkAccountHistoryWriter(ctx, tx, storeID, ids); err != nil {
		return nil, err
	}
	var sequence int64
	err = tx.QueryRow(ctx, `INSERT INTO mira_agent_graph_events(store_id,operation_id,request_sha256,child_thread_id,parent_thread_id,child_generation,parent_generation,status)
 VALUES($1,$2::uuid,$3,$4,NULLIF($5,''),$6,$7,$8) RETURNING event_seq`, storeID, operationID, digest, body.ChildID, parent, childGeneration, parentGeneration, body.Status).Scan(&sequence)
	if err != nil {
		return nil, err
	}
	if parent != "" {
		_, err = tx.Exec(ctx, `INSERT INTO mira_agent_graph_edges(store_id,child_thread_id,parent_thread_id,child_generation,parent_generation,status,event_seq)
 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(store_id,child_thread_id) DO UPDATE SET parent_thread_id=EXCLUDED.parent_thread_id,
 child_generation=EXCLUDED.child_generation,parent_generation=EXCLUDED.parent_generation,status=EXCLUDED.status,event_seq=EXCLUDED.event_seq`, storeID, body.ChildID, parent, childGeneration, parentGeneration, body.Status, sequence)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"duplicate": false}, nil
}
