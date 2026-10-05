package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// ---------------------------------------------------------------------------
// A fake Ctx. No database, no network, no real time.
// ---------------------------------------------------------------------------

type fakeFuture struct {
	value json.RawMessage
	err   error
}

func (f fakeFuture) Get() (json.RawMessage, error) { return f.value, f.err }

// errWaiting stands in for a workflow parking on an unsatisfied signal. A real
// engine suspends the goroutine and writes nothing; the fake reports it as an
// error so a test can prove the workflow got that far and then stopped.
var errWaiting = errors.New("workflow parked: signal not delivered")

type fakeCtx struct {
	calls  []string
	inputs []json.RawMessage
	waited []string
	sleeps []time.Duration

	// plans is served in order to successive Plan activities.
	plans []Plan
	// planCursor is how many plans have been handed out.
	planCursor int

	// signals holds delivered signal payloads; a missing name means the
	// workflow is still waiting.
	signals map[string]json.RawMessage

	// failures maps an activity type to the error it returns.
	failures map[string]error

	clock time.Time
}

func newFakeCtx(plans ...Plan) *fakeCtx {
	return &fakeCtx{
		plans:    plans,
		signals:  map[string]json.RawMessage{},
		failures: map[string]error{},
		clock:    time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC),
	}
}

func (c *fakeCtx) approve(reviewer string) {
	c.signals[SignalHumanApproval], _ = json.Marshal(Approval{Approved: true, Reviewer: reviewer})
}

func (c *fakeCtx) reject(reviewer, note string) {
	c.signals[SignalHumanApproval], _ = json.Marshal(Approval{Reviewer: reviewer, Note: note})
}

func (c *fakeCtx) ExecuteActivity(activityType string, input any) Future {
	c.calls = append(c.calls, activityType)
	raw, err := json.Marshal(input)
	if err != nil {
		return fakeFuture{err: err}
	}
	c.inputs = append(c.inputs, raw)

	if err, ok := c.failures[activityType]; ok {
		return fakeFuture{err: err}
	}

	switch activityType {
	case ActivityPlan:
		if c.planCursor >= len(c.plans) {
			return fakeFuture{err: errors.New("fake: ran out of scripted plans")}
		}
		p := c.plans[c.planCursor]
		c.planCursor++
		return marshalFuture(p)
	case ActivityFinalize:
		return marshalFuture(FinalizeResult{ConfirmationID: "cnf_1", Summary: "done"})
	default:
		return marshalFuture(map[string]string{"tool": activityType, "status": "ok"})
	}
}

func marshalFuture(v any) Future {
	raw, err := json.Marshal(v)
	if err != nil {
		return fakeFuture{err: err}
	}
	return fakeFuture{value: raw}
}

func (c *fakeCtx) Sleep(d time.Duration) Future {
	c.sleeps = append(c.sleeps, d)
	c.clock = c.clock.Add(d)
	return fakeFuture{value: json.RawMessage(`null`)}
}

func (c *fakeCtx) WaitForSignal(name string) Future {
	c.waited = append(c.waited, name)
	payload, ok := c.signals[name]
	if !ok {
		return fakeFuture{err: errWaiting}
	}
	return fakeFuture{value: payload}
}

func (c *fakeCtx) Now() time.Time { return c.clock }

func request() Request {
	return Request{RequestID: "req-1", UserID: "u-1", Goal: DoubleBookingGoal, MaxIterations: 3}
}

// ---------------------------------------------------------------------------

// The approval gate must be reached, and the workflow must stop there until a
// person actually signals. That pause is the point: nothing is confirmed while
// the workflow sits waiting, and it may sit for days across restarts.
func TestWorkflowReachesHumanApprovalAndBlocks(t *testing.T) {
	ctx := newFakeCtx(
		Plan{Thought: "search", Tool: ActivitySearchFlights, Args: "BLR to DEL Friday morning"},
		Plan{Done: true, Answer: "found the 07:40"},
	)
	// No signal is delivered.

	_, err := Run(ctx, request())
	if !errors.Is(err, errWaiting) {
		t.Fatalf("Run returned %v, want the workflow to be parked on the signal", err)
	}

	wantCalls := []string{ActivityPlan, ActivitySearchFlights, ActivityPlan}
	if !reflect.DeepEqual(ctx.calls, wantCalls) {
		t.Fatalf("activities = %v, want %v", ctx.calls, wantCalls)
	}
	if !reflect.DeepEqual(ctx.waited, []string{SignalHumanApproval}) {
		t.Fatalf("waited on %v, want exactly [%s]", ctx.waited, SignalHumanApproval)
	}
	// Nothing may be finalised on the far side of an unopened gate.
	for _, c := range ctx.calls {
		if c == ActivityFinalize {
			t.Fatal("finalised before the human approved")
		}
	}
}

func TestWorkflowRunsPlanToolApprovalFinalize(t *testing.T) {
	ctx := newFakeCtx(
		Plan{Thought: "search first", Tool: ActivitySearchFlights, Args: "BLR to DEL Friday morning"},
		Plan{Thought: "book it", Tool: ActivityBookFlight, Args: "BLR to DEL Friday morning, 6E-2113"},
		Plan{Done: true, Answer: "booked"},
	)
	ctx.approve("ops-1")

	res, err := Run(ctx, request())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantCalls := []string{
		ActivityPlan, ActivitySearchFlights,
		ActivityPlan, ActivityBookFlight,
		ActivityPlan,
		ActivityFinalize,
	}
	if !reflect.DeepEqual(ctx.calls, wantCalls) {
		t.Fatalf("activities =\n  %v\nwant\n  %v", ctx.calls, wantCalls)
	}
	if !res.Approved || res.Reviewer != "ops-1" {
		t.Errorf("approval not recorded: %+v", res)
	}
	if res.ConfirmationID != "cnf_1" {
		t.Errorf("ConfirmationID = %q", res.ConfirmationID)
	}
	if len(res.Transcript) != 2 {
		t.Errorf("transcript has %d turns, want 2", len(res.Transcript))
	}
	if res.BudgetExceeded {
		t.Error("BudgetExceeded set on a run that finished on its own")
	}
}

// A rejection is a legitimate business outcome, not an engine failure, and
// nothing may be finalised after one.
func TestRejectionStopsBeforeFinalize(t *testing.T) {
	ctx := newFakeCtx(
		Plan{Tool: ActivityBookFlight, Args: "BLR to DEL Friday morning"},
		Plan{Done: true, Answer: "booked"},
	)
	ctx.reject("ops-2", "wrong date")

	res, err := Run(ctx, request())
	if err != nil {
		t.Fatalf("a rejection should not be an error: %v", err)
	}
	if res.Approved {
		t.Error("Approved = true after a rejection")
	}
	if res.RejectionNote != "wrong date" {
		t.Errorf("RejectionNote = %q", res.RejectionNote)
	}
	for _, c := range ctx.calls {
		if c == ActivityFinalize {
			t.Fatal("finalised a rejected request")
		}
	}
}

// The loop is bounded. An agent that cannot finish must stop and ask a person
// rather than plan and spend forever.
func TestLoopIsBoundedByMaxIterations(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		wantPlans int
	}{
		{name: "explicit budget of two", max: 2, wantPlans: 2},
		{name: "explicit budget of three", max: 3, wantPlans: 3},
		{name: "zero falls back to the default", max: 0, wantPlans: DefaultMaxIterations},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A planner that never says it is done.
			plans := make([]Plan, 16)
			for i := range plans {
				plans[i] = Plan{Tool: ActivitySearchFlights, Args: "keep looking"}
			}
			ctx := newFakeCtx(plans...)
			ctx.approve("ops-3")

			req := request()
			req.MaxIterations = tc.max

			res, err := Run(ctx, req)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if ctx.planCursor != tc.wantPlans {
				t.Fatalf("made %d plans, want %d", ctx.planCursor, tc.wantPlans)
			}
			if !res.BudgetExceeded {
				t.Error("BudgetExceeded not reported after running out of steps")
			}
			// It still stops at the gate: a person decides what to do next.
			if len(ctx.waited) != 1 {
				t.Fatalf("waited on %v, want the approval gate", ctx.waited)
			}
		})
	}
}

func TestPlannerSeesTheTranscriptSoFar(t *testing.T) {
	ctx := newFakeCtx(
		Plan{Thought: "search", Tool: ActivitySearchFlights, Args: "BLR to DEL"},
		Plan{Done: true, Answer: "ok"},
	)
	ctx.approve("ops-4")

	if _, err := Run(ctx, request()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The second planning call is the fourth activity input.
	var second PlanInput
	if err := json.Unmarshal(ctx.inputs[2], &second); err != nil {
		t.Fatalf("second plan input: %v", err)
	}
	if len(second.Transcript) != 1 {
		t.Fatalf("second plan saw %d turns, want 1", len(second.Transcript))
	}
	if second.Transcript[0].Call.Args != "BLR to DEL" {
		t.Errorf("transcript lost the tool arguments: %+v", second.Transcript[0])
	}
	if second.Step != 1 {
		t.Errorf("second plan Step = %d, want 1", second.Step)
	}
}

func TestActivityFailurePropagates(t *testing.T) {
	boom := errors.New("flight api down")
	ctx := newFakeCtx(Plan{Tool: ActivityBookFlight, Args: "BLR to DEL"})
	ctx.failures[ActivityBookFlight] = boom

	if _, err := Run(ctx, request()); !errors.Is(err, boom) {
		t.Fatalf("Run returned %v, want the tool failure", err)
	}
	for _, c := range ctx.calls {
		if c == ActivityFinalize {
			t.Fatal("finalised after a failed tool call")
		}
	}
}

func TestRunJSONRejectsBadInputWithoutRetrying(t *testing.T) {
	ctx := newFakeCtx()
	_, err := RunJSON(ctx, json.RawMessage(`{"max_iterations": "lots"}`))
	if err == nil {
		t.Fatal("malformed input was accepted")
	}
	if !core.IsNonRetryable(err) {
		t.Fatalf("malformed input produced a retryable error: %v", err)
	}
	if len(ctx.calls) != 0 {
		t.Fatalf("scheduled activities for an undecodable request: %v", ctx.calls)
	}
}

func TestRunJSONRoundTrip(t *testing.T) {
	ctx := newFakeCtx(Plan{Done: true, Answer: "nothing to do"})
	ctx.approve("ops-5")

	in, err := json.Marshal(request())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := RunJSON(ctx, in)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("result is not decodable: %v", err)
	}
	if res.RequestID != "req-1" || !res.Approved {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Registration modes.
// ---------------------------------------------------------------------------

// The mode split is the argument this project makes, so it is asserted
// directly rather than left to a comment.
func TestToolActivitiesAreSemanticAndTheRestAreExact(t *testing.T) {
	want := map[string]core.IdempotencyMode{
		ActivityPlan:          core.ModeExact,
		ActivitySearchFlights: core.ModeSemantic,
		ActivityBookFlight:    core.ModeSemantic,
		ActivityFinalize:      core.ModeExact,
	}

	regs := NewActivities(Options{Planner: NewScriptedPlanner("empty")}).Registrations()
	if len(regs) != len(want) {
		t.Fatalf("registered %d activities, want %d", len(regs), len(want))
	}
	for _, r := range regs {
		w, ok := want[r.Type]
		if !ok {
			t.Fatalf("unexpected activity %q", r.Type)
		}
		if r.Mode != w {
			t.Errorf("%s registered as %s, want %s", r.Type, r.Mode, w)
		}
		// A semantic activity is useless without a sentence to compare, and an
		// exact one has no use for one.
		if r.Mode == core.ModeSemantic && r.IntentText == nil {
			t.Errorf("%s is semantic but supplies no intent text", r.Type)
		}
		if r.Mode == core.ModeExact && r.IntentText != nil {
			t.Errorf("%s is exact but supplies intent text", r.Type)
		}
	}
}

func TestIntentTextIsTheCanonicalisedArgument(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
		fail bool
	}{
		{name: "plain", args: "BLR to DEL Friday morning", want: "blr to del friday morning"},
		{name: "collapses whitespace", args: "  BLR   to \n DEL  ", want: "blr to del"},
		{name: "empty is refused", args: "   ", fail: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(ToolCall{Tool: ActivityBookFlight, Args: tc.args})
			got, err := toolCallIntent(raw)
			if tc.fail {
				if err == nil {
					t.Fatalf("empty arguments were accepted as %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("toolCallIntent: %v", err)
			}
			if got != tc.want {
				t.Fatalf("intent text = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Activities.
// ---------------------------------------------------------------------------

func TestBookFlightRecordsEverySeatItTakes(t *testing.T) {
	acts := NewActivities(Options{Planner: NewScriptedPlanner("none"), Sleep: noLatency})

	for _, args := range []string{IntentBeforeCrash, IntentAfterCrash} {
		raw, _ := json.Marshal(ToolCall{Tool: ActivityBookFlight, Args: args})
		if _, err := acts.BookFlight(context.Background(), raw); err != nil {
			t.Fatalf("BookFlight(%q): %v", args, err)
		}
	}

	bookings := acts.Bookings()
	if len(bookings) != 2 {
		t.Fatalf("recorded %d bookings, want 2", len(bookings))
	}
	if bookings[0].BookingID == bookings[1].BookingID {
		t.Fatal("two bookings share an id")
	}
}

func TestAgentActivitiesRespectTheFaultInjector(t *testing.T) {
	inj := faultinj.New(true)
	inj.FailNext(ActivityBookFlight, 2)
	acts := NewActivities(Options{Planner: NewScriptedPlanner("none"), Faults: inj, Sleep: noLatency})

	raw, _ := json.Marshal(ToolCall{Tool: ActivityBookFlight, Args: IntentBeforeCrash})
	fails := 0
	for n := 0; n < 3; n++ {
		if _, err := acts.BookFlight(context.Background(), raw); err != nil {
			fails++
			if !faultinj.IsInjected(err) {
				t.Fatalf("call %d failed for another reason: %v", n, err)
			}
		}
	}
	if fails != 2 {
		t.Fatalf("failed %d times, want 2", fails)
	}
	// The two failed calls took no seat.
	if got := len(acts.Bookings()); got != 1 {
		t.Fatalf("recorded %d bookings, want 1", got)
	}
}

func TestPlanStepRefusesWithoutAPlanner(t *testing.T) {
	acts := NewActivities(Options{Sleep: noLatency})
	raw, _ := json.Marshal(PlanInput{Goal: "anything"})

	_, err := acts.PlanStep(context.Background(), raw)
	if err == nil {
		t.Fatal("planned with no planner configured")
	}
	if !core.IsNonRetryable(err) {
		t.Fatalf("a missing planner is a configuration error and must not be retried: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ScriptedPlanner.
// ---------------------------------------------------------------------------

func TestScriptedPlannerIsDeterministicWithinAnIncarnation(t *testing.T) {
	makePlanner := func() *ScriptedPlanner {
		return NewScriptedPlanner("p",
			Plan{Tool: ActivitySearchFlights, Args: "a"},
			Plan{Tool: ActivityBookFlight, Args: "b"},
		)
	}

	var first, second []string
	for _, out := range []*[]string{&first, &second} {
		p := makePlanner()
		for i := 0; i < 2; i++ {
			plan, err := p.Plan(context.Background(), PlanInput{Step: i})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			*out = append(*out, plan.Args)
		}
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two fresh planners disagreed: %v vs %v", first, second)
	}
}

func TestScriptedPlannerReportsExhaustion(t *testing.T) {
	p := NewScriptedPlanner("p", Plan{Done: true})
	if _, err := p.Plan(context.Background(), PlanInput{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := p.Plan(context.Background(), PlanInput{})
	if !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("second call returned %v, want ErrScriptExhausted", err)
	}
	if p.Calls() != 1 || p.Remaining() != 0 {
		t.Fatalf("counters wrong: calls=%d remaining=%d", p.Calls(), p.Remaining())
	}
	p.Reset()
	if p.Calls() != 0 || p.Remaining() != 1 {
		t.Fatalf("Reset did not rewind: calls=%d remaining=%d", p.Calls(), p.Remaining())
	}
}

func TestCanonicalIntentDoesNotTranslateWords(t *testing.T) {
	// Normalising case and spacing is safe. Mapping "BLR" onto "Bangalore"
	// would be the hand-written rule this project argues does not scale, so
	// canonicalisation must leave that job to the embedding.
	if CanonicalIntent(IntentBeforeCrash) == CanonicalIntent(IntentAfterCrash) {
		t.Fatal("canonicalisation collapsed two differently-worded intents")
	}
}
