package lease

import (
	"strings"
	"testing"
	"time"
)

func TestIntervalLiteral(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0 milliseconds"},
		{-time.Second, "0 milliseconds"},
		{10 * time.Second, "10000 milliseconds"},
		{90 * time.Second, "90000 milliseconds"},
	}
	for _, tc := range tests {
		if got := interval(tc.in); got != tc.want {
			t.Errorf("interval(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTTLInterval(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		// A zero or negative TTL would mean a lease that has already expired, so
		// it is floored at one second rather than sent to the server as written.
		{0, "1000 milliseconds"},
		{-5, "1000 milliseconds"},
		{1, "1000 milliseconds"},
		{10, "10000 milliseconds"},
	}
	for _, tc := range tests {
		if got := ttlInterval(tc.in); got != tc.want {
			t.Errorf("ttlInterval(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The four statements below carry the whole safety argument. Each assertion
// names the failure that would follow if the clause were dropped, so a future
// edit that breaks one fails here with an explanation rather than in production.
func TestSQLKeepsItsGuarantees(t *testing.T) {
	tests := []struct {
		name   string
		sql    string
		needle string
		why    string
	}{
		{"acquire bumps the token", acquireSQL, "fencing_token = fencing_token + 1",
			"without the increment a stale node's token stays valid and it can keep writing"},
		{"acquire only takes a free shard", acquireSQL, "owner_node IS NULL OR expires_at < now()",
			"without this a node could take a shard someone else is actively holding"},
		{"acquire returns the new token", acquireSQL, "RETURNING fencing_token",
			"the caller has nothing to fence its writes with otherwise"},
		{"renew matches on the token", renewSQL, "fencing_token = $3",
			"matching on the owner alone would let a fenced-out node keep its old claim alive"},
		{"verify locks the row", verifyTokenSQL, "FOR UPDATE",
			"without the lock, ownership can change between the check and the writes it is supposed to protect"},
		{"ensure rows is idempotent", ensureRowsSQL, "ON CONFLICT (shard_id) DO NOTHING",
			"otherwise a second node starting up would reset tokens that are already in use"},
		{"release keeps the row", releaseSQL, "UPDATE shard_leases",
			"deleting the row would discard the fencing token, which must only ever increase"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.sql, tc.needle) {
				t.Fatalf("statement no longer contains %q: %s", tc.needle, tc.why)
			}
		})
	}
}
