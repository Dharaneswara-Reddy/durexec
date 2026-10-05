package obs

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/dharaneswar339/durexec/internal/core"
)

// counterValue reads one labelled counter back out of a registry. Going
// through Gather rather than the collector directly is deliberate: it proves
// the metric is actually registered and would appear on /metrics, which is the
// thing that breaks in practice.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m, labels) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
		t.Fatalf("metric %s has no series with labels %v", name, labels)
	}
	t.Fatalf("metric %s was not registered", name)
	return 0
}

func histogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	t.Fatalf("histogram %s was not registered", name)
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestNewRegistersEveryCollector is the guard against a metric being defined
// but never registered, which is invisible until a dashboard is empty.
func TestNewRegistersEveryCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	// Every collector needs at least one observation before it appears in a
	// gather, so touch them all.
	m.WorkflowFinished("BookingWorkflow", core.StatusCompleted)
	m.ObserveReplay(0.01)
	m.ObserveShardRecovery(1.5)
	m.LeaseTransition(3, LeaseAcquired)
	m.FencingRejection(3)
	m.ObserveDedup("search_flights", core.VerdictDuplicate, 0.02)
	m.SetQueueDepth(3, core.TaskWorkflow, 7)

	want := []string{
		"workflow_completed_total",
		"replay_duration_seconds",
		"shard_recovery_seconds",
		"lease_transitions_total",
		"fencing_rejections_total",
		"dedup_verdict_total",
		"dedup_latency_seconds",
		"task_queue_depth",
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, f := range families {
		present[f.GetName()] = true
	}
	for _, name := range want {
		if !present[name] {
			t.Errorf("metric %s is missing from the registry", name)
		}
	}
}

// TestFreshRegistryIsolatesTests is why New takes a Registerer: two registries
// must not see each other's counts.
func TestFreshRegistryIsolatesTests(t *testing.T) {
	regA, regB := prometheus.NewRegistry(), prometheus.NewRegistry()
	a, b := New(regA), New(regB)

	a.FencingRejection(1)
	a.FencingRejection(1)
	b.FencingRejection(1)

	if got := counterValue(t, regA, "fencing_rejections_total", map[string]string{"shard": "1"}); got != 2 {
		t.Fatalf("registry A = %v, want 2", got)
	}
	if got := counterValue(t, regB, "fencing_rejections_total", map[string]string{"shard": "1"}); got != 1 {
		t.Fatalf("registry B = %v, want 1", got)
	}
}

func TestCounterIncrements(t *testing.T) {
	tests := []struct {
		name   string
		emit   func(*Metrics)
		metric string
		labels map[string]string
		want   float64
	}{
		{
			name:   "workflow completed",
			emit:   func(m *Metrics) { m.WorkflowFinished("BookingWorkflow", core.StatusCompleted) },
			metric: "workflow_completed_total",
			labels: map[string]string{"workflow_type": "BookingWorkflow", "status": "COMPLETED"},
			want:   1,
		},
		{
			name: "workflow failed is a separate series",
			emit: func(m *Metrics) {
				m.WorkflowFinished("BookingWorkflow", core.StatusCompleted)
				m.WorkflowFinished("BookingWorkflow", core.StatusFailed)
				m.WorkflowFinished("BookingWorkflow", core.StatusFailed)
			},
			metric: "workflow_completed_total",
			labels: map[string]string{"workflow_type": "BookingWorkflow", "status": "FAILED"},
			want:   2,
		},
		{
			name: "lease transitions by direction",
			emit: func(m *Metrics) {
				m.LeaseTransition(2, LeaseAcquired)
				m.LeaseTransition(2, LeaseLost)
				m.LeaseTransition(2, LeaseLost)
			},
			metric: "lease_transitions_total",
			labels: map[string]string{"shard": "2", "direction": "lost"},
			want:   2,
		},
		{
			name: "fencing rejections prove the safety mechanism fired",
			emit: func(m *Metrics) {
				m.FencingRejection(5)
				m.FencingRejection(5)
				m.FencingRejection(6)
			},
			metric: "fencing_rejections_total",
			labels: map[string]string{"shard": "5"},
			want:   2,
		},
		{
			name: "dedup verdicts by activity and verdict",
			emit: func(m *Metrics) {
				m.ObserveDedup("search_flights", core.VerdictDuplicate, 0.01)
				m.ObserveDedup("search_flights", core.VerdictDuplicate, 0.01)
				m.ObserveDedup("search_flights", core.VerdictEscalate, 0.01)
				m.ObserveDedup("charge_payment", core.VerdictDistinct, 0.01)
			},
			metric: "dedup_verdict_total",
			labels: map[string]string{"activity_type": "search_flights", "verdict": "DUPLICATE"},
			want:   2,
		},
		{
			name: "escalate is its own series",
			emit: func(m *Metrics) {
				m.ObserveDedup("search_flights", core.VerdictEscalate, 0.01)
			},
			metric: "dedup_verdict_total",
			labels: map[string]string{"activity_type": "search_flights", "verdict": "ESCALATE"},
			want:   1,
		},
		{
			name:   "queue depth is a gauge, last value wins",
			emit:   func(m *Metrics) { m.SetQueueDepth(1, core.TaskActivity, 3); m.SetQueueDepth(1, core.TaskActivity, 9) },
			metric: "task_queue_depth",
			labels: map[string]string{"shard": "1", "task_type": "ACTIVITY"},
			want:   9,
		},
		{
			name:   "a large shard id still labels correctly",
			emit:   func(m *Metrics) { m.FencingRejection(4096) },
			metric: "fencing_rejections_total",
			labels: map[string]string{"shard": "4096"},
			want:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			m := New(reg)
			tc.emit(m)
			if got := counterValue(t, reg, tc.metric, tc.labels); got != tc.want {
				t.Fatalf("%s%v = %v, want %v", tc.metric, tc.labels, got, tc.want)
			}
		})
	}
}

func TestHistogramsObserve(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.ObserveReplay(0.005)
	m.ObserveReplay(0.010)
	m.ObserveShardRecovery(2)
	m.ObserveDedup("search_flights", core.VerdictDistinct, 0.03)

	if got := histogramCount(t, reg, "replay_duration_seconds"); got != 2 {
		t.Fatalf("replay_duration_seconds count = %d, want 2", got)
	}
	if got := histogramCount(t, reg, "shard_recovery_seconds"); got != 1 {
		t.Fatalf("shard_recovery_seconds count = %d, want 1", got)
	}
	// ObserveDedup has to move both the counter and the histogram: a verdict
	// with no latency recorded would make the workflow-path cost invisible.
	if got := histogramCount(t, reg, "dedup_latency_seconds"); got != 1 {
		t.Fatalf("dedup_latency_seconds count = %d, want 1", got)
	}
}

// TestMetricsSatisfiesSemanticObserver is a compile-time-shaped check that the
// semantic package can keep using Metrics through its one-method interface
// without importing Prometheus.
func TestMetricsSatisfiesSemanticObserver(t *testing.T) {
	var _ interface {
		ObserveDedup(string, core.DedupVerdict, float64)
	} = New(prometheus.NewRegistry())
}

// ---- logging --------------------------------------------------------------

func TestRequestIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := RequestID(ctx); got != "" {
		t.Fatalf("a bare context should carry no request id, got %q", got)
	}
	ctx = WithRequestID(ctx, "abc123")
	if got := RequestID(ctx); got != "abc123" {
		t.Fatalf("RequestID = %q, want abc123", got)
	}
}

func TestWithRequestIDIgnoresEmpty(t *testing.T) {
	ctx := WithRequestID(context.Background(), "")
	if got := RequestID(ctx); got != "" {
		t.Fatalf("an empty id should not be stored, got %q", got)
	}
}

// TestLoggerDoesNotPanic covers the wrapper surface. What it is logging is not
// interesting; that every helper survives a nil-ish setup is.
func TestLoggerDoesNotPanic(t *testing.T) {
	lg := NewLoggerFrom(nil).
		With("component", "test").
		WithShard(core.ShardID(3)).
		WithWorkflow("trip-42", core.RunID("run-1"))

	ctx := WithRequestID(context.Background(), "req-1")
	lg.Debug(ctx, "debug")
	lg.Info(ctx, "info", "k", 1)
	lg.Warn(ctx, "warn")
	lg.Error(ctx, "error", "err", "boom")
	if lg.Slog() == nil {
		t.Fatal("Slog returned nil")
	}
}

func TestShardLabel(t *testing.T) {
	for _, tc := range []struct {
		in   core.ShardID
		want string
	}{{0, "0"}, {15, "15"}, {255, "255"}, {256, "256"}, {99999, "99999"}, {-1, "-1"}} {
		if got := shardLabel(tc.in); got != tc.want {
			t.Fatalf("shardLabel(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRequestIDHeaderName(t *testing.T) {
	// The API echoes this header; a typo here would break correlation silently.
	if !strings.EqualFold(RequestIDHeader, "X-Request-Id") {
		t.Fatalf("RequestIDHeader = %q", RequestIDHeader)
	}
}
