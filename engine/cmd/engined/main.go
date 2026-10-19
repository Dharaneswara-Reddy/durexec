// Command engined is one node of the durable execution engine.
//
// Everything the engine does is in the packages under internal/. This file
// does nothing but assemble them, in the one order that works, and take them
// down again cleanly. It is deliberately the only place in the program that
// knows about all of them at once.
//
// The assembly order is not a matter of taste:
//
//  1. configuration, because a bad one must stop the process before it takes
//     a lease and starts telling other nodes it is alive;
//  2. the database, and the schema, because every other component's state
//     lives there;
//  3. the lease rows, because a shard with no row cannot be acquired;
//  4. the registries, because a worker that starts before its workflows are
//     registered will fail the first task it polls;
//  5. the HTTP server, because the moment it answers it may be asked to start
//     a workflow, and by then everything above it has to be ready;
//  6. the lease manager and the workers, last, because acquiring a shard is a
//     promise to the rest of the fleet that this node is doing that shard's
//     work.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/app/agent/llm"
	"github.com/dharaneswar339/durexec/app/delivery"
	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/config"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/eventlog"
	"github.com/dharaneswar339/durexec/internal/faultinj"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
	"github.com/dharaneswar339/durexec/internal/lease"
	"github.com/dharaneswar339/durexec/internal/obs"
	"github.com/dharaneswar339/durexec/internal/runtime"
	"github.com/dharaneswar339/durexec/internal/shard"
	"github.com/dharaneswar339/durexec/internal/taskq"
	"github.com/dharaneswar339/durexec/internal/timers"
	"github.com/dharaneswar339/durexec/internal/webui"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// migrationsDir can be overridden for a container image that puts the schema
// somewhere other than next to the binary's working directory.
const envMigrationsDir = "MIGRATIONS_DIR"

// shutdownGrace bounds the whole orderly shutdown: stop accepting requests,
// let in-flight tasks finish, hand the leases back. It is deliberately longer
// than one lease TTL would be, because handing a lease back explicitly is much
// faster for the fleet than letting it expire.
const shutdownGrace = 20 * time.Second

func main() {
	if err := run(); err != nil {
		// Written with the plain logger rather than the structured one,
		// because a failure here may well be the logger's own construction.
		fmt.Fprintf(os.Stderr, "engined: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	log := obs.NewLogger(slog.LevelInfo)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info(ctx, "starting engine node", "config", cfg.String())
	if cfg.SemanticIsFake() {
		log.Warn(ctx, "no EMBEDDER_URL configured: using the built-in deterministic stand-in encoder. "+
			"It matches identical intent text exactly and cannot recognise a reworded repeat, so verdicts from this node exercise the plumbing, not the contribution")
	}

	// ---- 2. the database ---------------------------------------------------

	pool, err := newPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	migrations := os.Getenv(envMigrationsDir)
	if migrations == "" {
		migrations = "migrations"
	}
	if abs, aerr := filepath.Abs(migrations); aerr == nil {
		migrations = abs
	}
	if err := eventlog.Migrate(ctx, pool, migrations); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	log.Info(ctx, "schema applied", "dir", migrations)

	events := eventlog.New(pool)

	// ---- 3. shard leases ---------------------------------------------------

	leases := lease.New(pool)
	if err := leases.EnsureRows(ctx, int32(cfg.ShardCount)); err != nil {
		return fmt.Errorf("ensure lease rows: %w", err)
	}

	// ---- 4. registries and the pieces that read them -----------------------

	activities := activity.NewRegistry()
	faults := faultinj.New(cfg.FaultInjection)
	renderers, err := registerActivities(activities, faults)
	if err != nil {
		return fmt.Errorf("register activities: %w", err)
	}

	workflows := workflow.NewRegistry()
	if err := runtime.RegisterDemoWorkflows(workflows); err != nil {
		return fmt.Errorf("register workflows: %w", err)
	}

	dispatcher := workflow.NewDispatcher(workflows)
	// The dispatcher stamps every scheduled activity with its declared
	// idempotency mode, and it does so on the REPLAY path, so this lookup must
	// be pure and in-memory. Registry.Mode is exactly that; anything that
	// could answer differently on a later run would break determinism.
	dispatcher.ModeFor = activities.Mode

	registry := prometheus.NewRegistry()
	metrics := obs.New(registry)
	runtimeMetrics := runtime.NewMetrics(registry)

	checker := &semantic.Checker{
		Embedder:   newEmbedder(cfg, log, ctx),
		Intents:    semantic.NewStore(pool),
		Thresholds: core.Thresholds{TauLow: cfg.TauLow, TauHigh: cfg.TauHigh},
		Clock:      core.SystemClock{},
		Observer:   metrics,
		RenderIntent: func(activityType string, input json.RawMessage) (string, error) {
			if fn, ok := renderers[activityType]; ok {
				return fn(input)
			}
			return "", nil // no opinion; the checker falls back to its generic form
		},
	}

	// The opt-in hybrid comparator (SEMANTIC_HYBRID). Assigning the extractor
	// is the whole switch: a nil Fields leaves the pure-semantic path exactly
	// as it was, which is what keeps the published measurement of the default
	// a measurement of what this binary does.
	//
	// One extractor is wired, for flight booking, because that is the only
	// activity in this deployment whose deciding fields the engine can
	// recover. An activity whose intent it cannot read falls back to the
	// pure-semantic comparison and journals that it did — see
	// internal/idem/semantic/hybrid.go.
	if cfg.SemanticHybrid {
		checker.Fields = semantic.ItineraryFields
		log.Info(ctx, "semantic dedup: hybrid comparator enabled",
			"deciding_fields", "route, when, daypart, pax",
			"note", "similarity is consulted only for the free text around those fields")
	}

	queue := taskq.New(pool, taskq.Config{LockDuration: cfg.TaskLock})

	engine := &runtime.Engine{
		Events:     events,
		Executions: events,
		// The committer is wrapped so that every write the fencing check refuses
		// is counted. The wrapper returns the error untouched — the fenced-out
		// worker stops exactly as it did before — and counts only a genuine token
		// mismatch, not the other ways a commit can report lost ownership.
		Committer:        eventlog.NewFencingCounter(events, metrics),
		Tasks:            queue,
		Leases:           leases,
		Dispatcher:       dispatcher,
		Activities:       activities,
		Executor:         activity.NewExecutor(activities),
		Checker:          checker,
		ShardCount:       int32(cfg.ShardCount),
		NodeID:           cfg.NodeID,
		Clock:            core.SystemClock{},
		Metrics:          metrics,
		Log:              log,
		Runtime:          runtimeMetrics,
		EscalationBlocks: cfg.EscalationBlocks,
		Backoff:          queue.Config().Backoff,
		// Taken from the queue rather than from cfg so the engine and the queue
		// cannot end up with two different ideas of how long a claim lasts: the
		// activity worker renews its claim on this number, and the queue grants
		// it on the same one, already defaulted.
		TaskLock: queue.Config().LockDuration,
	}

	pool2 := runtime.NewWorkerPool(engine, cfg.WorkflowWorkers, cfg.ActivityWorkers)

	// The lease manager is built before the engine's TokenFor is wired,
	// because that function reads the manager. Assigning it afterwards is what
	// breaks the cycle, and it is safe because nothing calls TokenFor until a
	// worker or a request arrives, which is after this line.
	// The ring is what actually spreads work across the cluster. Without it
	// every node wants every shard, whichever node boots first acquires all of
	// them, and the other two sit idle holding nothing — which is what this
	// cluster did before it was wired.
	ring := shard.NewRing(cfg.ClusterNodes, shard.DefaultVNodes)
	log.Info(ctx, "hash ring built", "nodes", cfg.ClusterNodes, "vnodes", shard.DefaultVNodes)

	liveness := newPeerLiveness(cfg)

	manager := lease.NewManager(leases, lease.ManagerConfig{
		Owner:  cfg.NodeID,
		Shards: shard.All(int32(cfg.ShardCount)),
		// Wants expresses a preference, not a right.
		//
		// In steady state a node only reaches for the shards the ring assigns
		// it, which is what produces an even split. But a preference alone
		// would break failover: if the preferred owner dies, its leases expire
		// and nobody else believes the shards are theirs, so that slice of the
		// workload stalls forever. So a node will also adopt an orphan — a
		// shard whose lease has been expired for well over the TTL, meaning its
		// owner is not coming back. Waiting two TTLs first stops a brief GC
		// pause or a rolling restart from causing needless shard churn.
		Wants: func(s core.ShardID) bool {
			if ring.Lookup(shard.Key(s)) == cfg.NodeID {
				return true
			}
			l, err := leases.Get(ctx, s)
			if err != nil {
				return false
			}
			return time.Since(l.ExpiresAt) > 2*cfg.LeaseTTL
		},
		// Releases is the handback that makes the ring's preference hold over
		// time. Wants alone is a one-way door: a node that adopted an orphan
		// keeps renewing it forever, so a restarted node owns nothing and, after
		// enough failovers, one node carries the whole cluster. This hands a
		// shard back as soon as its rightful owner is demonstrably alive.
		//
		// Liveness comes from the peer's /healthz rather than from the lease
		// table, because a freshly restarted node holds no leases at all — the
		// lease table cannot tell "alive but empty-handed" from "dead", and that
		// ambiguity is exactly the case this exists to fix.
		Releases: func(s core.ShardID) bool {
			preferred := ring.Lookup(shard.Key(s))
			if preferred == "" || preferred == cfg.NodeID {
				return false
			}
			return liveness.alive(ctx, preferred)
		},
		Policy: lease.Policy{TTL: cfg.LeaseTTL, RenewEvery: cfg.LeaseRenew},
		Tick:   tickFor(cfg.LeaseRenew),
		Clock:  core.SystemClock{},
		// Losing a shard must stop that shard's work immediately. The fencing
		// token would refuse the writes anyway, so this is not what makes the
		// engine correct — it is what stops a node polling a queue that is no
		// longer its business and slowing down the node that owns it.
		OnLost: func(s core.ShardID) {
			pool2.Stop(s)
		},
		OnError: func(s core.ShardID, err error) {
			log.Warn(ctx, "lease operation failed", "shard", int(s), "error", err)
		},
		// Counting transitions here rather than inside OnLost is what lets the
		// four directions be told apart: OnLost fires for a deliberate handback
		// as well as for a lease genuinely taken away, and it never fires at all
		// for an acquire or a renewal.
		Transitions: metrics,
	})
	engine.TokenFor = manager.Token

	// Queue depth is sampled on its own slow loop rather than measured as tasks
	// are polled: the poll is the engine's hottest statement, and a backlog is
	// interesting as a trend, not to the millisecond. It reports only the shards
	// this node owns, because the dashboard sums the gauge across all three
	// nodes and a shard reported twice would read as twice the backlog.
	depth := taskq.NewDepthSampler(queue, taskq.DepthConfig{
		Shards:   func() []core.ShardID { return ownedShards(manager) },
		Observer: metrics,
		OnError: func(err error) {
			log.Warn(ctx, "queue depth sample failed", "error", err)
		},
	})

	scanner := timers.NewScanner(timers.New(pool), queue, timers.ScannerConfig{
		Interval: time.Second,
		Clock:    core.SystemClock{},
		Shards:   func() []core.ShardID { return ownedShards(manager) },
		OnError: func(s core.ShardID, err error) {
			log.Warn(ctx, "timer scan failed", "shard", int(s), "error", err)
		},
	})

	// ---- 5. the HTTP front door -------------------------------------------

	server := api.New(api.Deps{
		Executions: events,
		Events:     events,
		Leases:     leases,
		Starter:    engine,
		Signaller:  engine,
		Canceller:  engine,
		Faults:     &runtime.Faults{Injector: faults},
		NodeID:     cfg.NodeID,
		Shards:     engine,
		// PeerAddr is what turns a rejection into a redirect. Without it a node
		// that does not own a shard answers 409 and the caller has to implement
		// cluster routing itself; with it the node forwards and the caller
		// never knows the request moved.
		//
		// The lease table records only a node name, never an address, so the
		// mapping has to come from configuration. Every node listens on the
		// same port, and in the compose network a node id is also its DNS name,
		// so the name plus that port is the address. A node not in
		// CLUSTER_NODES is unroutable, and forwarding degrades to the 409.
		PeerAddr: func(nodeID string) (string, bool) {
			if nodeID == "" || nodeID == cfg.NodeID {
				return "", false
			}
			for _, n := range cfg.ClusterNodes {
				if n == nodeID {
					return "http://" + nodeID + peerPort(cfg.ListenAddr), true
				}
			}
			return "", false
		},
		OwnedShards: shard.All(int32(cfg.ShardCount)),
		// What /healthz reports. It has to be the lease manager's live answer:
		// OwnedShards above is every shard in the cluster, so serving health
		// from it made all three nodes claim all sixteen shards no matter what
		// they held.
		OwnedNow: func() []core.ShardID { return ownedShards(manager) },
		Gatherer: registry,
		Log:      log,
		Clock:    core.SystemClock{},
	})

	// The API owns its own routes; the dashboard takes whatever is left. Go's
	// ServeMux matches the longest registered pattern, so the explicit API
	// prefixes win and "/" falls through to the embedded page. Serving both
	// from one mux is not cosmetic — the dashboard calls the API with relative
	// URLs, so they have to share an origin or every fetch is a CORS failure.
	root := http.NewServeMux()
	root.Handle("/v1/", server.Handler())
	root.Handle("/healthz", server.Handler())
	root.Handle("/metrics", server.Handler())
	root.Handle("/", webui.Handler())

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info(ctx, "http listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// ---- 6. leases, timers and workers ------------------------------------

	background, cancelBackground := context.WithCancel(context.Background())
	const backgroundLoops = 4
	done := make(chan struct{}, backgroundLoops)

	go func() { _ = manager.Run(background); done <- struct{}{} }()
	go func() { _ = scanner.Run(background); done <- struct{}{} }()
	go func() { _ = depth.Run(background); done <- struct{}{} }()
	go func() { pool2.Supervise(background, manager.Owned, tickFor(cfg.LeaseRenew)); done <- struct{}{} }()

	log.Info(ctx, "engine node ready",
		"shards", cfg.ShardCount,
		"workflows", workflows.Names(),
		"activities", len(activities.Names()),
		"escalation_blocks", cfg.EscalationBlocks)

	// ---- shutdown ----------------------------------------------------------

	select {
	case err := <-serveErr:
		if err != nil {
			cancelBackground()
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		log.Info(context.Background(), "shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	// Stop accepting first. A request that arrives after this point would be
	// starting work this node is about to stop doing.
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn(shutdownCtx, "http shutdown", "error", err)
	}

	// Then drain: every worker finishes the task it is holding and returns.
	pool2.StopAll()

	// Only then give the leases back. Doing it in this order means no shard is
	// handed to another node while this one is still writing to it, and doing
	// it at all means the fleet does not have to wait out a whole TTL to
	// notice that these shards are free.
	cancelBackground()
	for i := 0; i < backgroundLoops; i++ {
		select {
		case <-done:
		case <-shutdownCtx.Done():
			log.Warn(context.Background(), "shutdown timed out waiting for background loops")
			return nil
		}
	}

	log.Info(context.Background(), "engine node stopped cleanly")
	return nil
}

// newPool opens the connection pool and applies the durability setting.
//
// synchronous_commit is set on the connection rather than the server so the
// benchmark can turn it off for this process alone without touching a shared
// database. It defaults to on, because the guarantee this engine exists to
// provide is that a committed decision survives the machine losing power, and
// turning it off trades exactly that away for throughput.
// maxPoolConns bounds each node's connection pool. Three nodes at this size sit
// well inside PostgreSQL's default max_connections of 100.
const maxPoolConns int32 = 25

func newPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if cfg.SyncCommit {
		pcfg.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	} else {
		pcfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	}
	pcfg.ConnConfig.RuntimeParams["application_name"] = "durexec:" + cfg.NodeID

	// Pool sizing, and why it is capped.
	//
	// The obvious sum — one connection per worker goroutine, for every shard —
	// is 16 x (4 + 8) + 8 = 200 per node, or 600 across a three-node cluster.
	// PostgreSQL's default max_connections is 100, so the cluster exhausted the
	// server and every later connection was refused with "sorry, too many
	// clients already". That was observed on a live run, not theorised.
	//
	// The sum is wrong because a worker does not hold a connection for its
	// lifetime — only for the duration of a query. Between polls it holds
	// nothing. A smaller pool therefore does not deadlock; it just makes a
	// burst of workers queue briefly for a connection, which is exactly the
	// back-pressure we want when the database is the bottleneck.
	//
	// PoolMaxConns keeps three nodes comfortably inside a default server, while
	// still leaving room for psql and the monitoring stack. Raise it together
	// with max_connections if the cluster grows.
	needed := int32(cfg.ShardCount*(cfg.WorkflowWorkers+cfg.ActivityWorkers)) + 8
	if needed > maxPoolConns {
		needed = maxPoolConns
	}
	if needed > pcfg.MaxConns {
		pcfg.MaxConns = needed
	}

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// newEmbedder chooses the encoder behind the semantic duplicate check.
//
// A node with no EMBEDDER_URL is a complete, runnable node: it falls back to
// the deterministic stand-in encoder so the whole engine can be started and
// demonstrated with no model server anywhere. What the stand-in cannot do is
// recognise meaning — two differently worded requests hash to unrelated
// vectors — so it exercises the mechanism and its boundaries, and only a real
// sentence encoder demonstrates the contribution. Startup says so out loud.
func newEmbedder(cfg config.Config, log *obs.Logger, ctx context.Context) core.Embedder {
	if cfg.EmbedderURL == "" {
		return semantic.NewFakeEmbedder(semantic.DefaultDim)
	}
	log.Info(ctx, "using http embedder", "endpoint", cfg.EmbedderURL, "model", semantic.DefaultModel)
	return semantic.NewHTTPEmbedder(cfg.EmbedderURL, semantic.DefaultTimeout)
}

// registerActivities puts both demonstration applications' activities into one
// registry. The name and the idempotency mode both come from the application,
// never from a string typed here: the name is what history records, and the
// mode is the application author's statement that their arguments do or do not
// carry meaning.
// intentRenderers maps an activity type to the function that turns its input
// into the one sentence worth embedding. Only semantic-mode activities have
// one; everything else falls back to the checker's generic form.
type intentRenderers map[string]func(json.RawMessage) (string, error)

func registerActivities(r *activity.Registry, faults *faultinj.Injector) (intentRenderers, error) {
	renderers := intentRenderers{}
	deliveryActs := delivery.NewActivities(delivery.Options{Faults: faults})
	for _, reg := range deliveryActs.Registrations() {
		if err := r.Register(activity.Definition{
			Name:         reg.Type,
			Fn:           reg.Handler,
			Mode:         reg.Mode,
			Compensation: deliveryCompensation(reg.Type),
		}); err != nil {
			return nil, err
		}
	}

	// A real model when one is configured, the scripted planner otherwise.
	//
	// The scripted planner cannot evidence the project's claim: it replays the
	// wording we wrote, so any rephrasing a duplicate check then catches is a
	// rephrasing we supplied. With GROQ_API_KEY set the words come from the
	// model instead, which is the situation the contribution is about.
	var planner agent.Planner = demoPlanner{}
	if g := llm.NewGroqPlanner(); g != nil {
		planner = g
	}

	agentActs := agent.NewActivities(agent.Options{
		Planner: planner,
		Faults:  faults,
	})
	for _, reg := range agentActs.Registrations() {
		if err := r.Register(activity.Definition{
			Name: reg.Type,
			Fn:   reg.Handler,
			Mode: reg.Mode,
		}); err != nil {
			return nil, err
		}
		// Without this the checker embeds the whole tool-call envelope instead
		// of the sentence inside it, and the constant JSON scaffolding dilutes
		// every similarity score.
		if reg.IntentText != nil {
			renderers[reg.Type] = reg.IntentText
		}
	}
	return renderers, nil
}

// deliveryCompensation names the activity that undoes a forward step.
//
// The pairing lives here rather than in the application because a compensation
// is run as an ordinary activity through the engine — journaled, retried and
// crash-safe exactly like the forward step — and it is the engine's registry
// that has to know the pairing. The delivery saga already unwinds its own
// stack explicitly, so this is the declaration, not the mechanism.
func deliveryCompensation(activityType string) string {
	switch activityType {
	case delivery.ActivityChargePayment:
		return delivery.ActivityRefundPayment
	case delivery.ActivityReserveInventory:
		return delivery.ActivityReleaseInventory
	case delivery.ActivityNotifyRestaurant:
		return delivery.ActivityCancelRestaurantOrder
	default:
		return ""
	}
}

// demoPlanner stands in for the language model.
//
// A real deployment passes a model-backed planner. This one is a fixed
// three-step script — search, book, done — so the binary can be started and
// driven with nothing else running. It is stateless and derives its answer
// purely from the step number it is given, which matters: the planner is
// called from an activity, the activity's result is journaled, and a planner
// that remembered how many times it had been called would give a different
// answer to a replayed run than to the original one.
type demoPlanner struct{}

func (demoPlanner) Plan(_ context.Context, in agent.PlanInput) (agent.Plan, error) {
	switch in.Step {
	case 0:
		return agent.Plan{
			Step:    in.Step,
			Thought: "I need to find a flight that matches what the user asked for.",
			Tool:    agent.ActivitySearchFlights,
			Args:    in.Goal,
		}, nil
	case 1:
		return agent.Plan{
			Step:    in.Step,
			Thought: "A suitable flight was found. Book it.",
			Tool:    agent.ActivityBookFlight,
			Args:    in.Goal,
		}, nil
	default:
		return agent.Plan{
			Step:    in.Step,
			Thought: "The booking is done, so there is nothing left to do.",
			Done:    true,
			Answer:  "Booked the flight for: " + in.Goal,
		}, nil
	}
}

// ownedShards renders the lease manager's map as the sorted slice the timer
// scanner wants. It is called fresh on every scan, which is the point: a shard
// lost half a second ago must not be scanned again.
func ownedShards(m *lease.Manager) []core.ShardID {
	owned := m.Owned()
	out := make([]core.ShardID, 0, len(owned))
	for s := range owned {
		out = append(out, s)
	}
	return out
}

// tickFor picks a loop interval comfortably below the renewal interval, so a
// renewal is never late by a whole tick.
func tickFor(renew time.Duration) time.Duration {
	t := renew / 3
	if t < 100*time.Millisecond {
		t = 100 * time.Millisecond
	}
	if t > time.Second {
		t = time.Second
	}
	return t
}

// peerPort extracts the ":port" from a listen address so a peer can be reached
// on the same port this node listens on. ":8080" and "0.0.0.0:8080" both yield
// ":8080"; anything unparseable falls back to the default.
func peerPort(listenAddr string) string {
	if i := strings.LastIndex(listenAddr, ":"); i >= 0 {
		if port := listenAddr[i:]; len(port) > 1 {
			return port
		}
	}
	return ":8080"
}

// peerLiveness answers "is this peer up?" for the lease handback, and caches the
// answer briefly.
//
// Both properties are load-bearing. The check runs inside the lease manager's
// loop, which renews every shard this node holds, so a slow answer here delays
// every renewal behind it; a peer that hangs must cost milliseconds, never the
// lease TTL, or this node would lose shards it was never asked to give up. The
// cache means one probe per peer per interval rather than one per shard, so
// sixteen shards and two peers cost two HTTP calls, not sixteen.
type peerLiveness struct {
	cfg    config.Config
	client *http.Client
	ttl    time.Duration

	mu   sync.Mutex
	seen map[string]livenessEntry
}

type livenessEntry struct {
	alive bool
	at    time.Time
}

func newPeerLiveness(cfg config.Config) *peerLiveness {
	return &peerLiveness{
		cfg: cfg,
		// Well under the renew interval (3s by default), so a dead peer can
		// never stall the loop long enough to cost another shard its lease.
		client: &http.Client{Timeout: 400 * time.Millisecond},
		ttl:    2 * time.Second,
		seen:   map[string]livenessEntry{},
	}
}

func (p *peerLiveness) alive(ctx context.Context, node string) bool {
	p.mu.Lock()
	if e, ok := p.seen[node]; ok && time.Since(e.at) < p.ttl {
		p.mu.Unlock()
		return e.alive
	}
	p.mu.Unlock()

	ok := false
	for _, n := range p.cfg.ClusterNodes {
		if n != node {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://"+node+peerPort(p.cfg.ListenAddr)+"/healthz", nil)
		if err == nil {
			if resp, err := p.client.Do(req); err == nil {
				ok = resp.StatusCode == http.StatusOK
				_ = resp.Body.Close()
			}
		}
		break
	}

	p.mu.Lock()
	p.seen[node] = livenessEntry{alive: ok, at: time.Now()}
	p.mu.Unlock()
	return ok
}
