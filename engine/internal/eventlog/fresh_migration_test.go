package eventlog_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestFreshDatabaseMigration exercises Migrate() against a brand-new database,
// then again, then from three pools at once.
//
// These are the three things the EC2 deployment depends on and that no
// previous Compose run proves: the existing cluster's database has been
// migrated for months, so every run since has exercised the no-op path only.
// The concurrent case is what the advisory lock in migrate.go exists for --
// without it, three nodes racing CREATE EXTENSION IF NOT EXISTS vector hit a
// unique violation on pg_extension_name_index.
//
// Run it against a disposable database:
//
//	docker run -d --name migtest -e POSTGRES_USER=durexec \
//	  -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=durexec -p 55433:5432 \
//	  pgvector/pgvector:pg16
//	MIGTEST_DSN="postgres://durexec:pw@127.0.0.1:55433/durexec?sslmode=disable" \
//	MIGTEST_DIR="$PWD/migrations" go test ./internal/eventlog -run Fresh -v
func TestFreshDatabaseMigration(t *testing.T) {
	dsn := os.Getenv("MIGTEST_DSN")
	if dsn == "" {
		t.Skip("MIGTEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := eventlog.Migrate(ctx, pool, os.Getenv("MIGTEST_DIR")); err != nil {
		t.Fatalf("first migrate on an EMPTY database: %v", err)
	}
	if err := eventlog.Migrate(ctx, pool, os.Getenv("MIGTEST_DIR")); err != nil {
		t.Fatalf("second migrate (rerun safety): %v", err)
	}

	// Three concurrent migrators is the case the advisory lock exists for:
	// without it, racing CREATE EXTENSION hits a unique violation on
	// pg_extension_name_index.
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			p, e := pgxpool.New(ctx, dsn)
			if e != nil {
				errs <- e
				return
			}
			defer p.Close()
			errs <- eventlog.Migrate(ctx, p, os.Getenv("MIGTEST_DIR"))
		}()
	}
	for i := 0; i < 3; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("concurrent migrate %d: %v", i, e)
		}
	}
}
