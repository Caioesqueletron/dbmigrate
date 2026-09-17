package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // Postgres driver, registered via side-effect import
)

// AppliedMigration is one row of the schema_migrations bookkeeping table.
type AppliedMigration struct {
	Version   int64
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// Open connects to Postgres and ensures the schema_migrations table
// exists. It returns a *sql.DB, which is a *pool*, not a single
// connection — important context for lock.go, which must pin a single
// physical connection for the duration of an advisory lock.
func Open(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return db, nil
}

func ensureSchemaTable(ctx context.Context, conn Queryer) error {
	_, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     BIGINT PRIMARY KEY,
			name        TEXT NOT NULL,
			checksum    TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}
	return nil
}

// Queryer is the subset of *sql.DB / *sql.Conn / *sql.Tx we need. Using
// an interface instead of a concrete type lets the same code run
// against a pooled *sql.DB (for read-only Status calls, where holding a
// dedicated connection would be wasteful) and against the single pinned
// *sql.Conn used while an advisory lock is held (Up/Down).
type Queryer interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

func fetchApplied(ctx context.Context, q Queryer) ([]AppliedMigration, error) {
	if err := ensureSchemaTable(ctx, q); err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx, `SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("querying schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
