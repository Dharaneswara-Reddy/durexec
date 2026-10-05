package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// ---- the pure state machine ----------------------------------------------

func TestDecide(t *testing.T) {
	p := Policy{TTL: 10 * time.Second, RenewEvery: 3 * time.Second, AcquireEvery: 5 * time.Second}

	tests := []struct {
		name  string
		state ShardState
		now   time.Time
		want  Action
	}{
		{
			"a shard we have never touched is acquired at once",
			ShardState{},
			t0,
			ActionAcquire,
		},
		{
			"a failed acquisition is not retried immediately",
			ShardState{LastAttempt: t0},
			t0.Add(2 * time.Second),
			ActionNone,
		},
		{
			"a failed acquisition is retried after the acquire interval",
			ShardState{LastAttempt: t0},
			t0.Add(5 * time.Second),
			ActionAcquire,
		},
		{
			"a freshly acquired lease needs nothing yet",
			ShardState{Owned: true, Token: 1, LastRenew: t0},
			t0.Add(time.Second),
			ActionNone,
		},
		{
			"renewal is due at exactly the renew interval",
			ShardState{Owned: true, Token: 1, LastRenew: t0},
			t0.Add(3 * time.Second),
			ActionRenew,
		},
		{
			"renewal stays due while it keeps failing",
			ShardState{Owned: true, Token: 1, LastRenew: t0},
			t0.Add(7 * time.Second),
			ActionRenew,
		},
		{
			"once the TTL has passed the lease is gone and we stand down",
			ShardState{Owned: true, Token: 1, LastRenew: t0},
			t0.Add(10 * time.Second),
			ActionDeclareLost,
		},
		{
			"and stays lost however long we were stalled",
			ShardState{Owned: true, Token: 1, LastRenew: t0},
			t0.Add(time.Hour),
			ActionDeclareLost,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.state, tc.now, p); got != tc.want {
				t.Fatalf("Decide = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApply(t *testing.T) {
	owned := ShardState{Owned: true, Token: 4, LastRenew: t0}
	later := t0.Add(3 * time.Second)

	tests := []struct {
		name      string
		state     ShardState
		action    Action
		outcome   Outcome
		wantOwned bool
		wantToken core.FencingToken
		wantLost  bool
	}{
		{
			"a successful acquisition takes ownership with the new token",
			ShardState{}, ActionAcquire, Outcome{Token: 9},
			true, 9, false,
		},
		{
			"losing the race for a shard leaves us unowned and not lost",
			ShardState{}, ActionAcquire, Outcome{Err: ErrLeaseHeld},
			false, 0, false,
		},
		{
			"a successful renewal keeps ownership and the same token",
			owned, ActionRenew, Outcome{},
			true, 4, false,
		},
		{
			"a REJECTED renewal means we were fenced out and must drop the shard",
			owned, ActionRenew, Outcome{Err: core.ErrLostOwnership},
			false, 0, true,
		},
		{
			"a wrapped rejection is recognised too",
			owned, ActionRenew, Outcome{Err: fmt.Errorf("renew shard 3: %w", core.ErrLostOwnership)},
			false, 0, true,
		},
		{
			"a renewal that could not reach the database keeps ownership for now",
			owned, ActionRenew, Outcome{Err: errors.New("connection refused")},
			true, 4, false,
		},
		{
			"declaring the lease expired drops it without asking anyone",
			owned, ActionDeclareLost, Outcome{},
			false, 0, true,
		},
		{
			"doing nothing changes nothing",
			owned, ActionNone, Outcome{},
			true, 4, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, lost := Apply(tc.state, tc.action, tc.outcome, later)
			if got.Owned != tc.wantOwned {
				t.Errorf("Owned = %v, want %v", got.Owned, tc.wantOwned)
			}
			if got.Token != tc.wantToken {
				t.Errorf("Token = %d, want %d", got.Token, tc.wantToken)
			}
			if lost != tc.wantLost {
				t.Errorf("lost = %v, want %v", lost, tc.wantLost)
			}
		})
	}
}

// The dangerous case in full: the database is unreachable, so renewals keep
// failing for a reason that is not a rejection. Ownership must expire on its own
// once the TTL passes, without anybody telling us.
func TestOwnershipExpiresWhenRenewalsCannotReachTheDatabase(t *testing.T) {
	p := Policy{TTL: 10 * time.Second, RenewEvery: 3 * time.Second, AcquireEvery: 5 * time.Second}
	st := ShardState{Owned: true, Token: 2, LastRenew: t0}
	unreachable := Outcome{Err: errors.New("i/o timeout")}

	var declaredLostAt time.Time
	for i := 1; i <= 20; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		a := Decide(st, now, p)
		var lost bool
		st, lost = Apply(st, a, unreachable, now)
		if lost {
			declaredLostAt = now
			break
		}
	}
	if declaredLostAt.IsZero() {
		t.Fatal("ownership was never given up, so two nodes could believe they own the shard")
	}
	if got := declaredLostAt.Sub(t0); got != 10*time.Second {
		t.Fatalf("ownership was given up after %v, want exactly the 10s TTL", got)
	}
	if st.Owned {
		t.Fatal("state still claims ownership after declaring the loss")
	}
}

// A successful renewal must push the expiry out, so a healthy node keeps its
// shard indefinitely and never trips the TTL rule.
func TestAHealthyNodeKeepsItsShard(t *testing.T) {
	p := DefaultPolicy()
	st := ShardState{Owned: true, Token: 1, LastRenew: t0}
	for i := 1; i <= 300; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		a := Decide(st, now, p)
		if a == ActionDeclareLost || a == ActionAcquire {
			t.Fatalf("at +%ds a healthy node decided %q", i, a)
		}
		var lost bool
		st, lost = Apply(st, a, Outcome{}, now)
		if lost {
			t.Fatalf("at +%ds a healthy node lost its shard", i)
		}
	}
}

func TestPolicyWithDefaults(t *testing.T) {
	d := DefaultPolicy()
	if d.RenewEvery != 3*time.Second || d.TTL != 10*time.Second || d.AcquireEvery != 5*time.Second {
		t.Fatalf("DefaultPolicy changed: %+v", d)
	}
	if got := (Policy{}).withDefaults(); got != d {
		t.Fatalf("zero Policy = %+v, want the defaults", got)
	}
	// Renewing no more often than the lease lasts guarantees losing it, so the
	// clamp must fire rather than trusting the caller.
	bad := Policy{TTL: 9 * time.Second, RenewEvery: 30 * time.Second, AcquireEvery: time.Second}.withDefaults()
	if bad.RenewEvery >= bad.TTL {
		t.Fatalf("RenewEvery %v was not clamped below TTL %v", bad.RenewEvery, bad.TTL)
	}
}

func TestPolicyTTLSeconds(t *testing.T) {
	tests := []struct {
		ttl  time.Duration
		want int
	}{
		{10 * time.Second, 10},
		{time.Second, 1},
		{1500 * time.Millisecond, 2}, // rounds up, never down to zero
		{100 * time.Millisecond, 1},
		{0, 1},
	}
	for _, tc := range tests {
		if got := (Policy{TTL: tc.ttl}).TTLSeconds(); got != tc.want {
			t.Errorf("TTLSeconds(%v) = %d, want %d", tc.ttl, got, tc.want)
		}
	}
}

// ---- the manager, driven against a fake store -----------------------------

// fakeStore is an in-memory core.LeaseStore. It models the real UPDATE rules:
// an acquire succeeds only if the shard is free or already ours, and a renew
// succeeds only if both the owner and the token still match.
type fakeStore struct {
	mu    sync.Mutex
	rows  map[core.ShardID]core.Lease
	now   func() time.Time
	fail  error // when set, every call returns it
	calls []string
}

func newFakeStore(now func() time.Time, shards ...core.ShardID) *fakeStore {
	f := &fakeStore{rows: map[core.ShardID]core.Lease{}, now: now}
	for _, s := range shards {
		f.rows[s] = core.Lease{ShardID: s}
	}
	return f
}

func (f *fakeStore) Acquire(_ context.Context, shard core.ShardID, owner string, ttl int) (core.FencingToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "acquire")
	if f.fail != nil {
		return 0, f.fail
	}
	l := f.rows[shard]
	free := l.Owner == "" || l.Owner == owner || !l.ExpiresAt.After(f.now())
	if !free {
		return 0, ErrLeaseHeld
	}
	l.ShardID = shard
	l.Owner = owner
	l.Token++
	l.ExpiresAt = f.now().Add(time.Duration(ttl) * time.Second)
	f.rows[shard] = l
	return l.Token, nil
}

func (f *fakeStore) Renew(_ context.Context, shard core.ShardID, owner string, tok core.FencingToken, ttl int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "renew")
	if f.fail != nil {
		return f.fail
	}
	l := f.rows[shard]
	if l.Owner != owner || l.Token != tok {
		return core.ErrLostOwnership
	}
	l.ExpiresAt = f.now().Add(time.Duration(ttl) * time.Second)
	f.rows[shard] = l
	return nil
}

func (f *fakeStore) Release(_ context.Context, shard core.ShardID, owner string, tok core.FencingToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "release")
	l := f.rows[shard]
	if l.Owner == owner && l.Token == tok {
		l.Owner = ""
		l.ExpiresAt = f.now()
		f.rows[shard] = l
	}
	return nil
}

func (f *fakeStore) Get(_ context.Context, shard core.ShardID) (core.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.rows[shard]
	if !ok {
		return core.Lease{}, ErrLeaseNotFound
	}
	return l, nil
}

func (f *fakeStore) List(context.Context) ([]core.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.Lease, 0, len(f.rows))
	for _, l := range f.rows {
		out = append(out, l)
	}
	return out, nil
}

// steal simulates another node taking the shard: the token moves on, which is
// exactly what fences us out.
func (f *fakeStore) steal(shard core.ShardID, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.rows[shard]
	l.Owner = owner
	l.Token++
	l.ExpiresAt = f.now().Add(time.Minute)
	f.rows[shard] = l
}

var _ core.LeaseStore = (*fakeStore)(nil)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestManagerAcquiresThenRenews(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0, 1)
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0, 1}, Clock: clk,
	})
	ctx := context.Background()

	m.Step(ctx)
	owned := m.Owned()
	if len(owned) != 2 {
		t.Fatalf("owned %d shards after the first step, want 2", len(owned))
	}
	for shard, tok := range owned {
		if tok != 1 {
			t.Fatalf("shard %d token = %d, want 1 from the first acquisition", shard, tok)
		}
	}

	// Too soon to renew.
	clk.advance(time.Second)
	m.Step(ctx)
	if got := countCalls(store, "renew"); got != 0 {
		t.Fatalf("%d renewals after 1s, want 0", got)
	}

	// Now it is due.
	clk.advance(3 * time.Second)
	m.Step(ctx)
	if got := countCalls(store, "renew"); got != 2 {
		t.Fatalf("%d renewals after 4s, want 2 (one per shard)", got)
	}
	if len(m.Owned()) != 2 {
		t.Fatal("a successful renewal lost a shard")
	}
}

// The headline case: another node takes the shard, our renewal is refused, and
// we must drop ownership and tell the caller to stop working.
func TestManagerDropsOwnershipWhenARenewalIsRejected(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0)

	var lost []core.ShardID
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0}, Clock: clk,
		OnLost: func(s core.ShardID) { lost = append(lost, s) },
	})
	ctx := context.Background()

	m.Step(ctx)
	if _, ok := m.Token(0); !ok {
		t.Fatal("did not acquire the shard")
	}

	store.steal(0, "node-b")

	clk.advance(3 * time.Second)
	m.Step(ctx)

	if len(lost) != 1 || lost[0] != 0 {
		t.Fatalf("OnLost fired %v, want exactly [0]", lost)
	}
	if _, ok := m.Token(0); ok {
		t.Fatal("still claiming ownership after being fenced out")
	}
	if len(m.Owned()) != 0 {
		t.Fatal("Owned still lists a shard we were fenced out of")
	}

	// And we must not immediately fight the new owner for it back.
	clk.advance(time.Second)
	before := countCalls(store, "acquire")
	m.Step(ctx)
	if countCalls(store, "acquire") != before {
		t.Fatal("tried to re-acquire before the acquire interval elapsed")
	}
}

func TestManagerStandsDownWhenTheStoreIsUnreachable(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0)

	var lost []core.ShardID
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0}, Clock: clk,
		OnLost: func(s core.ShardID) { lost = append(lost, s) },
	})
	ctx := context.Background()
	m.Step(ctx)

	store.mu.Lock()
	store.fail = errors.New("connection refused")
	store.mu.Unlock()

	// Walk past the 10s TTL one second at a time.
	for i := 0; i < 12; i++ {
		clk.advance(time.Second)
		m.Step(ctx)
	}
	if len(lost) != 1 {
		t.Fatalf("OnLost fired %d times, want 1: an unreachable database must still end ownership", len(lost))
	}
	if len(m.Owned()) != 0 {
		t.Fatal("still claiming a shard whose lease has certainly expired")
	}
}

func TestManagerRespectsWants(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0, 1, 2)
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0, 1, 2}, Clock: clk,
		Wants: func(s core.ShardID) bool { return s != 1 },
	})
	m.Step(context.Background())

	owned := m.Owned()
	if len(owned) != 2 {
		t.Fatalf("owned %v, want shards 0 and 2 only", owned)
	}
	if _, ok := owned[1]; ok {
		t.Fatal("took a shard the ring did not want")
	}
}

func TestManagerLosesTheRaceGracefully(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0)
	store.steal(0, "node-b") // somebody else got there first

	var reported []error
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0}, Clock: clk,
		OnError: func(_ core.ShardID, err error) { reported = append(reported, err) },
	})
	m.Step(context.Background())

	if len(m.Owned()) != 0 {
		t.Fatal("took a shard that was already held")
	}
	// Losing a race is ordinary, not a fault, so it must not be reported as an
	// error the operator has to look at.
	if len(reported) != 0 {
		t.Fatalf("ErrLeaseHeld was reported as an error: %v", reported)
	}
}

func TestOwnedIsASnapshot(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0)
	m := NewManager(store, ManagerConfig{Owner: "node-a", Shards: []core.ShardID{0}, Clock: clk})
	m.Step(context.Background())

	snap := m.Owned()
	delete(snap, 0)
	snap[99] = 42

	if len(m.Owned()) != 1 {
		t.Fatal("mutating the returned map changed the manager's own state")
	}
}

// Owned is read by workers on other goroutines while the loop writes it, so it
// has to be safe under -race.
func TestOwnedIsSafeForConcurrentReaders(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0, 1, 2, 3)
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0, 1, 2, 3}, Clock: clk,
	})
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.Owned()
					_, _ = m.Token(1)
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		clk.advance(time.Second)
		m.Step(ctx)
	}
	close(stop)
	wg.Wait()
}

func TestRunReleasesEverythingOnShutdown(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0, 1)
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0, 1}, Clock: clk, Tick: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for len(m.Owned()) < 2 {
		select {
		case <-deadline:
			t.Fatal("the manager never acquired its shards")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}

	// A planned shutdown hands the shards straight over instead of making the
	// rest of the fleet wait out the TTL.
	if got := countCalls(store, "release"); got != 2 {
		t.Fatalf("%d releases on shutdown, want 2", got)
	}
	if len(m.Owned()) != 0 {
		t.Fatal("still claiming shards after shutdown")
	}
}

func countCalls(f *fakeStore, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == name {
			n++
		}
	}
	return n
}

// A node that adopted an orphan must hand it back once the rightful owner
// returns. Without this, observed on a live cluster, a restarted node owned
// zero shards forever and one node slowly accumulated the whole workload.
func TestManagerHandsBackAShardItShouldNotOwn(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0, 1)
	giveBack := map[core.ShardID]bool{}
	var lost []core.ShardID
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0, 1}, Clock: clk,
		Releases: func(s core.ShardID) bool { return giveBack[s] },
		OnLost:   func(s core.ShardID) { lost = append(lost, s) },
	})
	ctx := context.Background()

	m.Step(ctx) // acquires both
	if len(m.Owned()) != 2 {
		t.Fatalf("setup: owned %d shards, want 2", len(m.Owned()))
	}

	// The preferred owner of shard 1 has come back.
	giveBack[1] = true
	clk.advance(4 * time.Second) // renewal is due, so Releases is consulted
	m.Step(ctx)

	owned := m.Owned()
	if _, still := owned[1]; still {
		t.Fatal("shard 1 was renewed instead of handed back")
	}
	if _, kept := owned[0]; !kept {
		t.Fatal("shard 0 was dropped; only the shard Releases named should go")
	}
	if len(lost) != 1 || lost[0] != 1 {
		t.Fatalf("OnLost fired for %v, want exactly [1] so its workers stop", lost)
	}
	if got := countCalls(store, "release"); got != 1 {
		t.Fatalf("%d releases, want 1", got)
	}
}

// Releases is consulted only for a shard we own and are about to renew. It must
// never cause a release of a shard we do not hold, and must never fire while the
// lease is not yet due — that would turn a preference into constant churn.
func TestReleasesIsNotConsultedBeforeRenewalIsDue(t *testing.T) {
	clk := &fakeClock{now: t0}
	store := newFakeStore(clk.Now, 0)
	calls := 0
	m := NewManager(store, ManagerConfig{
		Owner: "node-a", Shards: []core.ShardID{0}, Clock: clk,
		Releases: func(core.ShardID) bool { calls++; return true },
	})
	ctx := context.Background()

	m.Step(ctx) // acquire: not a renewal, so Releases must not be asked
	if calls != 0 {
		t.Fatalf("Releases consulted %d times during acquisition, want 0", calls)
	}
	clk.advance(time.Second) // renewal not yet due
	m.Step(ctx)
	if calls != 0 || len(m.Owned()) != 1 {
		t.Fatalf("handed back before renewal was due (calls=%d, owned=%d)", calls, len(m.Owned()))
	}
}
