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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

	// The cluster's connection budget is checked against the server's own
	// max_connections before any worker starts. A node that starts anyway and
	// then fails a query with "sorry, too many clients already" at whatever
	// moment the cluster gets busy is both the worst time to find out and the
	// hardest to attribute; the cluster was in fact already capped at three
	// nodes by this, with nothing anywhere saying so.
	if serverMax, serr := serverMaxConnections(ctx, pool); serr != nil {
		log.Warn(ctx, "could not read the server's max_connections, "+
			"so the cluster's connection budget was not checked", "error", serr)
	} else if berr := checkConnBudget(nodeCountFromEnv(), serverMax); berr != nil {
		return berr
	} else {
		log.Info(ctx, "connection budget fits",
			"nodes", nodeCountFromEnv(), "per_node_pool", maxPoolConns,
			"per_node_renewal", renewPoolConns,
			"cluster_total", clusterConnBudget(nodeCountFromEnv()),
			"server_max_connections", serverMax)
	}

	// Claim renewal gets its own small pool. See renewPoolConns.
	renewPool, err := newRenewalPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer renewPool.Close()

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

	// Claim renewal is routed to its own pool, so a saturated main pool
	// cannot make a renewal miss the lock it is renewing. See
	// renewPoolConns and taskq.Queue.renewDB.
	queue := taskq.New(pool, taskq.Config{LockDuration: cfg.TaskLock}).
		WithRenewalDB(renewPool)

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
// The cluster's connection budget.
//
// maxPoolConns is a HARD per-node ceiling. It used to be only a floor-raiser:
// the code read `if needed > pcfg.MaxConns { pcfg.MaxConns = needed }` and
// never lowered anything, while pgxpool defaults MaxConns to
// max(4, runtime.NumCPU()) when the DSN carries no pool_max_conns
// (pgxpool/pool.go:379-383 at v5.10.0, and deploy/docker-compose.yml's
// DATABASE_URL carries none). The compose file also sets no CPU limit, so a
// container sees every core on the host. On a 34-core machine each node would
// therefore have taken 34 connections and three nodes 102, against a
// max_connections of 100 — reinstating the "sorry, too many clients already"
// failure the comment below says was fixed, on nothing but better hardware.
//
// 20 rather than 25 is what makes a fourth node fit. Measured on the live
// three-node cluster: 25+25+25 of engine plus 5 background plus walreceiver
// plus psql = 82 of 100, so a fourth node at 25 reaches 107 and fails. 20 is
// still far above steady-state demand, because a worker holds a connection
// only while a query runs.
const maxPoolConns int32 = 20

// minPoolConns keeps a pool usable even when the arithmetic below asks for
// almost nothing. Below about four, one slow query stalls a whole node.
const minPoolConns int32 = 4

// renewPoolConns is the separate pool that carries lease and claim renewal.
//
// Renewal is the one statement in the engine with a deadline it can miss
// destructively: if it cannot land before the task lock lapses, a completed
// activity's result is thrown away and the work is done again. Sharing the
// main pool means that deadline is at the mercy of however many polls and
// commits are queued ahead of it, so overload destroyed finished work and
// added the load of redoing it. Four is enough for every shard on a node,
// because a renewal is one short UPDATE every TASK_LOCK/3.
const renewPoolConns int32 = 4

// The rest of the budget, stated here rather than left implicit, because the
// failure it prevents is a cluster that starts and then cannot connect.
const (
	// replicationConns is the standby's walreceiver.
	replicationConns int32 = 2
	// monitoringConns covers Prometheus and any exporter.
	monitoringConns int32 = 4
	// migrationConns is the migrator, which runs briefly at startup.
	migrationConns int32 = 2
	// toolingConns covers psql, make verify, and cmd/loadgen during a
	// benchmark. loadgen used bare pgxpool.New, so it took max(4, NumCPU) = 12
	// on the measurement host: 82 + 12 = 94 of 100 while benchmarking, six
	// spare, which is a plausible contributor to the recorded 0.00 wf/s run.
	toolingConns int32 = 18
)

// envNodeCount is how many engine nodes the cluster will run. It is read here
// rather than added to config.Config because it is not a property of this
// node — it is a property of the deployment, and the only thing that uses it is
// the budget check below.
const envNodeCount = "NODE_COUNT"

// defaultNodeCount matches deploy/docker-compose.yml, which runs node-a, -b
// and -c.
const defaultNodeCount int32 = 3

func nodeCountFromEnv() int32 {
	v := os.Getenv(envNodeCount)
	if v == "" {
		return defaultNodeCount
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
		return defaultNodeCount
	}
	return int32(n)
}

// clusterConnBudget reports how many server connections a cluster of nodeCount
// engine nodes needs in the worst case, with everything else that connects.
//
// It is a pure function so it can be tested, which the old arithmetic could
// not be: cmd/engined had no test files at all.
func clusterConnBudget(nodeCount int32) int32 {
	perNode := maxPoolConns + renewPoolConns
	return nodeCount*perNode + replicationConns + monitoringConns +
		migrationConns + toolingConns
}

// poolSize is the per-node pool size.
//
// Separated from newPool and made pure so it can be tested without a database,
// which the old arithmetic could not be.
//
// It deliberately does NOT consult the MaxConns pgxpool.ParseConfig returned.
// That value is indistinguishable between "the operator asked for 12" and
// "the host happens to have 12 cores", because pgxpool fills it in with
// max(4, runtime.NumCPU()) whenever the DSN is silent. Letting it influence
// the answer is exactly how the pool size came to depend on the size of the
// machine. explicitMax is non-zero only when the DSN really did carry
// pool_max_conns, and even then the hard ceiling still applies: an operator
// may ask for a smaller pool, not a larger one than the cluster budget allows.
func poolSize(shardCount, wfWorkers, actWorkers int, explicitMax int32) int32 {
	// The ideal is one connection per worker goroutine per shard, plus a few
	// for the background loops. It is almost always far above the ceiling, and
	// that is fine — see the note in newPool on why a small pool
	// back-pressures rather than deadlocks.
	want := int32(shardCount*(wfWorkers+actWorkers)) + 8
	if explicitMax > 0 {
		want = explicitMax
	}
	if want > maxPoolConns {
		want = maxPoolConns
	}
	if want < minPoolConns {
		want = minPoolConns
	}
	return want
}

// dsnPoolMaxConns reports an explicit pool_max_conns from the DSN, or 0.
//
// pgxpool parses it into Config.MaxConns, but by then it is mixed in with the
// NumCPU default and cannot be told apart from it, so it is read here from the
// text instead.
func dsnPoolMaxConns(dsn string) int32 {
	u, err := url.Parse(dsn)
	if err != nil {
		// Not a URL — a keyword/value DSN. pgxpool handles those; an explicit
		// pool size in one is rare enough to treat as absent rather than
		// hand-parse, and the ceiling still applies either way.
		return 0
	}
	v := u.Query().Get("pool_max_conns")
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n <= 0 {
		return 0
	}
	return int32(n)
}

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
	// maxPoolConns keeps the cluster inside its budget. It is a ceiling in both
	// directions now; see the note on the constant for why only raising it was
	// a defect that better hardware would have reintroduced.
	pcfg.MaxConns = poolSize(cfg.ShardCount, cfg.WorkflowWorkers,
		cfg.ActivityWorkers, dsnPoolMaxConns(cfg.DatabaseURL))

	// An idle connection used to be held forever, which is why a node that was
	// doing nothing still occupied 25 of the server's 100: the pool had needed
	// them once. Releasing them means the budget is consumed by nodes actually
	// working, so an idle cluster has headroom for psql, a benchmark or a
	// migration. It does not replace the ceiling — four busy nodes still have
	// to fit — but it removes the common case where nothing fits.
	pcfg.MaxConnIdleTime = 30 * time.Second
	pcfg.MaxConnLifetime = 30 * time.Minute

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

// newRenewalPool opens the small, separate pool that carries claim renewal.
//
// See renewPoolConns for why renewal must not share the main pool. It is a
// second pool rather than a reserved connection because pgxpool has no
// reservation primitive, and a single dedicated connection would serialise
// every shard's renewals behind one another.
func newRenewalPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if pcfg.ConnConfig.RuntimeParams == nil {
		pcfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Renewal is a single-row UPDATE whose loss costs a completed activity's
	// result, so it is worth the fsync whatever the benchmark asked for.
	pcfg.ConnConfig.RuntimeParams["synchronous_commit"] = "on"
	// A distinct application_name is what makes the budget auditable from
	// pg_stat_activity, which is how the 82-of-100 figure was measured.
	pcfg.ConnConfig.RuntimeParams["application_name"] = "durexec:" + cfg.NodeID + ":renew"
	pcfg.MaxConns = renewPoolConns
	// Renewal must be able to act immediately, so unlike the main pool this
	// one keeps its connections warm. Four idle connections per node is a
	// price the budget above already accounts for.
	pcfg.MinConns = 1
	pcfg.MaxConnLifetime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database for claim renewal: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database for claim renewal: %w", err)
	}
	return pool, nil
}

// checkConnBudget refuses to start when the cluster cannot fit in the server.
//
// It is a startup failure on purpose. The alternative is what happened before:
// a node starts, serves traffic, and then a query fails with "sorry, too many
// clients already" at whatever moment the cluster happens to get busy — which
// is both the worst time to find out and the hardest to attribute. The message
// names every term so the operator can see which one to change.
func checkConnBudget(nodeCount int32, serverMax int32) error {
	if serverMax <= 0 {
		// Nothing to check against. Reported by the caller as a warning
		// rather than a failure: a server we cannot interrogate is not a
		// server we know is too small.
		return nil
	}
	need := clusterConnBudget(nodeCount)
	if need <= serverMax {
		return nil
	}
	return fmt.Errorf(
		"connection budget does not fit: %d nodes x (%d pool + %d renewal) "+
			"+ %d replication + %d monitoring + %d migration + %d tooling = %d, "+
			"but the server's max_connections is %d. "+
			"Raise max_connections to at least %d, or lower NODE_COUNT",
		nodeCount, maxPoolConns, renewPoolConns, replicationConns,
		monitoringConns, migrationConns, toolingConns, need, serverMax, need)
}

// serverMaxConnections reads the server's own limit, so the budget is checked
// against what the database actually allows rather than against an assumption.
func serverMaxConnections(ctx context.Context, pool *pgxpool.Pool) (int32, error) {
	var s string
	if err := pool.QueryRow(ctx, "SHOW max_connections").Scan(&s); err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("max_connections = %q: %w", s, err)
	}
	return int32(n), nil
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
