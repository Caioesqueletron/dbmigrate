package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Runner ties together the migrations directory, the database, and the
// advisory lock into the three CLI operations: Up, Down, Status.
type Runner struct {
	DB         *sql.DB
	Dir        string
	LockTimeout time.Duration
}

// StatusRow is a merged view of "known on disk" + "applied in DB" for
// one version, used by `dbmigrate status`.
type StatusRow struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt *time.Time
	Drifted   bool // file content changed since it was applied
}

// Status doesn't need the advisory lock — it's read-only — so it queries
// through the normal pooled *sql.DB rather than pinning a connection.
func (r *Runner) Status(ctx context.Context) ([]StatusRow, error) {
	local, err := LoadDir(r.Dir)
	if err != nil {
		return nil, err
	}
	applied, err := fetchApplied(ctx, r.DB)
	if err != nil {
		return nil, err
	}
	appliedByVersion := make(map[int64]AppliedMigration, len(applied))
	for _, a := range applied {
		appliedByVersion[a.Version] = a
	}

	rows := make([]StatusRow, 0, len(local))
	for _, m := range local {
		row := StatusRow{Version: m.Version, Name: m.Name}
		if a, ok := appliedByVersion[m.Version]; ok {
			row.Applied = true
			at := a.AppliedAt
			row.AppliedAt = &at
			row.Drifted = a.Checksum != m.Checksum
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Up applies all pending migrations (or at most `limit` of them, if
// limit > 0), each inside its own transaction, holding the cluster-wide
// advisory lock for the whole operation.
func (r *Runner) Up(ctx context.Context, limit int) ([]Migration, error) {
	var applied []Migration
	err := WithAdvisoryLock(ctx, r.DB, r.LockTimeout, func(conn *sql.Conn) error {
		local, err := LoadDir(r.Dir)
		if err != nil {
			return err
		}
		done, err := fetchApplied(ctx, conn)
		if err != nil {
			return err
		}
		doneByVersion := make(map[int64]AppliedMigration, len(done))
		for _, a := range done {
			doneByVersion[a.Version] = a
		}

		for _, m := range local {
			if a, ok := doneByVersion[m.Version]; ok {
				// Idempotent: already applied. Verify it hasn't drifted
				// so we fail loudly instead of silently skipping a
				// migration file that was edited after being applied.
				if a.Checksum != m.Checksum {
					return fmt.Errorf("migration %d_%s has changed since it was applied (checksum mismatch) — this is not safe to re-run", m.Version, m.Name)
				}
				continue
			}
			if limit > 0 && len(applied) >= limit {
				break
			}
			if err := runInTx(ctx, conn, m.UpSQL, m.Version, m.Name, m.Checksum, true); err != nil {
				return fmt.Errorf("applying migration %d_%s: %w", m.Version, m.Name, err)
			}
			applied = append(applied, m)
		}
		return nil
	})
	return applied, err
}

// Down rolls back the most recently applied `n` migrations (default 1),
// most-recent-first, each inside its own transaction.
func (r *Runner) Down(ctx context.Context, n int) ([]Migration, error) {
	if n <= 0 {
		n = 1
	}
	var rolledBack []Migration
	err := WithAdvisoryLock(ctx, r.DB, r.LockTimeout, func(conn *sql.Conn) error {
		local, err := LoadDir(r.Dir)
		if err != nil {
			return err
		}
		byVersion := make(map[int64]Migration, len(local))
		for _, m := range local {
			byVersion[m.Version] = m
		}

		done, err := fetchApplied(ctx, conn)
		if err != nil {
			return err
		}
		// fetchApplied returns ascending order; walk backwards for "most
		// recent first" rollback semantics.
		for i := len(done) - 1; i >= 0 && len(rolledBack) < n; i-- {
			a := done[i]
			m, ok := byVersion[a.Version]
			if !ok {
				return fmt.Errorf("cannot roll back version %d: migration file no longer exists on disk", a.Version)
			}
			if err := runInTx(ctx, conn, m.DownSQL, m.Version, m.Name, m.Checksum, false); err != nil {
				return fmt.Errorf("rolling back migration %d_%s: %w", m.Version, m.Name, err)
			}
			rolledBack = append(rolledBack, m)
		}
		return nil
	})
	return rolledBack, err
}

// runInTx executes one migration direction (up or down SQL) plus the
// corresponding schema_migrations bookkeeping row, atomically: either
// both the schema change and the bookkeeping update land, or neither
// does. This is what makes an interrupted `dbmigrate up` safe to simply
// re-run — there's never a state where the DB schema changed but
// schema_migrations doesn't know about it, or vice versa.
func runInTx(ctx context.Context, conn *sql.Conn, sqlText string, version int64, name, checksum string, isUp bool) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op if Commit succeeds

	if _, err := tx.ExecContext(ctx, sqlText); err != nil {
		return err
	}

	if isUp {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			version, name, checksum)
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
	}
	if err != nil {
		return err
	}

	return tx.Commit()
}
