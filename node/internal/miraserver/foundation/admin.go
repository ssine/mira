package foundation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetAdminPassword creates the singleton administrator or replaces its
// credentials, revoking all existing sessions on replacement.
func SetAdminPassword(ctx context.Context, pool *pgxpool.Pool, username, password string) (string, error) {
	if !ValidAdminUsername(username) {
		return "", fmt.Errorf("invalid administrator username")
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin administrator update: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var adminUserID string
	err = tx.QueryRow(ctx, "SELECT admin_user_id::text FROM mira_admin_users LIMIT 1").Scan(&adminUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`INSERT INTO mira_admin_users (username, password_hash) VALUES ($1, $2) RETURNING admin_user_id::text`,
			username, passwordHash,
		).Scan(&adminUserID)
	} else if err == nil {
		_, err = tx.Exec(ctx,
			`UPDATE mira_admin_users SET username = $2, password_hash = $3, updated_at = NOW() WHERE admin_user_id = $1::uuid`,
			adminUserID, username, passwordHash,
		)
		if err == nil {
			_, err = tx.Exec(ctx,
				`UPDATE mira_admin_sessions SET revoked_at = NOW() WHERE admin_user_id = $1::uuid AND revoked_at IS NULL`,
				adminUserID,
			)
		}
	}
	if err != nil {
		return "", fmt.Errorf("configure administrator: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit administrator update: %w", err)
	}
	return adminUserID, nil
}
