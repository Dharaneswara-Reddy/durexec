// Package obs is the engine's observability surface: the Prometheus metrics
// that make the distributed mechanisms visible, and a small structured logger
// that carries a request id through every layer.
//
// The metrics here are not decoration. Several of them exist specifically so
// that a claim in the evaluation can be checked against a running system
// rather than taken on trust — most of all fencing_rejections_total, which is
// the only external evidence that the safety mechanism ever actually fires.
package obs

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Namespace prefixes nothing on purpose: the metric names below are spelled
// out in full so they can be grepped for verbatim in dashboards, in the
// evaluation chapter, and in this source tree.

// Metrics holds every collector the engine reports.
//
// It is a struct of collectors rather than a set of package-level variables,
// and New takes a prometheus.Registerer rather than using the default
// registry, so each test can build a fresh registry and assert on exact
// counter values. Package-level collectors would leak state between tests and
// make "did this increment?" unanswerable.
type Metrics struct {
	WorkflowCompleted *prometheus.CounterVec
	ReplayDuration    prometheus.Histogram
	ShardRecovery     prometheus.Histogram
	LeaseTransitions  *prometheus.CounterVec
	FencingRejections *prometheus.CounterVec
	DedupVerdicts     *prometheus.CounterVec
	DedupLatency      prometheus.Histogram
	TaskQueueDepth    *prometheus.GaugeVec
}

// Lease transition directions, so callers do not invent their own spellings
// and split one time series into three.
const (
	LeaseAcquired = "acquired"
	LeaseRenewed  = "renewed"
	LeaseLost     = "lost"
	LeaseReleased = "released"
)

// New builds and registers every collector against reg.
//
// It uses MustRegister, so a duplicate registration panics at start-up rather
// than leaving the process running with half its metrics missing. Passing a
// fresh prometheus.NewRegistry() is what tests do.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		WorkflowCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "workflow_completed_total",
			Help: "Workflow executions that reached a terminal state, by type and final status.",
		}, []string{"workflow_type", "status"}),

		// Replay is on the critical path of every single workflow task, not
		// just of recovery: the engine rebuilds state from history before
		// every decision. If this histogram grows with history length, the
		// engine needs continue-as-new or snapshots.
		ReplayDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "replay_duration_seconds",
			Help:    "Time to rebuild workflow state by replaying its history.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
		}),

		// Shard recovery is the headline availability number: how long a
		// shard's work is stalled between one node dying and another taking
		// over its lease. Buckets run to minutes because a stuck recovery is
		// exactly the case worth seeing.
		ShardRecovery: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "shard_recovery_seconds",
			Help:    "Time from a shard becoming ownerless to another node owning it.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 14),
		}),

		LeaseTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "lease_transitions_total",
			Help: "Shard lease ownership changes, by shard and direction.",
		}, []string{"shard", "direction"}),

		// This one proves the safety mechanism actually fires. A fenced-out
		// node believes it still owns a shard and tries to write; the token
		// check inside the transaction refuses it. Without this counter, a
		// run in which no split brain ever happened is indistinguishable from
		// one in which fencing was silently broken the whole time.
		FencingRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fencing_rejections_total",
			Help: "Writes rejected because the writer's fencing token was stale.",
		}, []string{"shard"}),

		DedupVerdicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dedup_verdict_total",
			Help: "Semantic dedup verdicts, by activity type and verdict.",
		}, []string{"activity_type", "verdict"}),

		// Dedup latency sits on the workflow path, so it is added directly to
		// the time to make one decision. Its upper buckets are what justify
		// the embedding timeout being as short as it is.
		DedupLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "dedup_latency_seconds",
			Help:    "Wall time of one semantic dedup check, including the embedding call.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		}),

		TaskQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "task_queue_depth",
			Help: "Currently visible tasks waiting in a shard's queue, by task type.",
		}, []string{"shard", "task_type"}),
	}

	reg.MustRegister(
		m.WorkflowCompleted,
		m.ReplayDuration,
		m.ShardRecovery,
		m.LeaseTransitions,
		m.FencingRejections,
		m.DedupVerdicts,
		m.DedupLatency,
		m.TaskQueueDepth,
	)
	return m
}

// The helpers below exist so that callers never spell a label value by hand.
// Every label here is a core type, and converting it in one place is what
// keeps "COMPLETED" from appearing alongside "Completed" in the same series.

// WorkflowFinished records a workflow reaching a terminal state.
func (m *Metrics) WorkflowFinished(workflowType string, status core.ExecutionStatus) {
	m.WorkflowCompleted.WithLabelValues(workflowType, string(status)).Inc()
}

// ObserveReplay records how long rebuilding state from history took.
func (m *Metrics) ObserveReplay(seconds float64) { m.ReplayDuration.Observe(seconds) }

// ObserveShardRecovery records how long a shard was without an owner.
func (m *Metrics) ObserveShardRecovery(seconds float64) { m.ShardRecovery.Observe(seconds) }

// LeaseTransition records one ownership change. Direction should be one of the
// Lease* constants.
func (m *Metrics) LeaseTransition(shard core.ShardID, direction string) {
	m.LeaseTransitions.WithLabelValues(shardLabel(shard), direction).Inc()
}

// FencingRejection records that a stale writer was refused.
func (m *Metrics) FencingRejection(shard core.ShardID) {
	m.FencingRejections.WithLabelValues(shardLabel(shard)).Inc()
}

// ObserveDedup records one semantic dedup verdict and how long it took.
//
// This signature is the whole of what internal/idem/semantic depends on: that
// package declares a one-method Observer interface and this method satisfies
// it, so the semantic checker never imports Prometheus and stays unit-testable
// with no registry at all.
func (m *Metrics) ObserveDedup(activityType string, verdict core.DedupVerdict, seconds float64) {
	m.DedupVerdicts.WithLabelValues(activityType, string(verdict)).Inc()
	m.DedupLatency.Observe(seconds)
}

// SetQueueDepth reports the current depth of one shard's queue.
func (m *Metrics) SetQueueDepth(shard core.ShardID, taskType core.TaskType, depth float64) {
	m.TaskQueueDepth.WithLabelValues(shardLabel(shard), string(taskType)).Set(depth)
}

func shardLabel(s core.ShardID) string {
	// Small ids are the overwhelmingly common case (a cluster has tens of
	// shards, not millions), so a lookup table avoids an allocation per
	// observation on the hot path.
	if s >= 0 && int(s) < len(smallShardLabels) {
		return smallShardLabels[s]
	}
	return itoa(int64(s))
}

var smallShardLabels = buildShardLabels(256)

func buildShardLabels(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = itoa(int64(i))
	}
	return out
}
