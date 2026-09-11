package miraserver

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type codexWriterKey struct{}
type codexWriter struct{ NodeID, BindingID, RuntimeID string }

// The authenticated principal supplies the Node identity; never accept an
// internal identity header from the caller. Legacy clients resolve to default.
func withCodexWriter(ctx context.Context, request *http.Request, principal *foundation.Principal) context.Context {
	if principal.Kind != "node" {
		return ctx
	}
	return context.WithValue(ctx, codexWriterKey{}, codexWriter{principal.NodeID, request.Header.Get("X-Mira-Codex-Account"), request.Header.Get("X-Mira-Codex-Runtime")})
}

// Called inside the existing canonical history transaction and its thread
// locks. Handoff takes these same locks after the old runtime has drained.
// Administrative imports and storage rebuilds do not impersonate a model.
func checkAccountHistoryWriter(ctx context.Context, tx pgx.Tx, storeID string, threadIDs []string) error {
	writer, ok := ctx.Value(codexWriterKey{}).(codexWriter)
	if !ok || len(threadIDs) == 0 {
		return nil
	}
	var stale bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mira_codex_execution_routes r
	 JOIN mira_node_codex_accounts b USING(node_account_id)
	 WHERE r.store_id=$1 AND r.thread_id=ANY($2::text[]) AND
	 (b.node_id::text<>$3 OR NOT b.enabled OR
	 CASE WHEN $4='' THEN NOT b.is_default ELSE r.node_account_id::text<>$4 END OR
	 ($5<>'' AND r.runtime_id<>$5)))`, storeID, threadIDs, writer.NodeID, writer.BindingID, writer.RuntimeID).Scan(&stale)
	if err != nil {
		return err
	}
	if stale {
		return &HTTPError{Status: 409, Code: "account_execution_changed", Message: "conversation execution moved to another account or runtime; reopen it before continuing"}
	}
	return nil
}
