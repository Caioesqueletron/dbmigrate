package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"time"
)

// lockKey is a fixed, application-wide advisory lock id. Every dbmigrate
// process trying to run migrations against the same database contends
// for this one key, which is exactly what we want: "up"/"down" must be
// serialized cluster-wide (e.g. across every replica of a service
// starting up at once and each trying to run migrations on boot).
//
// pg_advisory_lock takes a bigint; we derive a stable one from a string
// so the key is readable in code instead of a magic number.
var lockKey = int64(mustHash("dbmigrate:schema-lock"))

func mustHash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// WithAdvisoryLock is the crux of "implementing advisory locks
// correctly": Postgres session-level advisory locks
// (pg_advisory_lock/pg_advisory_unlock) are tied to the *database
// session*, not to a logical "connection" in application code. If you
// call pg_advisory_lock through database/sql's pooled *sql.DB directly,
// the pool is free to hand the *next* query a completely different
// physical connection — so your "unlock" call can silently run on a
// different session than the one that holds the lock, or worse, the
// lock leaks until that session's connection is eventually closed by
// the pool.
//
// The fix is to pin one physical connection for the entire critical
// section via db.Conn(ctx), acquire the lock on it, run all migration
// work through that same *sql.Conn, then explicitly unlock and release
// it back to the pool — all in a defer chain that runs even on panic or
// early return.
func WithAdvisoryLock(ctx context.Context, db *sql.DB, timeout time.Duration, fn func(conn *sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring dedicated connection: %w", err)
	}
	defer conn.Close() // returns the physical connection to the pool

	if err := acquireLock(ctx, conn, timeout); err != nil {
		return err
	}
	defer func() {
		// Best-effort unlock on the SAME connection that acquired it.
		// If this fails (e.g. connection died), Postgres releases
		// session-level advisory locks automatically when the session
		// ends, so we don't leak the lock forever — but we still try
		// the graceful path first.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	return fn(conn)
}

// acquireLock polls pg_try_advisory_lock instead of blocking on
// pg_advisory_lock directly, so a caller-provided timeout/context can
// actually interrupt a long wait (a plain pg_advisory_lock call blocks
// server-side and isn't responsive to client-side context cancellation
// once the driver has sent the query).
func acquireLock(ctx context.Context, conn *sql.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		var acquired bool
		row := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey)
		if err := row.Scan(&acquired); err != nil {
			return fmt.Errorf("acquiring advisory lock: %w", err)
		}
		if acquired {
			return nil
		}

		if timeout > 0 && time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for migration lock — another dbmigrate process is likely running", timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
