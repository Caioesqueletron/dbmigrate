# dbmigrate

A small, educational schema migration tool for PostgreSQL. The headline
feature isn't the CLI surface (that part is deliberately minimal) — it's
implementing **session-level advisory locks correctly** against Go's
pooled `database/sql`, which is the detail most hand-rolled migration
tools get subtly wrong.

## Quick start

```bash
go mod tidy   # fetches github.com/lib/pq
go build -o dbmigrate ./cmd/dbmigrate

export DATABASE_URL="postgres://user:pass@localhost:5432/mydb?sslmode=disable"

dbmigrate create add_users_table
dbmigrate up
dbmigrate down 1
dbmigrate status
```

```
$ dbmigrate status
VERSION              NAME                           APPLIED AT
20260101120000     ✓  add_users_table               2026-01-01T12:00:03Z
20260102093000        add_users_last_login           -
```

## Features

| Feature                    | Notes |
|------------------------------|-------|
| Versioning                  | Timestamp-prefixed filenames (`YYYYMMDDHHMMSS_name.{up,down}.sql`) — sortable, collision-free across branches |
| Distributed advisory lock    | `pg_advisory_lock` pinned to a single `*sql.Conn`, see below |
| Rollback                    | `dbmigrate down [n]`, most-recent-first |
| Checksums                   | SHA-256 of each migration's SQL, stored in `schema_migrations`; `status`/`up` detect drift if a file is edited after being applied |
| Transactions                | Each migration's SQL + bookkeeping insert/delete run in one transaction |
| CLI                         | `create`, `up`, `down`, `status` subcommands |
| PostgreSQL                  | via `github.com/lib/pq` |
| Idempotent `up`              | already-applied migrations (matching checksum) are silently skipped |

## Architecture

```
cmd/dbmigrate/main.go        CLI: subcommand dispatch, env vars, output
internal/migrate/
  migration.go               Migration type + directory scanning/pairing
  create.go                  Scaffolds new .up.sql/.down.sql pairs
  db.go                      Connection + schema_migrations bookkeeping
  lock.go                    Advisory locking — the differentiator, see below
  runner.go                  Up/Down/Status orchestration, transactions
```

## Advisory locks, done correctly

This is the part worth reading carefully.

Postgres session-level advisory locks (`pg_advisory_lock` /
`pg_advisory_unlock`) are tied to the **database session** — i.e. to one
specific TCP connection's backend process — not to any concept in your
application code. `pg_advisory_unlock` only releases a lock if it's
called **on the exact same session** that acquired it.

`database/sql.DB` is a *pool*. If you call `db.Exec("SELECT
pg_advisory_lock($1)", key)` and later `db.Exec("SELECT
pg_advisory_unlock($1)", key)`, nothing in the standard library
guarantees those two calls land on the same physical connection — the
pool is explicitly free to check a connection back in between calls and
hand a different one to the next query. Two very real bugs follow from
getting this wrong:

1. **The unlock silently does nothing** (wrong session), so the lock
   stays held until that first connection is eventually closed by the
   pool — which might be minutes or hours later — blocking every other
   `dbmigrate` process (e.g. every replica of a service trying to
   migrate on startup) for that whole window.
2. Under concurrent load, two goroutines can each *believe* they hold
   the lock if the pool reuses a connection in a way that interleaves
   badly with retry logic.

`internal/migrate/lock.go` avoids this with `WithAdvisoryLock`:

```go
conn, _ := db.Conn(ctx)      // pin ONE physical connection out of the pool
defer conn.Close()           // returns it to the pool when we're done

acquireLock(ctx, conn, timeout)   // pg_try_advisory_lock on THIS conn
defer conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", lockKey) // same conn

return fn(conn)              // ALL migration work runs through this same conn
```

`db.Conn(ctx)` is the standard library's escape hatch for exactly this
situation: it checks out a single physical connection and hands you a
`*sql.Conn` that stays pinned to it until you call `Close()`. Every
query inside the critical section — including every migration's
transaction — is run through that same `*sql.Conn`, so lock and unlock
are guaranteed to be the same session.

Two more details that matter in practice:

- **`pg_try_advisory_lock`, polled, instead of blocking `pg_advisory_lock`.**
  A blocking advisory lock request, once sent to the server, isn't
  interruptible by client-side context cancellation — Postgres just
  parks the backend. `acquireLock` instead polls the non-blocking
  `pg_try_advisory_lock` on a short ticker, so a `-timeout` or a
  cancelled `context.Context` actually takes effect instead of hanging
  the whole CLI indefinitely if another process is mid-migration.
- **Failure doesn't leak the lock forever.** If the process crashes
  mid-migration, Postgres automatically releases session-level advisory
  locks when that session's connection closes — so a crashed
  `dbmigrate` doesn't permanently wedge the next run. The `defer`-based
  unlock is the *fast* path; connection death is the *fallback* safety
  net.

## Idempotency & drift detection

Every applied migration's checksum (SHA-256 of its up+down SQL) is
stored in `schema_migrations`. Running `up` again after it already
succeeded is a no-op — migrations are matched by version and, if already
applied with a matching checksum, skipped. But if someone edits an
already-applied migration file, `up` (and `status`) will surface a loud
checksum-mismatch error instead of silently re-running or ignoring the
drift, since re-applying edited "history" is rarely what you want and
almost always worth a human looking at it.

## What this demonstrates

- Correct use of `db.Conn(ctx)` to pin a physical connection for session-scoped Postgres features (advisory locks)
- Non-blocking lock acquisition (`pg_try_advisory_lock`) polled against a `context.Context`, instead of a blocking call that can't be cancelled
- Transactional migration application (schema change + bookkeeping row, atomically)
- Checksum-based drift detection instead of blind re-execution
- A dependency-light CLI (only `lib/pq` beyond the standard library)

## Limitations (by design, for an educational project)

- No down-migration dry-run/plan preview before executing.
- No support for multiple migration "branches" merging out of order —
  migrations are expected to be applied linearly, which is the common
  case but not universal.
- Only PostgreSQL is supported (advisory locks are a Postgres-specific
  feature; a MySQL/SQLite backend would need a different locking
  strategy, e.g. `GET_LOCK()` for MySQL).
