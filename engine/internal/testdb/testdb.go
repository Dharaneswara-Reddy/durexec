// Package testdb is the one place integration tests get their PostgreSQL.
//
// Three rules, each added because a real defect passed without them:
//
//  1. A test that REQUIRES a database must fail when there is none. Setting
//     DUREXEC_REQUIRE_DB=1 turns "no DSN" from a silent skip into a failure.
//     CI and `make test-integration` set it. Without it the test still skips,
//     but says exactly why.
//  2. Every test creates the schema it needs itself. Nothing relies on another
//     package having run first (a fresh database used to fail 14 tests with
//     "relation does not exist" when packages ran in parallel).
//  3. Tests never run against the demonstration cluster. The leader is on
//     port 5433 and the standby on 5434; a DSN pointing there is refused
//     unless DUREXEC_ALLOW_DEMO_DB=1 (this is how 48 junk rows once ended up
//     at shard_id 900-911 of the live demo database).
//
// This package must not import any other durexec package, so every package's
// own tests can use it without an import cycle. That is why it applies the
// migration files itself instead of calling eventlog.Migrate.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	EnvDSN       = "DUREXEC_TEST_DSN"
	EnvRequire   = "DUREXEC_REQUIRE_DB"
	EnvAllowDemo = "DUREXEC_ALLOW_DEMO_DB"

	// MUST equal eventlog.migrateLockKey. eventlog's own tests call
	// eventlog.Migrate while other packages call this one; with different keys
	// the two migrators race on CREATE EXTENSION / CREATE TABLE IF NOT EXISTS
	// and one dies with a unique violation (seen on a fresh database).
	migrateLockKey int64 = 0x6475726578656301
)

// demoPorts are the host ports of the demonstration cluster's databases.
var demoPorts = map[string]bool{"5433": true, "5434": true}

// DSN returns the test DSN, or skips (or fails, when DUREXEC_REQUIRE_DB is set).
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		if os.Getenv(EnvRequire) != "" {
			t.Fatalf("%s is not set but %s is: this test needs PostgreSQL and must not be skipped", EnvDSN, EnvRequire)
		}
		t.Skipf("SKIPPED (optional): %s is not set, so this PostgreSQL test did not run. "+
			"Run `make test-integration` to run it against a throwaway database.", EnvDSN)
	}
	if err := refuseDemo(dsn); err != nil {
		t.Fatalf("%v", err)
	}
	return dsn
}

func refuseDemo(dsn string) error {
	if os.Getenv(EnvAllowDemo) != "" {
		return nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil // not a URL; let the driver complain
	}
	if demoPorts[u.Port()] && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
		return fmt.Errorf("testdb: %s points at port %s, the demonstration cluster's database. "+
			"Tests write and delete rows; use a throwaway database (make test-integration) "+
			"or set %s=1 if you really mean it", EnvDSN, u.Port(), EnvAllowDemo)
	}
	return nil
}

// Pool connects, applies every *.up.sql migration (idempotent, serialised by an
// advisory lock so parallel packages do not race), and returns the pool.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := DSN(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("testdb: %v", err)
	}
	return pool
}

// Migrate applies engine/migrations/*.up.sql in name order.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no *.up.sql files in %s", dir)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()
	for _, n := range names {
		sqlText, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return fmt.Errorf("read %s: %w", n, err)
		}
		if _, err := conn.Exec(ctx, string(sqlText)); err != nil {
			return fmt.Errorf("apply %s: %w", n, err)
		}
	}
	return nil
}
