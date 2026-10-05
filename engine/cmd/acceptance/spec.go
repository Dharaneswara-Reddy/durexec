package main

// The requirement catalogue, transcribed verbatim from the two tables in
// docs/DESIGN_DOCUMENT.html: "Functional requirements" (FR-1..FR-16) and
// "Non-functional requirements" (NFR-1..NFR-10).
//
// It is copied into Go rather than parsed out of the HTML at run time on
// purpose. The document is the specification; if somebody edits a target in it
// the matrix must not silently start checking something else, it must fail to
// match this table and be updated deliberately. Short is the abbreviation used
// in the terminal table, and Acceptance is the document's own acceptance
// column, which is the condition a PASS on that row is claiming.

// Requirement is one row of the specification.
type Requirement struct {
	ID    string
	Kind  string // "FR" or "NFR"
	Text  string // the requirement, as written in the design document
	Short string // abbreviated for the terminal table
	// Acceptance is the design document's acceptance condition (FR) or its
	// target plus measurement method (NFR). A PASS means this exact condition
	// was observed.
	Acceptance string
}

// Spec is the full matrix, in report order: FR first, then NFR.
var Spec = []Requirement{
	{
		ID: "FR-1", Kind: "FR",
		Text:       "Persist every workflow state transition as an immutable, ordered event before acting on it.",
		Short:      "Immutable ordered event before acting",
		Acceptance: "History replays byte-identically after a kill.",
	},
	{
		ID: "FR-2", Kind: "FR",
		Text:       "Resume a crashed workflow from its recorded history without re-running completed steps.",
		Short:      "Resume crashed run, no step re-run",
		Acceptance: "Kill mid-workflow; no completed activity runs twice.",
	},
	{
		ID: "FR-3", Kind: "FR",
		Text:       "Retry a failed step with configurable backoff and a non-retryable error class.",
		Short:      "Retry with backoff + non-retryable class",
		Acceptance: "Fail 3x, succeed on 4th; log shows one success.",
	},
	{
		ID: "FR-4", Kind: "FR",
		Text:       "Sleep durably for arbitrary durations without holding a thread or connection.",
		Short:      "Durable sleep, no thread held",
		Acceptance: "A 2-minute sleep survives a full cluster restart.",
	},
	{
		ID: "FR-5", Kind: "FR",
		Text:       "Accept external signals, including human approval, into a running workflow.",
		Short:      "External signals incl. human approval",
		Acceptance: "Signal delivered before the workflow waits is buffered, not lost.",
	},
	{
		ID: "FR-6", Kind: "FR",
		Text:       "Partition workflows across a fixed shard count and assign shards to nodes by consistent hashing.",
		Short:      "Sharding + consistent-hash assignment",
		Acceptance: "16 shards over 3 nodes; gateway routes correctly.",
	},
	{
		ID: "FR-7", Kind: "FR",
		Text:       "Transfer shard ownership automatically when the owning node fails.",
		Short:      "Automatic shard ownership transfer",
		Acceptance: "docker kill the owner; work resumes unattended.",
	},
	{
		ID: "FR-8", Kind: "FR",
		Text:       "Prevent a node that has lost ownership from mutating shard state.",
		Short:      "Fencing: stale owner cannot write",
		Acceptance: "Zero duplicate events across 20 induced failovers.",
	},
	{
		ID: "FR-9", Kind: "FR",
		Text:       "Replicate each shard's log to a follower and promote it on failure.",
		Short:      "Replication + follower promotion",
		Acceptance: "Follower promoted; recovery time recorded p50/p99.",
	},
	{
		ID: "FR-10", Kind: "FR",
		Text:       "Run registered compensations in reverse order when a workflow fails partway.",
		Short:      "Saga compensation in LIFO order",
		Acceptance: "History shows LIFO compensation order.",
	},
	{
		ID: "FR-11", Kind: "FR",
		Text:       "Surface an explicit terminal state when compensation itself exhausts its retries.",
		Short:      "Explicit COMPENSATING_FAILED state",
		Acceptance: "COMPENSATING_FAILED visible in the admin panel.",
	},
	{
		ID: "FR-12", Kind: "FR",
		Text:       "Forward a stable deduplication key to every external call.",
		Short:      "Stable dedup key on every call",
		Acceptance: "Key identical across every crash and replay.",
	},
	{
		ID: "FR-13", Kind: "FR",
		Text:       "Detect a semantically equivalent repeat of an action before it executes, per activity, opt-in.",
		Short:      "Semantic repeat detected before execution",
		Acceptance: "A reproducible double-booking is blocked.",
	},
	{
		ID: "FR-14", Kind: "FR",
		Text:       "Journal every deduplication verdict with its score, threshold and matched call.",
		Short:      "Verdicts journaled with score+threshold",
		Acceptance: "Any skipped action is explainable from the log alone.",
	},
	{
		ID: "FR-15", Kind: "FR",
		Text:       "Expose workflow status and full history without a separate read model.",
		Short:      "Status + history without a read model",
		Acceptance: "Query API returns rendered history.",
	},
	{
		ID: "FR-16", Kind: "FR",
		Text:       "Run two unrelated applications on the same engine binary.",
		Short:      "Two applications, one binary",
		Acceptance: "Food delivery and AI agent, no recompilation.",
	},

	{
		ID: "NFR-1", Kind: "NFR",
		Text:       "Durability of committed work",
		Short:      "Durability of committed work",
		Acceptance: "Zero loss. Compare completed-step counts either side of 20 induced kills.",
	},
	{
		ID: "NFR-2", Kind: "NFR",
		Text:       "Recovery time after node loss",
		Short:      "Recovery time after node loss",
		Acceptance: "p50 < 6 s, p99 < 15 s. Lease expiry 10 s plus replay; distribution over 20 kills.",
	},
	{
		ID: "NFR-3", Kind: "NFR",
		Text:       "Duplicate side effects, exact mode",
		Short:      "Duplicate side effects (exact mode)",
		Acceptance: "0 per 1 000 runs. Count real invocations at a stub external API.",
	},
	{
		ID: "NFR-4", Kind: "NFR",
		Text:       "Shard availability gap during failover",
		Short:      "Availability gap during failover",
		Acceptance: "<= lease TTL (10 s). Affects 1/16 of workflows; stated, not hidden.",
	},
	{
		ID: "NFR-5", Kind: "NFR",
		Text:       "Throughput scaling",
		Short:      "Throughput scaling 1 -> 2 shards",
		Acceptance: ">= 1.7x from 1->2 shards. Completed workflows per second at 1, 2, 4 shards.",
	},
	{
		ID: "NFR-6", Kind: "NFR",
		Text:       "Replay latency",
		Short:      "Replay latency at 200 events",
		Acceptance: "< 50 ms at 200 events. Instrumented timer around history load and replay.",
	},
	{
		ID: "NFR-7", Kind: "NFR",
		Text:       "Added latency of the semantic check",
		Short:      "Semantic check added latency",
		Acceptance: "p99 < 40 ms. Local embedding inference, per-check timer.",
	},
	{
		ID: "NFR-8", Kind: "NFR",
		Text:       "Semantic false-positive rate",
		Short:      "Semantic false-positive rate",
		Acceptance: "< 2% at tau = 0.90. Hand-labelled retry pairs; the asymmetric risk.",
	},
	{
		ID: "NFR-9", Kind: "NFR",
		Text:       "Operational footprint",
		Short:      "Operational footprint",
		Acceptance: "< 3 GB, 1 machine. Full stack under Docker Compose.",
	},
	{
		ID: "NFR-10", Kind: "NFR",
		Text:       "External dependencies",
		Short:      "External dependency count",
		Acceptance: "One. PostgreSQL. No broker, no coordinator, no consensus cluster.",
	},
}

// Event type names and workflow type names this tool reads out of the log.
//
// They are string literals rather than imports of internal/core and app/* so
// that the acceptance tool verifies the wire and database representation the
// examiner can see for themselves, not the Go constants the engine happens to
// compile with. If somebody renames an event type, this tool must notice that
// the log no longer contains what the design document describes, and an import
// would quietly rename along with it.
const (
	evWorkflowStarted   = "WorkflowExecutionStarted"
	evWorkflowCompleted = "WorkflowExecutionCompleted"
	evWorkflowFailed    = "WorkflowExecutionFailed"
	evActivityScheduled = "ActivityTaskScheduled"
	evActivityCompleted = "ActivityTaskCompleted"
	evActivityFailed    = "ActivityTaskFailed"
	evTimerStarted      = "TimerStarted"
	evTimerFired        = "TimerFired"
	evSignalReceived    = "SignalReceived"
	evDedupEvaluated    = "SemanticDedupEvaluated"
	evCompScheduled     = "CompensationScheduled"
	evCompCompleted     = "CompensationCompleted"
	evCompFailed        = "CompensationFailed"
)

const (
	statusRunning            = "RUNNING"
	statusCompleted          = "COMPLETED"
	statusFailed             = "FAILED"
	statusCompensating       = "COMPENSATING"
	statusCompensatingFailed = "COMPENSATING_FAILED"
)

// The two demonstration applications named by FR-16's acceptance condition.
const (
	wfDelivery = "DeliveryOrder"
	wfAgent    = "TravelAgent"
)

// defaultCompensationMap says which forward activity each compensation undoes,
// for the LIFO check in FR-10.
//
// The mapping has to be supplied from outside the log because the log does not
// contain it: history records that CancelRestaurantOrder ran, not that it was
// registered as the undo for NotifyRestaurant. It comes from reading
// app/delivery/workflow.go, and -compensation-map overrides it for another
// application.
var defaultCompensationMap = map[string]string{
	"RefundPayment":         "ChargePayment",
	"ReleaseInventory":      "ReserveInventory",
	"CancelRestaurantOrder": "NotifyRestaurant",
}

// coordinationServices is the denylist behind NFR-10.
//
// NFR-10 claims exactly one external dependency, PostgreSQL, and names the
// three categories it is claiming not to have: a broker, a coordinator, a
// consensus cluster. These are the products that would show up if one had
// crept in — as a metric family exported by the node, as a module in go.mod,
// or as a service in the compose file.
var coordinationServices = []string{
	"zookeeper", "etcd", "consul", "kafka", "rabbitmq", "nats", "pulsar",
	"redis", "memcached", "temporal", "cadence", "raft", "paxos",
	"serf", "chubby", "curator",
}

// isPostgres recognises the one dependency NFR-10 does allow, plus the
// observability and test containers that are not part of the running engine's
// dependency set. Prometheus and Grafana scrape the engine; the engine does
// not call them, and the design document counts dependencies of the engine.
var permittedServices = []string{
	"postgres", "pgvector", "prometheus", "grafana", "embedder", "node-", "migrate",
}
