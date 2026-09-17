// Command dbmigrate is a small, educational schema migration tool for
// PostgreSQL. See README.md for architecture notes, especially on how
// advisory locking is implemented correctly across database/sql's
// connection pool.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/example/dbmigrate/internal/migrate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	dir := os.Getenv("DBMIGRATE_DIR")
	if dir == "" {
		dir = "migrations"
	}
	dsn := os.Getenv("DATABASE_URL")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "create":
		runCreate(dir, args)
	case "up":
		runUp(ctx, dsn, dir, args)
	case "down":
		runDown(ctx, dsn, dir, args)
	case "status":
		runStatus(ctx, dsn, dir, args)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dbmigrate — a small PostgreSQL migration tool

Usage:
  dbmigrate create <name>     Scaffold a new migration (up/down SQL pair)
  dbmigrate up [n]             Apply all pending migrations (or at most n)
  dbmigrate down [n]           Roll back the last migration (or the last n)
  dbmigrate status             Show applied / pending migrations

Environment:
  DATABASE_URL   PostgreSQL connection string (required for up/down/status)
  DBMIGRATE_DIR  Migrations directory (default: ./migrations)
`)
}

func runCreate(dir string, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: dbmigrate create <name>"))
	}
	up, down, err := migrate.Create(dir, args[0])
	if err != nil {
		fatal(err)
	}
	fmt.Println("created:")
	fmt.Println(" ", up)
	fmt.Println(" ", down)
}

func runUp(ctx context.Context, dsn, dir string, args []string) {
	requireDSN(dsn)
	limit := 0
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil {
			fatal(fmt.Errorf("invalid count %q: %w", args[0], err))
		}
		limit = n
	}

	r := newRunner(dsn, dir)
	defer r.DB.Close()

	applied, err := r.Up(ctx, limit)
	if err != nil {
		fatal(err)
	}
	if len(applied) == 0 {
		fmt.Println("nothing to do (already up to date)")
		return
	}
	for _, m := range applied {
		fmt.Printf("applied: %d_%s\n", m.Version, m.Name)
	}
}

func runDown(ctx context.Context, dsn, dir string, args []string) {
	requireDSN(dsn)
	n := 1
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil {
			fatal(fmt.Errorf("invalid count %q: %w", args[0], err))
		}
		n = v
	}

	r := newRunner(dsn, dir)
	defer r.DB.Close()

	rolledBack, err := r.Down(ctx, n)
	if err != nil {
		fatal(err)
	}
	if len(rolledBack) == 0 {
		fmt.Println("nothing to roll back")
		return
	}
	for _, m := range rolledBack {
		fmt.Printf("rolled back: %d_%s\n", m.Version, m.Name)
	}
}

func runStatus(ctx context.Context, dsn, dir string, _ []string) {
	requireDSN(dsn)
	r := newRunner(dsn, dir)
	defer r.DB.Close()

	rows, err := r.Status(ctx)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("%-16s %-4s %-30s %s\n", "VERSION", "", "NAME", "APPLIED AT")
	for _, row := range rows {
		mark := " "
		if row.Applied {
			mark = "✓"
		}
		appliedAt := "-"
		if row.AppliedAt != nil {
			appliedAt = row.AppliedAt.Format(time.RFC3339)
		}
		drift := ""
		if row.Drifted {
			drift = "  (CHECKSUM MISMATCH — file changed after being applied)"
		}
		fmt.Printf("%-16d %-4s %-30s %s%s\n", row.Version, mark, row.Name, appliedAt, drift)
	}
}

func newRunner(dsn, dir string) *migrate.Runner {
	db, err := migrate.Open(dsn)
	if err != nil {
		fatal(err)
	}
	return &migrate.Runner{DB: db, Dir: dir, LockTimeout: 30 * time.Second}
}

func requireDSN(dsn string) {
	if dsn == "" {
		fatal(fmt.Errorf("DATABASE_URL environment variable is required, e.g.\n  export DATABASE_URL=\"postgres://user:pass@localhost:5432/mydb?sslmode=disable\""))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
