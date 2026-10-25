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
	// reasonSeqRace is a commit refused by the events primary key while this
	// node still owned the shard: two workers reached the same sequence number
	// and this one lost. Separate from reasonFenced because the two used to be
	// reported as one thing, and a node whose workers keep losing races is a
	// node with a contention problem, not one that keeps being fenced out.
	reasonSeqRace = "seq_race"
	// reasonLockLost is an activity whose task claim lapsed while it was still
	// running, so its result was abandoned rather than committed.
	reasonLockLost = "task_lock_lost"
	// reasonDiverged is a replay whose recorded history and current code
	// disagree about what the run did. The task is PARKED, not failed, so this
	// counter rising is the only automatic signal that a deployment is
	// incompatible with work in flight — it used to be journaled as a terminal
	// workflow failure instead, which lost the information along with the run.
	// A non-zero value here needs an operator: nothing clears it by itself.
	reasonDiverged = "replay_diverged"
	// reasonVersionRefused is a run this binary declines to replay because it
	// was started by a code version it does not accept. Distinct from
	// reasonDiverged: nothing has actually disagreed yet, the binary simply
	// refused to find out.
	reasonVersionRefused = "version_refused"
	// reasonUnregistered is a task whose workflow type this binary cannot run.
	// During a rolling deployment it is expected and transient; sustained, it
	// means a type no binary registers any more, and the task used to be
	// retried for the life of the cluster with no cap and no dead letter.
	reasonUnregistered = "workflow_type_unregistered"
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
