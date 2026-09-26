package miraserver

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/executionstate"
)

func syncExecutionHistory(ctx context.Context, tx pgx.Tx, store string, before, after map[string]historyEntry) error {
	for id, previous := range before {
		next, exists := after[id]
		if !exists || next.Generation != previous.Generation || next.ItemCount <= previous.ItemCount {
			continue
		}
		if err := executionstate.ApplyAppends(ctx, tx, store, id, next.Generation, previous.ItemCount, next.ItemCount); err != nil {
			return err
		}
	}
	return nil
}
