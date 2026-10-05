package eventlog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// upSuffix is the marker for a forward migration. The matching *.down.sql files
// exist for a human to run by hand; the engine never rolls a schema backwards on
// its own, because doing so would delete durable history.
const upSuffix = ".up.sql"

// Migrate applies every *.up.sql file in dir, in lexical filename order.
//
// There is no migration library and no schema_migrations bookkeeping table on
// purpose. Every statement in the schema is written with IF NOT EXISTS, so
// applying the same file twice is a no-op, and the ordering rule is one a human
// can verify by running `ls`. A dependency whose only job is to remember which
// files it already ran would add a failure mode without removing one.
//
// Each file runs as its own Exec. pgx sends a multi-statement string through the
// simple protocol, which PostgreSQL wraps in an implicit transaction, so one
// file either applies completely or not at all.
// migrateLockKey is an arbitrary but fixed identifier for the advisory lock
// that serialises schema application across nodes.
const migrateLockKey int64 = 0x6475726578656301 // "durexec" + 1

// Migrate applies every *.up.sql in dir, in lexical order, under a session
// advisory lock.
//
// The lock is not defensive programming — without it a three-node cluster fails
// to start. Every node migrates on boot, and they boot together, so they race.
// "CREATE EXTENSION IF NOT EXISTS vector" is the one that breaks first: the
// existence check and the insert are not atomic against a concurrent creator,
// so two nodes both see "not there", both insert, and the loser dies with a
// unique violation on pg_extension_name_index. CREATE TABLE IF NOT EXISTS can
// fail the same way.
//
// pg_advisory_lock makes the whole sequence single-file: the first node
// migrates while the others wait, then each of them finds the work already done
// and its IF NOT EXISTS clauses become genuine no-ops.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("eventlog: acquire migration connection: %w", err)
	}
	defer conn.Release()

	// The lock is held on this one connection, so it must be taken and released
	// on the same connection — hence the explicit Acquire rather than the pool.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("eventlog: take migration lock: %w", err)
	}
	defer func() {
		// Best effort: a failure here leaks the lock only until the connection
		// closes, which the pool does on release of a broken connection.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("eventlog: read migrations dir %q: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}

	for _, name := range UpMigrations(names) {
		path := filepath.Join(dir, name)
		sql, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("eventlog: read migration %q: %w", path, err)
		}
		if strings.TrimSpace(string(sql)) == "" {
			continue
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("eventlog: apply migration %q: %w", name, err)
		}
	}
	return nil
}

// UpMigrations selects the forward migrations from a list of filenames and puts
// them in the order they must be applied.
//
// It is exported and pure so the ordering rule — the one thing here that can be
// silently wrong — can be tested without a directory or a database. Plain
// lexical sort is the contract: "0002" sorts after "0001" because the names are
// zero-padded, which is why the numbering convention has to be kept.
func UpMigrations(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.HasSuffix(n, upSuffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
