// Package api is the engine's front door: a net/http server over the stdlib
// ServeMux, using Go 1.22 method-and-pattern routes ("POST /v1/workflows").
//
// There is no web framework here, and that is a considered choice rather than
// minimalism for its own sake. The routing table is a dozen fixed paths with
// one path variable; a framework would add a dependency, its own middleware
// model and its own error conventions, in exchange for syntax the standard
// library has had since Go 1.22.
//
// Two things in this package carry real design weight:
//
// The error mapping in writeError. It is the API's contract with every client,
// and every client's retry strategy is derived from it, so it lives in exactly
// one function.
//
// The routing decision in Decide. Whether this node may serve a request is a
// pure function of the shard, this node's name, the lease row and the current
// time, which is what makes it exhaustively testable.
//
// Every response carries X-Shard-Id and X-Owner-Node. During a failover demo
// those two headers are how you watch work move between nodes without reading
// anybody's logs.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/obs"
)

// ---- the interfaces the server depends on ---------------------------------

// StartRequest is what the API hands the engine to open a workflow.
type StartRequest struct {
	ShardID      core.ShardID
	WorkflowID   string
	WorkflowType string
	Input        json.RawMessage
	// IdempotencyKey is the caller's own de-duplication token from the
	// Idempotency-Key header. It is passed through rather than acted on here:
	// the executions table already makes a duplicate start harmless, and the
	// engine may want the key for its own bookkeeping.
	IdempotencyKey string
}

// StartResult is what comes back. AlreadyOpen distinguishes "we opened a new
// run" from "one was already open", which is the difference between a 201 and
// a 200 and nothing more — both are success.
type StartResult struct {
	RunID       core.RunID
	Status      core.ExecutionStatus
	AlreadyOpen bool
}

// Starter opens workflows. It is a local interface rather than a core port
// because starting a workflow is an engine behaviour, not a storage one, and
// the package that implements it is being written by another agent right now.
type Starter interface {
	StartWorkflow(ctx context.Context, req StartRequest) (StartResult, error)
}

// SignalRequest delivers an external event into a running workflow.
type SignalRequest struct {
	ShardID        core.ShardID
	WorkflowID     string
	Name           string
	Payload        json.RawMessage
	IdempotencyKey string
}

// Signaller delivers signals.
type Signaller interface {
	Signal(ctx context.Context, req SignalRequest) error
}

// CancelRequest asks a running workflow to stop, which in a saga means running
// its compensations rather than simply halting.
type CancelRequest struct {
	ShardID        core.ShardID
	WorkflowID     string
	Reason         string
	IdempotencyKey string
}

// Canceller cancels workflows.
type Canceller interface {
	Cancel(ctx context.Context, req CancelRequest) error
}

// FailStepRequest arms a deliberate failure. This is a demo and evaluation
// tool: fault injection has to be triggerable from outside the process,
// because the whole point is to break a node while it is running.
type FailStepRequest struct {
	ShardID      core.ShardID    `json:"-"`
	WorkflowID   string          `json:"workflow_id"`
	ActivityType string          `json:"activity_type"`
	K            int             `json:"k"`
	Attempts     int             `json:"attempts"`
	NonRetryable bool            `json:"non_retryable"`
	Error        string          `json:"error"`
	Extra        json.RawMessage `json:"extra,omitempty"`
}

// FaultInjector arms deliberate failures.
type FaultInjector interface {
	FailStep(ctx context.Context, req FailStepRequest) error
}

// ---- dependencies ---------------------------------------------------------

// Deps is everything the server needs. Storage arrives as the core ports;
// engine behaviour arrives as the small local interfaces above.
//
// The split is what lets this package compile and be tested before any of the
// engine exists: nothing in Deps is a concrete type from another package.
// Every field may be nil, and a route whose dependency is missing answers 501
// rather than panicking — a half-wired node during integration should tell you
// which piece is missing, not crash.
type Deps struct {
	// Storage ports, implemented by internal/eventlog and the lease store.
	Executions core.ExecutionStore
	Events     core.EventStore
	Leases     core.LeaseStore

	// StartObserver counts start requests by outcome. It is a one-method
	// interface rather than a *obs.Metrics so that this package never imports
	// Prometheus and its tests need no registry — the same arrangement that
	// keeps internal/idem/semantic free of a metrics dependency. Nil disables
	// the counting and changes nothing else.
	StartObserver StartObserver

	// Engine behaviour.
	Starter   Starter
	Signaller Signaller
	Canceller Canceller
	Faults    FaultInjector

	// Routing.
	NodeID string
	Shards ShardResolver
	// OwnedShards is the set of shards the collection endpoints read when the
	// request names none. It is a plain slice rather than an interface because
	// it is data, not behaviour, and engined deliberately sets it to EVERY
	// shard: a listing that silently omitted a shard this node happens not to
	// hold would make the dashboard's own contents depend on which node
	// answered. Over-reading is the safe direction there.
	//
	// It is therefore NOT the answer to "what does this node own", and must
	// never be used as one. See OwnedNow.
	OwnedShards []core.ShardID

	// OwnedNow reports the shards this node holds a lease on AT THIS MOMENT.
	// It is a function because the answer changes as leases come and go, which
	// is the whole reason /healthz cannot be served from OwnedShards.
	//
	// This exists because it was missing. /healthz reported len(OwnedShards),
	// i.e. the cluster's shard count, so every node answered
	// {"status":"ok","owned_shards":16} whatever it actually held. A cluster
	// in which four shards had no lease row at all, and half of all start
	// requests were being refused, reported three healthy nodes each owning
	// all sixteen. Nil keeps the old behaviour for callers that have no lease
	// manager, such as the API's own tests.
	OwnedNow func() []core.ShardID

	// ---- forwarding -------------------------------------------------------

	// PeerAddr turns a node name into a base URL such as
	// "http://node-b:8080". It is a function rather than a map because the
	// lease table stores only a node name and has no idea how to reach it:
	// that mapping is a deployment concern, and it differs between a docker
	// compose file, a Kubernetes service and a laptop running three processes
	// on three ports. Returning false means "I cannot reach that node", which
	// produces the ordinary 409 naming the owner — never a panic and never a
	// wait on an address we guessed.
	//
	// A nil PeerAddr disables forwarding entirely, and the node behaves
	// exactly as it did before: it refuses work for shards it does not own.
	PeerAddr func(nodeID string) (string, bool)

	// ForwardTimeout bounds one forwarded hop, including reading the peer's
	// response body. Zero means defaultForwardTimeout.
	ForwardTimeout time.Duration

	// ForwardClient overrides the HTTP client used for forwarding. It exists
	// for tests and for a deployment that needs its own transport, such as one
	// terminating TLS between nodes. A nil client gets a pooled default.
	ForwardClient *http.Client

	// Observability. Gatherer backs /metrics; a nil one falls back to the
	// default registry so a quick manual run still exposes something.
	Gatherer prometheus.Gatherer
	Log      *obs.Logger
	Clock    core.Clock
}

// Server serves the HTTP API for one node.
type Server struct {
	deps Deps
	gw   *Gateway
	mux  *http.ServeMux

	// fwdClient is the default peer client, built once so that connections to
	// the other nodes are pooled across requests.
	fwdClient *http.Client
}

// New builds a server. The gateway is constructed here from the same Deps so
// that routing and handling can never be pointed at two different lease
// stores.
func New(d Deps) *Server {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Log == nil {
		d.Log = obs.NewLoggerFrom(nil)
	}
	s := &Server{
		deps: d,
		gw: &Gateway{
			Self:   d.NodeID,
			Shards: d.Shards,
			Leases: d.Leases,
			Clock:  d.Clock,
		},
		mux:       http.NewServeMux(),
		fwdClient: newForwardClient(),
	}
	s.routes()
	return s
}

// StartObserver records the outcome of one workflow start request.
type StartObserver interface {
	WorkflowStartObserved(workflowType, outcome string)
}

// Gateway exposes the routing gateway, for a caller that wants to make the
// same ownership decision outside an HTTP request.
func (s *Server) Gateway() *Gateway { return s.gw }

// Handler returns the fully wrapped handler, middleware included. Use this
// rather than the Server itself when embedding in another mux.
func (s *Server) Handler() http.Handler {
	return s.withNoStore(s.withRequestID(s.withDefaultRoutingHeaders(s.withBufferedBody(s.mux))))
}

// ServeHTTP makes Server itself an http.Handler, so httptest.NewServer(s)
// works in tests exactly as the real listener does in production.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// routes is the entire routing table, in one place and in one order.
//
// Method-and-pattern routes mean an unmatched method gets a 405 from the mux
// itself rather than falling into a handler that has to check r.Method — one
// fewer thing every handler can forget.
func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/workflows", s.handleStartWorkflow)
	s.mux.HandleFunc("GET /v1/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("GET /v1/workflows/{id}", s.handleGetWorkflow)
	s.mux.HandleFunc("GET /v1/workflows/{id}/history", s.handleGetHistory)
	s.mux.HandleFunc("POST /v1/workflows/{id}/signals", s.handleSignal)
	s.mux.HandleFunc("POST /v1/workflows/{id}/cancel", s.handleCancel)

	s.mux.HandleFunc("GET /v1/dedup/verdicts", s.handleDedupVerdicts)
	s.mux.HandleFunc("POST /v1/admin/fail-step", s.handleFailStep)
	s.mux.HandleFunc("GET /v1/shards", s.handleShards)

	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.Handle("GET /metrics", s.metricsHandler())
}

func (s *Server) metricsHandler() http.Handler {
	g := s.deps.Gatherer
	if g == nil {
		g = prometheus.DefaultGatherer
	}
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{})
}

// ---- middleware -----------------------------------------------------------

// withNoStore forbids caching of every response this server produces.
//
// Without it the engine sends no cache directives at all, and a browser is then
// free to apply its own heuristics: it may reuse an earlier reply to
// GET /v1/workflows without asking the engine again. The dashboard polls those
// endpoints to show live state, so a cached reply is not a stale optimisation
// but a wrong answer — it showed workflows that had already been deleted. The
// dashboard page itself is covered too, so a rebuilt UI is picked up on reload
// rather than needing a forced refresh.
func (s *Server) withNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// withRequestID gives every request an id and echoes it back.
//
// An inbound id is honoured so that a client's own trace id survives into the
// engine's logs; otherwise one is minted. Either way the id goes into the
// context, where the logger picks it up automatically, and into the response
// header, so a caller holding a failed response can find the matching log line
// without knowing anything about our internals.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(obs.RequestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(obs.RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(obs.WithRequestID(r.Context(), id)))
	})
}

// unroutedShard is the X-Shard-Id value for a response that is not about one
// shard — health, metrics, the shard list. A negative number is used rather
// than an empty header so the header is always present and always parses.
const unroutedShard = "-1"

// withDefaultRoutingHeaders makes X-Shard-Id and X-Owner-Node unconditional.
//
// They are set before the handler runs, so a handler that knows better can
// overwrite them and one that does not still emits something. Making them
// unconditional is what turns a failover demo into "watch the header change"
// instead of "correlate three log files".
func (s *Server) withDefaultRoutingHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderShardID, unroutedShard)
		w.Header().Set(HeaderOwnerNode, s.deps.NodeID)
		next.ServeHTTP(w, r)
	})
}

// Header names, exported because the demo client and the tests both assert on
// them and neither should be spelling them by hand.
const (
	HeaderShardID        = "X-Shard-Id"
	HeaderOwnerNode      = "X-Owner-Node"
	HeaderIdempotencyKey = "Idempotency-Key"
)

// setRouting stamps the real shard and owner once routing is known. Called on
// the refusal path as well as the success path: on a 409 the client most wants
// to know who it should have asked.
func setRouting(w http.ResponseWriter, rt Routing, self string) {
	w.Header().Set(HeaderShardID, itoa(int64(rt.ShardID)))
	owner := rt.Owner
	if rt.Local {
		owner = self
	}
	if owner != "" {
		w.Header().Set(HeaderOwnerNode, owner)
	}
}
