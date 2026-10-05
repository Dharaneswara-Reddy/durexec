// Package lease decides which node owns which shard, and hands out the fencing
// token that proves it.
//
// The idea is smaller than it sounds. There is one row per shard holding four
// columns: who owns it, a number that only ever goes up, when the claim expires.
// A node takes a shard by updating that row, and it keeps the shard by updating
// the expiry every few seconds. If the node dies, it stops updating, the expiry
// passes, and anyone else may take the row.
//
// The number that only goes up is the fencing token, and it is what makes this
// safe rather than merely convenient. Every takeover increments it. A node that
// froze — garbage collection, a network partition, a paused virtual machine —
// can wake up still believing it owns the shard, but it is holding an old
// number. Every write it attempts re-reads the row inside the same transaction
// and compares, sees a higher number, and is refused. The stale node cannot
// corrupt anything, so we never have to guarantee that it noticed it was
// replaced. That property is why this package can replace a separate
// coordination service such as ZooKeeper.
package lease

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ErrLeaseHeld means the shard is currently owned by a different node whose
// lease has not expired. It is a normal outcome, not a fault: a node scanning
// for work to pick up will see it constantly. It is a distinct error from
// core.ErrLostOwnership, which is the far more serious "we were holding this and
// have now been fenced out".
var ErrLeaseHeld = errors.New("lease: shard is held by another live owner")

// ErrLeaseNotFound means there is no row for the shard. Call EnsureRows.
var ErrLeaseNotFound = errors.New("lease: no row for shard")

// DB is the slice of pgx this package uses, so a Store can sit on a pool, a
// single connection or an open transaction.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store is the PostgreSQL implementation of core.LeaseStore.
type Store struct{ db DB }

var _ core.LeaseStore = (*Store)(nil)

// New builds a Store over a pool.
func New(pool *pgxpool.Pool) *Store { return &Store{db: pool} }

// NewWithDB builds a Store over any query runner.
func NewWithDB(db DB) *Store { return &Store{db: db} }

// interval renders a Go duration as a PostgreSQL interval literal in
// milliseconds, so there is no unit or locale ambiguity in what the server
// parses.
func interval(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return strconv.FormatInt(d.Milliseconds(), 10) + " milliseconds"
}

// ttlInterval turns the interface's ttlSeconds into that literal.
func ttlInterval(ttlSeconds int) string {
	if ttlSeconds <= 0 {
		ttlSeconds = 1
	}
	return interval(time.Duration(ttlSeconds) * time.Second)
}

// ---- Acquire --------------------------------------------------------------

// acquireSQL is the whole election, in one statement.
//
// The WHERE clause names the only three situations in which taking the shard is
// legitimate: we already own it (so this is a re-acquire after a restart), it
// has no owner at all, or the current owner's claim has expired. Because this is
// a single UPDATE, PostgreSQL evaluates the condition and writes the new owner
// under one row lock — two nodes racing here cannot both succeed, because the
// second one re-checks the condition against the row the first one already
// changed and finds it no longer true.
//
// The fencing token is incremented on every acquisition, including a re-acquire
// by the same node. That is intentional: a node that restarted has no idea what
// its previous incarnation left in flight, and bumping the token invalidates
// anything that incarnation might still be trying to write.
const acquireSQL = `
UPDATE shard_leases
   SET owner_node = $2, fencing_token = fencing_token + 1, expires_at = now() + $3::interval
 WHERE shard_id = $1
   AND (owner_node = $2 OR owner_node IS NULL OR expires_at < now())
RETURNING fencing_token`

// Acquire takes an unowned or expired lease and bumps the fencing token.
// Zero rows updated means someone else holds a live lease: that is ErrLeaseHeld,
// which the caller is expected to shrug off and retry later.
func (s *Store) Acquire(ctx context.Context, shard core.ShardID, owner string, ttlSeconds int) (core.FencingToken, error) {
	var tok int64
	err := s.db.QueryRow(ctx, acquireSQL, int32(shard), owner, ttlInterval(ttlSeconds)).Scan(&tok)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("lease: acquire shard %d as %q: %w", shard, owner, ErrLeaseHeld)
	}
	if err != nil {
		return 0, fmt.Errorf("lease: acquire shard %d: %w", shard, err)
	}
	return core.FencingToken(tok), nil
}

// ---- Renew ----------------------------------------------------------------

// renewSQL extends a lease we believe we already hold. The token is part of the
// WHERE clause, not just the owner name, so a node that was fenced out and
// re-acquired the shard under a new token cannot have its old goroutine keep the
// old claim alive.
const renewSQL = `
UPDATE shard_leases SET expires_at = now() + $4::interval
 WHERE shard_id = $1 AND owner_node = $2 AND fencing_token = $3`

// Renew extends a lease we already hold.
//
// Zero rows means we were fenced out — the owner name or the token no longer
// matches, so somebody else has taken the shard. This returns
// core.ErrLostOwnership, and the caller must stop all work on that shard
// immediately: any transaction it is still holding will be refused at the
// fencing check anyway, but stopping now avoids doing work that cannot land.
func (s *Store) Renew(ctx context.Context, shard core.ShardID, owner string, tok core.FencingToken, ttlSeconds int) error {
	tag, err := s.db.Exec(ctx, renewSQL, int32(shard), owner, int64(tok), ttlInterval(ttlSeconds))
	if err != nil {
		return fmt.Errorf("lease: renew shard %d: %w", shard, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("lease: renew shard %d as %q token %d: %w", shard, owner, tok, core.ErrLostOwnership)
	}
	return nil
}

// ---- Release --------------------------------------------------------------

// releaseSQL gives the shard up. The expiry is set into the past rather than the
// row being deleted, because the row is also where the fencing token lives, and
// the token must survive every ownership change to keep increasing.
//
// The owner is set to NULL so the next node to look does not have to wait for a
// clock comparison to come out in its favour.
const releaseSQL = `
UPDATE shard_leases SET owner_node = NULL, expires_at = now()
 WHERE shard_id = $1 AND owner_node = $2 AND fencing_token = $3`

// Release voluntarily gives up a shard on graceful shutdown. Releasing a lease
// we no longer hold is not an error — the outcome we wanted is already true.
func (s *Store) Release(ctx context.Context, shard core.ShardID, owner string, tok core.FencingToken) error {
	if _, err := s.db.Exec(ctx, releaseSQL, int32(shard), owner, int64(tok)); err != nil {
		return fmt.Errorf("lease: release shard %d: %w", shard, err)
	}
	return nil
}

// ---- Get / List -----------------------------------------------------------

const getSQL = `
SELECT shard_id, COALESCE(owner_node, ''), fencing_token, expires_at
  FROM shard_leases WHERE shard_id = $1`

// Get reads current ownership, used by the gateway to answer NotOwner. An empty
// Owner means the shard is unclaimed.
func (s *Store) Get(ctx context.Context, shard core.ShardID) (core.Lease, error) {
	var (
		l     core.Lease
		id    int32
		token int64
	)
	err := s.db.QueryRow(ctx, getSQL, int32(shard)).Scan(&id, &l.Owner, &token, &l.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Lease{}, fmt.Errorf("lease: get shard %d: %w", shard, ErrLeaseNotFound)
	}
	if err != nil {
		return core.Lease{}, fmt.Errorf("lease: get shard %d: %w", shard, err)
	}
	l.ShardID = core.ShardID(id)
	l.Token = core.FencingToken(token)
	return l, nil
}

const listSQL = `
SELECT shard_id, COALESCE(owner_node, ''), fencing_token, expires_at
  FROM shard_leases ORDER BY shard_id`

// List returns every shard's ownership, for the /v1/shards admin view.
func (s *Store) List(ctx context.Context) ([]core.Lease, error) {
	rows, err := s.db.Query(ctx, listSQL)
	if err != nil {
		return nil, fmt.Errorf("lease: list: %w", err)
	}
	defer rows.Close()

	var out []core.Lease
	for rows.Next() {
		var (
			l     core.Lease
			id    int32
			token int64
		)
		if err := rows.Scan(&id, &l.Owner, &token, &l.ExpiresAt); err != nil {
			return nil, fmt.Errorf("lease: scan lease: %w", err)
		}
		l.ShardID = core.ShardID(id)
		l.Token = core.FencingToken(token)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lease: list: %w", err)
	}
	return out, nil
}

// ---- EnsureRows -----------------------------------------------------------

// ensureRowSQL creates a shard's lease row if it is missing. generate_series
// does the whole range in one statement, and ON CONFLICT DO NOTHING makes it
// idempotent, so every node in the fleet can run this at startup without
// coordinating and without ever resetting a token that is already in use.
const ensureRowsSQL = `
INSERT INTO shard_leases (shard_id, owner_node, fencing_token, expires_at)
SELECT g, NULL, 0, now() FROM generate_series(0, $1::int - 1) AS g
ON CONFLICT (shard_id) DO NOTHING`

// EnsureRows inserts lease rows for shards 0..n-1 if they do not already exist.
// It is safe to call on every startup.
func (s *Store) EnsureRows(ctx context.Context, n int32) error {
	if n <= 0 {
		return nil
	}
	if _, err := s.db.Exec(ctx, ensureRowsSQL, n); err != nil {
		return fmt.Errorf("lease: ensure %d lease rows: %w", n, err)
	}
	return nil
}

// ---- VerifyToken ----------------------------------------------------------

// verifyTokenSQL locks the lease row for the rest of the caller's transaction.
const verifyTokenSQL = `SELECT fencing_token FROM shard_leases WHERE shard_id = $1 FOR UPDATE`

// VerifyToken is the safety primitive the commit path is built on.
//
// It must be called with a transaction that is about to write, never with a
// pool. FOR UPDATE holds a lock on the lease row until that transaction ends, so
// no other transaction can change ownership between this check and those writes.
// Without the shared transaction the check would be worthless: it would only
// tell you who owned the shard a moment ago.
//
// A mismatch, or a missing row, is core.ErrLostOwnership. The caller must roll
// back and stop.
func VerifyToken(ctx context.Context, tx pgx.Tx, shard core.ShardID, tok core.FencingToken) error {
	var stored int64
	err := tx.QueryRow(ctx, verifyTokenSQL, int32(shard)).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("lease: shard %d has no lease row: %w", shard, core.ErrLostOwnership)
	case err != nil:
		return fmt.Errorf("lease: verify token for shard %d: %w", shard, err)
	case core.FencingToken(stored) != tok:
		return fmt.Errorf("lease: shard %d is at token %d, caller holds %d: %w",
			shard, stored, tok, core.ErrLostOwnership)
	}
	return nil
}

// VerifyToken is also available as a method so callers holding a Store do not
// have to remember which package the free function lives in.
func (s *Store) VerifyToken(ctx context.Context, tx pgx.Tx, shard core.ShardID, tok core.FencingToken) error {
	return VerifyToken(ctx, tx, shard, tok)
}
