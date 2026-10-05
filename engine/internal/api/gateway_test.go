package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

var routingNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestDecide is the whole reason the routing rule is a pure function: every
// case can be enumerated here with no database, no clock and no HTTP.
func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		self      string
		lease     core.Lease
		wantLocal bool
		wantOwner string
	}{
		{
			name:      "we own it and the lease is live",
			self:      "node-a",
			lease:     core.Lease{ShardID: 3, Owner: "node-a", Token: 7, ExpiresAt: routingNow.Add(time.Minute)},
			wantLocal: true,
			wantOwner: "node-a",
		},
		{
			name:      "another node owns it",
			self:      "node-a",
			lease:     core.Lease{ShardID: 3, Owner: "node-c", Token: 7, ExpiresAt: routingNow.Add(time.Minute)},
			wantLocal: false,
			wantOwner: "node-c",
		},
		{
			// The dangerous case. Our name is still in the row, but the lease
			// has lapsed, so another node may take it at any moment. Serving
			// here is the split brain the fencing token exists to stop.
			name:      "our own lease has expired",
			self:      "node-a",
			lease:     core.Lease{ShardID: 3, Owner: "node-a", Token: 7, ExpiresAt: routingNow.Add(-time.Second)},
			wantLocal: false,
			wantOwner: "",
		},
		{
			name:      "expiry exactly now is expired",
			self:      "node-a",
			lease:     core.Lease{ShardID: 3, Owner: "node-a", ExpiresAt: routingNow},
			wantLocal: false,
			wantOwner: "",
		},
		{
			name:      "another node's expired lease reports no owner",
			self:      "node-a",
			lease:     core.Lease{ShardID: 3, Owner: "node-c", ExpiresAt: routingNow.Add(-time.Hour)},
			wantLocal: false,
			wantOwner: "",
		},
		{
			name:      "unowned shard",
			self:      "node-a",
			lease:     core.Lease{ShardID: 3, Owner: "", ExpiresAt: routingNow.Add(time.Minute)},
			wantLocal: false,
			wantOwner: "",
		},
		{
			// A node with no name must never match an unowned lease row, or an
			// unconfigured process would claim every shard in the cluster.
			name:      "an unnamed node never owns anything",
			self:      "",
			lease:     core.Lease{ShardID: 3, Owner: "", ExpiresAt: routingNow.Add(time.Minute)},
			wantLocal: false,
			wantOwner: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(3, tc.self, tc.lease, routingNow)
			if got.ShardID != 3 {
				t.Fatalf("ShardID = %d, want 3", got.ShardID)
			}
			if got.Local != tc.wantLocal {
				t.Fatalf("Local = %v, want %v", got.Local, tc.wantLocal)
			}
			if got.Owner != tc.wantOwner {
				t.Fatalf("Owner = %q, want %q", got.Owner, tc.wantOwner)
			}
		})
	}
}

// TestDecideIsPure: the same inputs must always give the same answer, since
// two nodes computing routing independently have to agree.
func TestDecideIsPure(t *testing.T) {
	lease := core.Lease{ShardID: 1, Owner: "node-a", ExpiresAt: routingNow.Add(time.Minute)}
	first := Decide(1, "node-a", lease, routingNow)
	for i := 0; i < 100; i++ {
		if Decide(1, "node-a", lease, routingNow) != first {
			t.Fatal("Decide returned different answers for identical inputs")
		}
	}
}

func TestGatewayResolveLocal(t *testing.T) {
	gw := &Gateway{
		Self:   "node-a",
		Shards: fixedShard(3),
		Leases: &fakeLeases{lease: core.Lease{ShardID: 3, Owner: "node-a", ExpiresAt: routingNow.Add(time.Minute)}},
		Clock:  fixedClock{routingNow},
	}
	rt, err := gw.Resolve(context.Background(), "trip-42")
	if err != nil {
		t.Fatal(err)
	}
	if !rt.Local || rt.ShardID != 3 {
		t.Fatalf("Resolve = %+v, want a local routing for shard 3", rt)
	}
}

func TestGatewayResolveNotOwner(t *testing.T) {
	gw := &Gateway{
		Self:   "node-a",
		Shards: fixedShard(3),
		Leases: &fakeLeases{lease: core.Lease{ShardID: 3, Owner: "node-c", ExpiresAt: routingNow.Add(time.Minute)}},
		Clock:  fixedClock{routingNow},
	}
	rt, err := gw.Resolve(context.Background(), "trip-42")

	var notOwner *core.NotOwnerError
	if !errors.As(err, &notOwner) {
		t.Fatalf("err = %v, want a *core.NotOwnerError", err)
	}
	if notOwner.CurrentOwner != "node-c" || notOwner.ShardID != 3 {
		t.Fatalf("NotOwnerError = %+v", notOwner)
	}
	// The routing value comes back even on the refusal, so the response can
	// still say which shard was involved.
	if rt.ShardID != 3 || rt.Local {
		t.Fatalf("Routing = %+v", rt)
	}
}

// TestGatewayResolveUnreadableLease: a lease we cannot read is a lease we do
// not hold. A database blip must refuse routing, not be reported as a workflow
// failure.
func TestGatewayResolveUnreadableLease(t *testing.T) {
	gw := &Gateway{
		Self:   "node-a",
		Shards: fixedShard(3),
		Leases: &fakeLeases{err: errors.New("connection refused")},
		Clock:  fixedClock{routingNow},
	}
	_, err := gw.Resolve(context.Background(), "trip-42")

	var notOwner *core.NotOwnerError
	if !errors.As(err, &notOwner) {
		t.Fatalf("err = %v, want a *core.NotOwnerError", err)
	}
	if notOwner.CurrentOwner != "" {
		t.Fatalf("CurrentOwner = %q, want it unknown", notOwner.CurrentOwner)
	}
}
