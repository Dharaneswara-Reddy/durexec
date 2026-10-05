package runtime

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the few collectors that belong to the integration layer rather
// than to any one component.
//
// internal/obs.Metrics already covers everything the engine's own packages
// report, and it is another agent's file, so the counters that only the worker
// loops can produce live here instead. They are registered against the same
// registry, so /metrics still exposes one coherent set.
type Metrics struct {
	// Escalations counts how often the semantic check landed in the uncertain
	// band, labelled with which policy was in force at the time.
	//
	// obs.Metrics already counts verdicts by type, but it cannot say which
	// policy produced them, and the two arms of the experiment differ only in
	// that. Without the label the "executes" and "blocks" runs are one
	// indistinguishable series.
	Escalations *prometheus.CounterVec

	// TaskFailures counts worker-loop failures that were not the workflow's
	// own: a poll that errored, a commit that was rejected, a task released
	// for another try. It is deliberately coarse — the logs carry the detail —
	// and exists so a node that is quietly failing every task does not look
	// the same as an idle one.
	TaskFailures *prometheus.CounterVec
}

// NewMetrics registers the runtime collectors. Like obs.New it takes the
// registry rather than using the default one, so a test can build a fresh
// registry and assert on exact counts.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Escalations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dedup_escalation_total",
			Help: "Semantic dedup checks that fell in the uncertain band, by activity type and escalation policy.",
		}, []string{"activity_type", "policy"}),

		TaskFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "worker_task_failures_total",
			Help: "Worker task attempts that did not commit, by task type and reason.",
		}, []string{"task_type", "reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.Escalations, m.TaskFailures)
	}
	return m
}

// failure reasons, spelled once so the same event cannot appear under two
// names in the same series.
const (
	reasonPoll     = "poll"
	reasonLoad     = "load"
	reasonDispatch = "dispatch"
	reasonCommit   = "commit"
	reasonFenced   = "fenced"
	reasonRetry    = "released_for_retry"
)

// countEscalation is nil-safe so the dedup pass can call it unconditionally.
func (m *Metrics) countEscalation(activityType, policy string) {
	if m == nil {
		return
	}
	m.Escalations.WithLabelValues(activityType, policy).Inc()
}

// countFailure is nil-safe so every worker can call it unconditionally.
func (m *Metrics) countFailure(taskType, reason string) {
	if m == nil {
		return
	}
	m.TaskFailures.WithLabelValues(taskType, reason).Inc()
}
