package lease

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---- the pure state machine ----------------------------------------------

// Action is what the manager should do about one shard at one instant.
type Action string

const (
	// ActionNone: nothing is due yet.
	ActionNone Action = "none"
	// ActionRenew: we own this shard and the renewal is due.
	ActionRenew Action = "renew"
	// ActionAcquire: we do not own this shard and it is time to try.
	ActionAcquire Action = "acquire"
	// ActionDeclareLost: we still think we own this shard, but we have not
	// managed to renew it for longer than the lease lasts, so in the database it
	// has already expired and somebody else may have taken it. We must give it up
	// ourselves without being told.
	ActionDeclareLost Action = "declare_lost"
)

// ShardState is everything the manager remembers about one shard. It carries no
// clock and no database handle, which is what lets the decision below be a plain
// function of values.
type ShardState struct {
	Owned bool
	Token core.FencingToken
	// LastRenew is when ownership was last actually confirmed by the database,
	// by a successful acquire or a successful renew.
	LastRenew time.Time
	// LastAttempt is when we last tried to acquire a shard we do not own.
	LastAttempt time.Time
}

// Policy holds the three durations that drive the loop.
type Policy struct {
	// TTL is how long a lease survives without a renewal. 10s.
	TTL time.Duration
	// RenewEvery is how often we refresh a lease we hold. 3s.
	RenewEvery time.Duration
	// AcquireEvery is how often we try for a shard we do not hold. 5s.
	AcquireEvery time.Duration
}

// DefaultPolicy renews every 3 seconds on a 10 second lease and looks for
// unowned shards every 5 seconds.
//
// The gap between 3 and 10 is the safety margin, and it is deliberately wide.
// A node gets three chances to renew before its lease lapses, so a single slow
// query or a brief network hiccup does not cause a needless handover. Handovers
// are not dangerous — the fencing token makes them safe — but they are wasteful,
// because the new owner has to reload state the old owner already had.
func DefaultPolicy() Policy {
	return Policy{TTL: 10 * time.Second, RenewEvery: 3 * time.Second, AcquireEvery: 5 * time.Second}
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.TTL <= 0 {
		p.TTL = d.TTL
	}
	if p.RenewEvery <= 0 {
		p.RenewEvery = d.RenewEvery
	}
	if p.AcquireEvery <= 0 {
		p.AcquireEvery = d.AcquireEvery
	}
	if p.RenewEvery >= p.TTL {
		// Renewing no more often than the lease lasts guarantees losing it. Rather
		// than trust the caller, clamp to a third of the TTL, which restores the
		// three-chances margin described above.
		p.RenewEvery = p.TTL / 3
	}
	return p
}

// TTLSeconds is the TTL in the units core.LeaseStore takes. It rounds up so a
// sub-second policy in a test never asks the database for a zero-length lease.
func (p Policy) TTLSeconds() int {
	s := int((p.TTL + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// Decide is the whole ownership policy, as a pure function.
//
// No database, no clock, no goroutine: given what we remember about a shard and
// what time it is, it returns the one thing to do. That means the awkward cases
// — a renewal that never comes back, a lease that quietly expired while we were
// stalled — can be tested exhaustively in microseconds instead of being hoped
// about.
//
// The order of the checks matters. Expiry is tested before renewal, because once
// the lease has outlived its TTL there is no point renewing: the row in the
// database is already claimable by anyone, and continuing to act as owner would
// mean two nodes believing they own one shard.
func Decide(st ShardState, now time.Time, p Policy) Action {
	p = p.withDefaults()

	if st.Owned {
		if !st.LastRenew.IsZero() && now.Sub(st.LastRenew) >= p.TTL {
			return ActionDeclareLost
		}
		if st.LastRenew.IsZero() || now.Sub(st.LastRenew) >= p.RenewEvery {
			return ActionRenew
		}
		return ActionNone
	}

	if st.LastAttempt.IsZero() || now.Sub(st.LastAttempt) >= p.AcquireEvery {
		return ActionAcquire
	}
	return ActionNone
}

// Outcome is what the database said in response to an Action.
type Outcome struct {
	// Err is nil on success. core.ErrLostOwnership from a renewal means we were
	// fenced out; ErrLeaseHeld from an acquire means somebody else got there
	// first, which is ordinary.
	Err error
	// Token is the new fencing token, set only for a successful acquire.
	Token core.FencingToken
}

// Apply folds an Outcome back into the state and reports whether ownership was
// lost as a result. It is pure, for the same reason Decide is.
//
// The subtle case is a renewal that fails for a reason other than being fenced
// out — the database was unreachable, the query timed out. We keep believing we
// own the shard, but we deliberately do NOT advance LastRenew. That leaves the
// clock running, so if the trouble persists past the TTL, Decide returns
// ActionDeclareLost on a later tick and we stand down on our own. Ownership is
// never extended by an attempt that did not reach the database.
func Apply(st ShardState, a Action, out Outcome, now time.Time) (ShardState, bool) {
	switch a {
	case ActionAcquire:
		st.LastAttempt = now
		if out.Err != nil {
			return st, false
		}
		st.Owned = true
		st.Token = out.Token
		st.LastRenew = now
		return st, false

	case ActionRenew:
		if out.Err == nil {
			st.LastRenew = now
			return st, false
		}
		if errors.Is(out.Err, core.ErrLostOwnership) {
			return dropped(st, now), true
		}
		return st, false

	case ActionDeclareLost:
		return dropped(st, now), true

	default:
		return st, false
	}
}

// dropped is the single place ownership is given up, so the reset is identical
// however we arrived at it. LastAttempt is set to now so the manager waits a
// full acquire interval before trying to take the shard back, rather than
// fighting the node that just took it.
func dropped(st ShardState, now time.Time) ShardState {
	st.Owned = false
	st.Token = 0
	st.LastRenew = time.Time{}
	st.LastAttempt = now
	return st
}

// ---- the manager ----------------------------------------------------------

// TransitionObserver receives one call per ownership change. It is declared
// here, as the narrowest interface the manager needs, rather than importing the
// metrics package, so this package stays testable with no Prometheus registry
// and keeps no dependency on another agent's wiring. internal/obs.Metrics
// satisfies it.
type TransitionObserver interface {
	LeaseTransition(shard core.ShardID, direction string)
}

// The four directions one ownership change can take.
//
// They are spelled to match the direction label values internal/obs exports as
// LeaseAcquired, LeaseRenewed, LeaseLost and LeaseReleased, and are restated
// here rather than imported to keep the dependency pointing one way only.
// manager_test.go pins the two spellings together, because a drift between them
// would not break anything — it would quietly split one time series in two and
// leave the dashboard looking plausible.
const (
	// DirectionAcquired: the database handed us the shard and a new, higher
	// fencing token.
	DirectionAcquired = "acquired"
	// DirectionRenewed: we already owned the shard and pushed its expiry out.
	// This is the heartbeat, so it is the baseline every other direction is
	// read against: roughly one per shard per RenewEvery in steady state.
	DirectionRenewed = "renewed"
	// DirectionLost: ownership went away without our consent — either another
	// node fenced us out, or we could not reach the database for a whole TTL
	// and stood down.
	DirectionLost = "lost"
	// DirectionReleased: we gave the shard up deliberately, on shutdown or on a
	// handback to its preferred owner.
	DirectionReleased = "released"
)

// ManagerConfig configures the background ownership loop.
type ManagerConfig struct {
	// Owner is this node's identity, written into owner_node. It must be stable
	// across a restart of the same node and unique across the fleet.
	Owner string
	// Shards is the set of shards this node is willing to own.
	Shards []core.ShardID
	// Policy holds the three durations. Zero fields take DefaultPolicy values.
	Policy Policy
	// Tick is how often the loop wakes up. It should be well below RenewEvery so
	// a renewal is never late by a whole tick. Zero means 1s.
	Tick time.Duration
	// Clock is the source of "now" for every decision, so a test can drive the
	// state machine to any point in time. Zero means the system clock.
	Clock core.Clock
	// Wants optionally filters which shards to try for, so the consistent hash
	// ring can express a preference. Returning false only stops us reaching for a
	// shard we do not hold; it never makes us drop one we do, because dropping a
	// shard is the lease's decision, not the ring's.
	Wants func(core.ShardID) bool

	// Releases optionally hands an owned shard back. It is consulted only for
	// a shard this node currently owns and is about to renew; returning true
	// releases the lease instead of renewing it, and fires OnLost.
	//
	// It is the other half of Wants. Wants lets a node adopt an orphan when
	// its preferred owner dies, which is what makes failover work — but on its
	// own it is a one-way door. When the preferred owner comes back, the node
	// that adopted its shards keeps renewing them forever, because a live lease
	// can never be acquired. Observed on a live cluster: a restarted node owned
	// zero shards and stayed that way, and after enough failovers one node
	// would accumulate the entire workload.
	//
	// Releasing is safe by construction. The rightful owner acquires with a
	// higher fencing token, so anything this node still has in flight is
	// refused at commit rather than written twice.
	Releases func(core.ShardID) bool
	// OnLost is called once, outside the manager's lock, whenever a shard is lost
	// — either fenced out by another node or expired because we could not renew.
	// The callback must stop all work on that shard. It must not block for long.
	OnLost func(core.ShardID)
	// OnError observes database errors. Nil discards them.
	OnError func(shard core.ShardID, err error)

	// Transitions counts ownership changes. Nil records nothing, not a panic,
	// because the manager has to run in a unit test with no registry.
	//
	// It lives on the manager rather than being left to OnLost, which is where
	// it started: OnLost fires for a handback as well as for a real loss, so
	// counting there reported a deliberate release as a lost lease and could
	// never see an acquire or a renewal at all. The manager is the only place
	// that knows which of the four actually happened.
	Transitions TransitionObserver
}

func (c ManagerConfig) withDefaults() ManagerConfig {
	c.Policy = c.Policy.withDefaults()
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.Clock == nil {
		c.Clock = core.SystemClock{}
	}
	return c
}

// Manager keeps this node's shard leases alive and picks up unowned shards.
type Manager struct {
	store core.LeaseStore
	cfg   ManagerConfig

	mu    sync.RWMutex
	state map[core.ShardID]ShardState
}

// NewManager builds a Manager. It takes core.LeaseStore rather than *Store so a
// test can drive the whole loop against a fake with no database.
func NewManager(store core.LeaseStore, cfg ManagerConfig) *Manager {
	cfg = cfg.withDefaults()
	m := &Manager{store: store, cfg: cfg, state: make(map[core.ShardID]ShardState, len(cfg.Shards))}
	for _, s := range cfg.Shards {
		m.state[s] = ShardState{}
	}
	return m
}

// Owned returns a snapshot of the shards this node currently holds and the
// fencing token for each.
//
// It returns a fresh map every call. Handing out the internal one would let a
// caller read it while the loop is writing, which is a data race and, worse,
// could show a shard as owned a moment after it was lost.
func (m *Manager) Owned() map[core.ShardID]core.FencingToken {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[core.ShardID]core.FencingToken, len(m.state))
	for id, st := range m.state {
		if st.Owned {
			out[id] = st.Token
		}
	}
	return out
}

// Token reports the fencing token for a shard, and whether we own it at all.
// Callers pass the token straight into a Decision, where it is re-verified
// inside the writing transaction — so a token that goes stale between this call
// and the commit is caught there, not here.
func (m *Manager) Token(shard core.ShardID) (core.FencingToken, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.state[shard]
	return st.Token, st.Owned
}

// Run drives the loop until ctx is cancelled, then releases every held lease so
// a planned shutdown hands the shards over immediately instead of making the
// rest of the fleet wait out the TTL.
func (m *Manager) Run(ctx context.Context) error {
	t := time.NewTicker(m.cfg.Tick)
	defer t.Stop()

	// One pass immediately, so a starting node does not idle for a whole tick
	// before claiming anything.
	m.Step(ctx)

	for {
		select {
		case <-ctx.Done():
			m.releaseAll()
			return nil
		case <-t.C:
			m.Step(ctx)
		}
	}
}

// Step runs one pass over every shard. It is exported so a test can drive the
// manager one deterministic pass at a time instead of waiting for a ticker.
func (m *Manager) Step(ctx context.Context) {
	now := m.cfg.Clock.Now()
	for _, shard := range m.cfg.Shards {
		m.stepShard(ctx, shard, now)
	}
}

func (m *Manager) stepShard(ctx context.Context, shard core.ShardID, now time.Time) {
	m.mu.RLock()
	st := m.state[shard]
	m.mu.RUnlock()

	action := Decide(st, now, m.cfg.Policy)
	if action == ActionAcquire && m.cfg.Wants != nil && !m.cfg.Wants(shard) {
		return
	}
	if action == ActionRenew && m.cfg.Releases != nil && m.cfg.Releases(shard) {
		m.handback(ctx, shard, st, now)
		return
	}

	var out Outcome
	switch action {
	case ActionNone:
		return
	case ActionRenew:
		out.Err = m.store.Renew(ctx, shard, m.cfg.Owner, st.Token, m.cfg.Policy.TTLSeconds())
	case ActionAcquire:
		out.Token, out.Err = m.store.Acquire(ctx, shard, m.cfg.Owner, m.cfg.Policy.TTLSeconds())
	case ActionDeclareLost:
		// No call: the point of this action is that the database is exactly what
		// we could not reach.
	}

	next, lost := Apply(st, action, out, now)

	m.mu.Lock()
	m.state[shard] = next
	m.mu.Unlock()

	m.count(shard, DirectionOf(action, out, lost))

	// The callback runs after the lock is released. Ownership must already be
	// recorded as gone before anyone is told, and holding the lock across a
	// caller's code invites a deadlock.
	if lost && m.cfg.OnLost != nil {
		m.cfg.OnLost(shard)
	}
	if out.Err != nil && m.cfg.OnError != nil && !errors.Is(out.Err, ErrLeaseHeld) {
		m.cfg.OnError(shard, out.Err)
	}
}

// DirectionOf names the transition an action and its outcome amount to, or ""
// when ownership did not move.
//
// It is pure and exported for the same reason Decide and Apply are: the mapping
// is the part with a real chance of being subtly wrong, and a wrong direction
// label is not a crash, it is a dashboard that reads plausibly and says the
// wrong thing. The two silent cases are the ones worth stating: an acquire that
// found the shard already held changed nothing, and a renewal that failed to
// reach the database changed nothing either — we still own the shard, and the
// loss, if the trouble persists, is counted later by ActionDeclareLost.
func DirectionOf(a Action, out Outcome, lost bool) string {
	if lost {
		return DirectionLost
	}
	if out.Err != nil {
		return ""
	}
	switch a {
	case ActionAcquire:
		return DirectionAcquired
	case ActionRenew:
		return DirectionRenewed
	default:
		return ""
	}
}

// count reports one transition. An empty direction is "nothing happened", so
// every caller can hand over whatever DirectionOf returned without checking.
func (m *Manager) count(shard core.ShardID, direction string) {
	if direction == "" || m.cfg.Transitions == nil {
		return
	}
	m.cfg.Transitions.LeaseTransition(shard, direction)
}

// releaseAll gives up every held lease on shutdown. It uses a fresh, short
// context because the manager's own context is already cancelled by the time
// this runs, and a cancelled context cannot issue the release.
// handback releases one owned shard so its preferred owner can take it.
//
// A failed release keeps ownership: the lease is still ours, it will be renewed
// on the next tick, and the handback is simply retried. Dropping local state
// on a failed release would leave the node believing it had let go of a shard
// the database still says it owns.
func (m *Manager) handback(ctx context.Context, shard core.ShardID, st ShardState, now time.Time) {
	if err := m.store.Release(ctx, shard, m.cfg.Owner, st.Token); err != nil {
		if m.cfg.OnError != nil {
			m.cfg.OnError(shard, err)
		}
		return
	}
	m.mu.Lock()
	m.state[shard] = dropped(st, now)
	m.mu.Unlock()
	// Released, not lost: nobody took this shard from us, we handed it over. The
	// distinction is the whole value of the label — a cluster rebalancing itself
	// and a cluster failing over look identical without it.
	m.count(shard, DirectionReleased)
	if m.cfg.OnLost != nil {
		m.cfg.OnLost(shard)
	}
}

func (m *Manager) releaseAll() {
	owned := m.Owned()
	if len(owned) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for shard, tok := range owned {
		// A release that did not reach the database is not a handover: the row
		// still names this node and the rest of the fleet has to wait out the
		// whole TTL before the shard is claimable. That is what "lost" means
		// everywhere else, so it is counted as that rather than flattered.
		direction := DirectionReleased
		if err := m.store.Release(ctx, shard, m.cfg.Owner, tok); err != nil {
			direction = DirectionLost
			if m.cfg.OnError != nil {
				m.cfg.OnError(shard, err)
			}
		}
		m.mu.Lock()
		m.state[shard] = dropped(m.state[shard], m.cfg.Clock.Now())
		m.mu.Unlock()
		m.count(shard, direction)
	}
}
