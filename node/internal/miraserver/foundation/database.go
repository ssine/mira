package foundation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockSQL = "SELECT pg_advisory_lock(1298756193, 1298753394)"
const migrationUnlockSQL = "SELECT pg_advisory_unlock(1298756193, 1298753394)"

func OpenPool(ctx context.Context, config Config) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	poolConfig.MaxConns = config.PoolSize
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

func CurrentSchemaVersion() int {
	if len(schemaMigrations) == 0 {
		return 0
	}
	return schemaMigrations[len(schemaMigrations)-1].Version
}

func Migrations() []Migration {
	result := make([]Migration, len(schemaMigrations))
	copy(result, schemaMigrations)
	return result
}

// InitializeDatabase applies every missing migration through throughVersion.
// Omit throughVersion to migrate to the current schema. A session advisory lock
// serializes concurrent Server startups; every migration still has its own
// transaction, matching the released migration semantics.
func InitializeDatabase(ctx context.Context, pool *pgxpool.Pool, throughVersion ...int) error {
	limit := CurrentSchemaVersion()
	if len(throughVersion) > 1 {
		return fmt.Errorf("at most one throughVersion may be supplied")
	}
	if len(throughVersion) == 1 {
		if throughVersion[0] < 0 {
			return fmt.Errorf("throughVersion must not be negative")
		}
		limit = throughVersion[0]
	}
	if err := validateMigrations(); err != nil {
		return err
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, migrationLockSQL); err != nil {
		return fmt.Errorf("lock database migrations: %w", err)
	}
	defer func() { _, _ = connection.Exec(context.Background(), migrationUnlockSQL) }()
	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS codex_schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	for _, migration := range schemaMigrations {
		if migration.Version > limit {
			break
		}
		var name, checksum string
		err := connection.QueryRow(ctx,
			"SELECT name, checksum FROM codex_schema_migrations WHERE version = $1",
			migration.Version,
		).Scan(&name, &checksum)
		if err == nil {
			if name != migration.Name || checksum != migration.Checksum {
				return fmt.Errorf("database migration %d checksum mismatch", migration.Version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read database migration %d: %w", migration.Version, err)
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin database migration %d: %w", migration.Version, err)
		}
		if _, err = tx.Exec(ctx, migration.SQL); err == nil {
			_, err = tx.Exec(ctx,
				`INSERT INTO codex_schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
				migration.Version, migration.Name, migration.Checksum,
			)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply database migration %d (%s): %w", migration.Version, migration.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit database migration %d (%s): %w", migration.Version, migration.Name, err)
		}
	}
	return nil
}

func validateMigrations() error {
	previous := 0
	for _, migration := range schemaMigrations {
		if migration.Version != previous+1 || migration.Name == "" || migration.SQL == "" {
			return fmt.Errorf("invalid database migration sequence at version %d", migration.Version)
		}
		digest := sha256.Sum256([]byte(migration.SQL))
		if hex.EncodeToString(digest[:]) != migration.Checksum {
			return fmt.Errorf("compiled database migration %d checksum mismatch", migration.Version)
		}
		previous = migration.Version
	}
	return nil
}
