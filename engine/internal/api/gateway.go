package api

import (
	"context"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ShardResolver maps a workflow's business key onto its permanent shard. It is
// declared as a one-method interface rather than taken as a dependency on the
// shard package so the API can be tested with a trivial stub, and so the
// integrator can swap the shard count without this package knowing.
type ShardResolver interface {
	ShardFor(workflowID string) core.ShardID
}

// Routing is the answer to "should this node handle this request itself?".
type Routing struct {
	ShardID core.ShardID
	// Owner is the node that currently owns the shard, or "" when the lease is
	// unowned or expired and nobody does.
	Owner string
	// Local is true when this node may handle the request itself.
	Local bool
}

// Decide is the routing rule, and it is a pure function of its four arguments.
//
// Keeping it pure is the point. Routing is where a distributed system goes
// wrong quietly: a node that wrongly believes it owns a shard will happily
// serve stale reads and attempt writes it has no right to make. That rule
// therefore has to be exhaustively testable, and it cannot be if it is
// entangled with a database call, a clock reading and an HTTP response.
// Everything that can fail — fetching the lease, reading the time — happens in
// Gateway.Resolve; the decision itself happens here, where a table test can
// enumerate every case.
//
// A node may handle a request only when it is the named owner AND the lease
// has not expired. The expiry half matters as much as the name: a node that
// has been partitioned away is still written in the lease row as the owner,
// but its lease has lapsed and another node is entitled to take it. Serving
// requests on a lapsed lease is exactly the split brain the fencing token
// exists to stop, and it is cheaper to refuse here than to be refused later by
// the token check inside the commit.
//
// Being wrong in the refusing direction is safe and expected. A gateway acting
// on a lease it read a moment ago can return NotOwner for a shard it has since
// been given; the client refreshes and retries, and nothing is corrupted.
func Decide(shard core.ShardID, self string, lease core.Lease, now time.Time) Routing {
	owner := lease.Owner
	expired := !lease.ExpiresAt.After(now)
	if expired {
		// Report no owner rather than the stale name. Telling a client to
		// retry against a node whose lease has lapsed just sends it somewhere
		// that will refuse it again.
		owner = ""
	}
	return Routing{
		ShardID: shard,
		Owner:   owner,
		Local:   !expired && lease.Owner == self && self != "",
	}
}

// Gateway answers routing questions for one node. It is the impure shell
// around Decide: it fetches the lease and reads the clock, then defers the
// actual judgement.
type Gateway struct {
	Self   string
	Shards ShardResolver
	Leases core.LeaseStore
	Clock  core.Clock
}

// NewGateway builds a gateway for the node named self.
func NewGateway(self string, shards ShardResolver, leases core.LeaseStore) *Gateway {
	return &Gateway{Self: self, Shards: shards, Leases: leases, Clock: core.SystemClock{}}
}

// Resolve maps a workflow id to a shard and reports whether this node may
// serve it. When it may not, the returned error is a *core.NotOwnerError
// carrying the current owner, which writeError turns into a 409 the client can
// act on.
//
// The Routing value is returned even alongside the error, because the response
// headers should still say which shard was involved and who owns it. A client
// debugging a failover wants that on the refusal more than on the success.
func (g *Gateway) Resolve(ctx context.Context, workflowID string) (Routing, error) {
	shard := g.Shards.ShardFor(workflowID)
	return g.ResolveShard(ctx, shard)
}

// ResolveShard is Resolve for a caller that already knows the shard, such as
// an admin endpoint addressing a shard directly.
func (g *Gateway) ResolveShard(ctx context.Context, shard core.ShardID) (Routing, error) {
	lease, err := g.Leases.Get(ctx, shard)
	if err != nil {
		// A lease we cannot read is a lease we do not hold. Refusing with the
		// owner unknown is the honest answer, and it keeps a database blip
		// from being reported as a workflow failure.
		return Routing{ShardID: shard}, &core.NotOwnerError{ShardID: shard, CurrentOwner: ""}
	}

	rt := Decide(shard, g.Self, lease, g.now())
	if !rt.Local {
		return rt, &core.NotOwnerError{ShardID: shard, CurrentOwner: rt.Owner}
	}
	return rt, nil
}

func (g *Gateway) now() time.Time {
	if g.Clock != nil {
		return g.Clock.Now()
	}
	return time.Now().UTC()
}
