package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// The gate, checked against the project's own recorded numbers.
// ---------------------------------------------------------------------------

// TestGateAgreesWithTheRecordedJudgements is the test that makes the gate worth
// having. The published runs were judged by hand — scaling-c16 was correctly
// quarantined as generator-bound, and the audit later found the 4-shard points
// of scaling-saturated.txt have the same signature. Encoding those judgements
// here means the next run cannot quietly disagree with them.
//
// The numbers are verbatim from docs/results/scaling-saturated.txt and
// docs/results/scaling-c16-GENERATOR-BOUND.csv, with the ~2.01s service floor
// derived from the workload in app/delivery/activities.go.
func TestGateAgreesWithTheRecordedJudgements(t *testing.T) {
	const floor = 2010 * time.Millisecond

	// floorOf builds the minimum result the gate needs: a clean generator
	// (nothing dropped, no lag) and a p50, so only the service-floor test can
	// fire. That isolates the one property these recorded points fail.
	clean := func(p50 time.Duration, completed int64, window time.Duration) arrivalResult {
		return arrivalResult{
			Rate:         float64(completed) / window.Seconds(),
			ServiceFloor: floor,
			Scheduled:    completed,
			Issued:       completed,
			Dropped:      0,
			Completed:    completed,
			Window:       window,
			Latency:      Summary{N: int(completed), P50: p50},
		}
	}

	cases := []struct {
		name     string
		res      arrivalResult
		want     Verdict
		becauseo string
	}{
		{
			// c=128, 1 shard: 12.80 wf/s, p50 9.8039s. Nearly 8s above the
			// floor — a real queue, a real measurement.
			name: "recorded c128 1 shard is saturated",
			res:  clean(9803900*time.Microsecond, 256, 20*time.Second),
			want: VerdictSaturated,
		},
		{
			// c=128, 2 shards: 23.15 wf/s, p50 5.4579s. Also well above.
			name: "recorded c128 2 shards is saturated",
			res:  clean(5457900*time.Microsecond, 463, 20*time.Second),
			want: VerdictSaturated,
		},
		{
			// c=128, 4 shards: 46.30 wf/s, p50 2.0152s — ON the floor. The
			// engine had drained its queue. This is the point whose being
			// treated as a measurement makes the 2x-to-4-shards claim fail.
			name: "recorded c128 4 shards is NOT saturated",
			res:  clean(2015200*time.Microsecond, 926, 20*time.Second),
			want: VerdictNotSaturated,
		},
		{
			// c=64, 4 shards: 32.10 wf/s, p50 2.0081s — on the floor, and
			// 64/2.0081 = 31.87, within 0.7% of the recorded figure.
			name: "recorded c64 4 shards is NOT saturated",
			res:  clean(2008100*time.Microsecond, 642, 20*time.Second),
			want: VerdictNotSaturated,
		},
		{
			// The already-quarantined c=16 file: p50 2.0070s and 2.0079s.
			name: "quarantined c16 2 shards is NOT saturated",
			res:  clean(2007900*time.Microsecond, 174, 20*time.Second),
			want: VerdictNotSaturated,
		},
		{
			name: "quarantined c16 4 shards is NOT saturated",
			res:  clean(2007000*time.Microsecond, 170, 20*time.Second),
			want: VerdictNotSaturated,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := c.res.classify()
			if got != c.want {
				t.Fatalf("verdict %s, want %s — %s", got, c.want, why)
			}
			t.Logf("%s: %s", got, why)
		})
	}
}

// TestGateCatchesAGeneratorThatCannotKeepItsSchedule covers the open loop's own
// failure mode. A generator that drops arrivals or starts them late is offering
// a lower rate than it claims, and reporting the result as capacity would be
// the same mistake as the closed loop in a new costume.
func TestGateCatchesAGeneratorThatCannotKeepItsSchedule(t *testing.T) {
	base := arrivalResult{
		Rate: 10, ServiceFloor: time.Second, Window: 10 * time.Second,
		Scheduled: 100, Issued: 100, Completed: 100,
		Latency: Summary{N: 100, P50: 5 * time.Second},
	}
	if got, why := base.classify(); got != VerdictSaturated {
		t.Fatalf("the baseline should be saturated, got %s: %s", got, why)
	}

	dropped := base
	dropped.Dropped = 5
	dropped.Issued = 95
	got, why := dropped.classify()
	if got != VerdictGeneratorBound {
		t.Fatalf("5%% of arrivals dropped should be GENERATOR_BOUND, got %s: %s", got, why)
	}
	if !strings.Contains(why, "dropped") {
		t.Fatalf("the reason must name the drops: %s", why)
	}

	// One drop in 100 is at the limit, not over it.
	atLimit := base
	atLimit.Dropped = 1
	atLimit.Issued = 99
	if got, why := atLimit.classify(); got != VerdictSaturated {
		t.Fatalf("1%% dropped is at the limit and should still pass, got %s: %s", got, why)
	}

	// Lag: the gap at 10/s is 100ms, so the limit is 25ms.
	late := base
	late.LagP99 = 60 * time.Millisecond
	got, why = late.classify()
	if got != VerdictGeneratorBound {
		t.Fatalf("p99 lag of 60ms against a 100ms gap should be GENERATOR_BOUND, got %s: %s", got, why)
	}
	if !strings.Contains(why, "lag") {
		t.Fatalf("the reason must name the lag: %s", why)
	}
}

// TestGateCatchesAnOverloadedEngine checks the third refusal: a queue still
// growing when the window closed brackets capacity from above but is not a
// capacity figure.
func TestGateCatchesAnOverloadedEngine(t *testing.T) {
	res := arrivalResult{
		Rate: 100, ServiceFloor: time.Second, Window: 10 * time.Second,
		Scheduled: 1000, Issued: 1000, Completed: 400, TimedOut: 600,
		InFlightSlope: 42.5,
		Latency:       Summary{N: 400, P50: 8 * time.Second},
	}
	got, why := res.classify()
	if got != VerdictOverloaded {
		t.Fatalf("a growing queue with unfinished work should be OVERLOADED, got %s: %s", got, why)
	}
	if !strings.Contains(why, "above capacity") {
		t.Fatalf("the reason must say the rate is above capacity: %s", why)
	}
}

// TestGateRefusesToRunWithoutADeclaredFloor is the rule that keeps the gate
// honest: the floor is a property of the workload and has to be stated before
// the run, not chosen afterwards to suit the result.
func TestGateRefusesToRunWithoutADeclaredFloor(t *testing.T) {
	cfg := config{rates: []float64{10}}
	_, err := runArrivalMode(context.Background(), nil, cfg, &strings.Builder{})
	if err == nil {
		t.Fatal("-mode arrival must refuse to run with no -service-floor")
	}
	if !strings.Contains(err.Error(), "service-floor") {
		t.Fatalf("the refusal must name the flag: %v", err)
	}

	cfg = config{serviceFloor: 2 * time.Second}
	if _, err := runArrivalMode(context.Background(), nil, cfg, &strings.Builder{}); err == nil {
		t.Fatal("-mode arrival must refuse to run with no -rates")
	}
}

// ---------------------------------------------------------------------------
// In-flight slope.
// ---------------------------------------------------------------------------

func TestInFlightSlopeDistinguishesLevelFromGrowing(t *testing.T) {
	at := make([]float64, 20)
	level := make([]int64, 20)
	growing := make([]int64, 20)
	for i := range at {
		at[i] = float64(i) * 0.1
		// Level but noisy, as a system at capacity actually looks.
		level[i] = int64(50 + (i%3 - 1))
		growing[i] = int64(50 + 10*i)
	}

	_, _, slope := inFlightStats(level, at)
	if slope < -5 || slope > 5 {
		t.Fatalf("a level series gave a slope of %.2f/s, want about 0", slope)
	}
	p50, max, slope := inFlightStats(growing, at)
	if slope < 50 {
		t.Fatalf("a series growing 10 per 100ms gave a slope of %.2f/s, want about 100", slope)
	}
	if max != 240 {
		t.Fatalf("max in flight = %d, want 240", max)
	}
	if p50 < 100 || p50 > 160 {
		t.Fatalf("median in flight = %d, want about 140", p50)
	}
	// Too few points to fit: must report 0 rather than a meaningless number.
	if _, _, s := inFlightStats([]int64{1, 2}, []float64{0, 1}); s != 0 {
		t.Fatalf("two points gave a slope of %.2f, want 0", s)
	}
}

func TestParseFloats(t *testing.T) {
	got, err := parseFloats("5,10.5, 20 ,40")
	if err != nil {
		t.Fatalf("parseFloats: %v", err)
	}
	want := []float64{5, 10.5, 20, 40}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "abc", "5,abc"} {
		if _, err := parseFloats(bad); err == nil {
			t.Fatalf("parseFloats(%q) should have failed", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Validation against a workload whose capacity is known exactly.
// ---------------------------------------------------------------------------

// knownWorkload is a stub engine with a service time and a concurrency limit
// chosen by the test, so its true capacity is limit/serviceTime — an exact
// number, not an estimate.
//
// This is what stops the P1.8 mistake recurring. A generator that has never
// been pointed at a workload whose capacity is known is an unvalidated
// instrument, and the closed-loop generator's defect was exactly that nobody
// had checked it could find a saturation point at all.
type knownWorkload struct {
	serviceTime time.Duration
	limit       int

	mu      sync.Mutex
	running int

	sem chan struct{}
}

func newKnownWorkload(serviceTime time.Duration, limit int) *knownWorkload {
	return &knownWorkload{
		serviceTime: serviceTime,
		limit:       limit,
		sem:         make(chan struct{}, limit),
	}
}

// capacity is the exact throughput this workload can sustain.
func (k *knownWorkload) capacity() float64 {
	return float64(k.limit) / k.serviceTime.Seconds()
}

// handler serves the two endpoints driveOne uses. A start is admitted
// immediately and the work is done by a bounded pool, so the stub behaves like
// a server with `limit` workers: offered more than limit/serviceTime, its
// queue grows without bound, which is exactly the behaviour an open loop must
// be able to detect.
func (k *knownWorkload) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	done := map[string]bool{}
	var mu sync.Mutex

	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/workflows"):
			id := idFromStartBody(r)
			if id == "" {
				// Fail loudly rather than accept a start the status route can
				// never match; a stub that cannot be completed would report
				// zero capacity and look like an engine defect.
				t.Errorf("start request carried no workflow_id")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			go func() {
				k.sem <- struct{}{}
				time.Sleep(k.serviceTime)
				<-k.sem
				mu.Lock()
				done[id] = true
				mu.Unlock()
			}()
			started(w, id, 0)
		case r.Method == http.MethodGet:
			id := lastPathSegment(r.URL.Path)
			mu.Lock()
			finished := done[id]
			mu.Unlock()
			status := "RUNNING"
			if finished {
				status = "COMPLETED"
			}
			writeTestJSON(w, http.StatusOK, map[string]any{
				"workflow_id": id, "run_id": "11111111-1111-1111-1111-111111111111",
				"shard_id": 0, "status": status, "last_seq": 1,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestOpenLoopFindsAKnownCapacity is the validation run. Below the stub's true
// capacity the gate must say NOT_SATURATED and completions must track the
// offered rate; above it, completions must plateau at the known capacity and
// the gate must stop calling it a measurement.
//
// A CLOSED loop cannot produce this result: at any concurrency its throughput
// is concurrency/latency, so it reports a number that moves with its own
// configuration rather than plateauing at the server's limit.
func TestOpenLoopFindsAKnownCapacity(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive; runs for a few seconds")
	}
	// 100ms of service across 4 concurrent slots = exactly 40/s.
	wl := newKnownWorkload(100*time.Millisecond, 4)
	node := newFakeNode(t, "node-a", wl.handler(t))
	client := newTestClient(node)

	cfg := config{
		shardCount:     1,
		shards:         1,
		duration:       4 * time.Second,
		warmup:         500 * time.Millisecond,
		grace:          2 * time.Second,
		poll:           20 * time.Millisecond,
		workflowType:   "DeliveryOrder",
		idPrefix:       "validate",
		prep:           0,
		serviceFloor:   100 * time.Millisecond,
		arrivalWorkers: 512,
	}

	type point struct {
		rate      float64
		completed float64
		verdict   Verdict
	}
	var points []point

	// Below capacity, at capacity, and well past it.
	for _, rate := range []float64{10, 20, 120} {
		gen, err := NewIDGen(fmt.Sprintf("validate-r%.0f", rate), 1, []core.ShardID{0})
		if err != nil {
			t.Fatalf("idgen: %v", err)
		}
		res := driveArrivals(context.Background(), client, gen, cfg, rate,
			cfg.duration-cfg.warmup)
		v, why := res.classify()
		t.Logf("offered %.0f/s: completed %.1f/s, p50 %s, in-flight slope %+.2f, "+
			"dropped %d, lag p99 %s -> %s (%s)",
			rate, res.CompletedPerSecond(), fmtDur(res.Latency.P50),
			res.InFlightSlope, res.Dropped, fmtDur(res.LagP99), v, why)
		points = append(points, point{rate, res.CompletedPerSecond(), v})

		// Accounting must balance however the point turned out: every
		// scheduled arrival is either issued or dropped.
		if res.Issued+res.Dropped != res.Scheduled {
			t.Fatalf("accounting does not balance: %d issued + %d dropped != %d scheduled",
				res.Issued, res.Dropped, res.Scheduled)
		}
	}

	cap := wl.capacity()

	// Below capacity: completions track the offered rate, and the gate must
	// NOT call it a capacity measurement.
	for _, p := range points[:2] {
		if p.completed < p.rate*0.6 {
			t.Errorf("offered %.0f/s but only completed %.1f/s, well below a capacity "+
				"of %.0f/s: the generator could not offer the rate", p.rate, p.completed, cap)
		}
		if p.verdict == VerdictSaturated {
			t.Errorf("offered %.0f/s against a capacity of %.0f/s and the gate said "+
				"SATURATED; it must not report capacity from an unsaturated point",
				p.rate, cap)
		}
	}

	// Past capacity: completions must plateau near the known capacity, not
	// follow the offered rate.
	over := points[2]
	if over.completed > cap*1.6 {
		t.Errorf("offered %.0f/s and measured %.1f/s against a true capacity of %.0f/s: "+
			"the measurement is above what the workload can do",
			over.rate, over.completed, cap)
	}
	if over.completed < cap*0.4 {
		t.Errorf("offered %.0f/s and measured only %.1f/s against a true capacity of "+
			"%.0f/s: the generator, not the workload, was the limit",
			over.rate, over.completed, cap)
	}
	if over.verdict == VerdictNotSaturated {
		t.Errorf("offered %.0f/s, three times the %.0f/s capacity, and the gate still "+
			"said NOT_SATURATED", over.rate, cap)
	}
}

// idFromStartBody pulls the workflow id out of a start request.
//
// Client.StartWorkflow POSTs to /v1/workflows with the id in the JSON body,
// and Client.GetWorkflow reads /v1/workflows/{id}, so the stub has to decode
// the body to connect the two. Guessing it from the path yields "workflows"
// for every start, which makes every status read miss and reports zero
// completions at every rate — a stub that silently measures nothing.
func idFromStartBody(r *http.Request) string {
	var body struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkflowID == "" {
		return ""
	}
	return body.WorkflowID
}

func lastPathSegment(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
