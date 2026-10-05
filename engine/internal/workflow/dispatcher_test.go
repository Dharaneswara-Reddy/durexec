package workflow

import (
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// threeActivities is the canonical shape of workflow code: three calls that
// look sequential and blocking, and are in fact three separate executions of
// this same function, each one further along than the last.
func threeActivities(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
	if _, err := ctx.ExecuteActivity("Reserve", nil).Get(); err != nil {
		return nil, err
	}
	if _, err := ctx.ExecuteActivity("Charge", nil).Get(); err != nil {
		return nil, err
	}
	out, err := ctx.ExecuteActivity("Ship", nil).Get()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TestSequentialActivitiesOneCommandPerTask walks the whole life of a workflow
// four tasks at a time. Each task must produce exactly one command, because the
// workflow cannot know what to do next until the previous activity's result is
// in history.
func TestSequentialActivitiesOneCommandPerTask(t *testing.T) {
	d := dispatcherFor(t, threeActivities)

	// Task 1: nothing but the start event. The workflow reaches its first
	// ExecuteActivity in NEW MODE, emits one command, and suspends.
	h := newHistory(t)
	res := run(t, d, h.done())
	assertCommands(t, res, "ScheduleActivity k=0 Reserve")
	if res.Status != core.StatusRunning || !res.Blocking {
		t.Fatalf("expected a suspended workflow, got status %q blocking=%v", res.Status, res.Blocking)
	}

	// Task 2: k=0 is now in history. The first call replays from the record and
	// emits nothing; the second is new.
	h.activityDone(0, "Reserve", `{"ok":true}`)
	res = run(t, d, h.done())
	assertCommands(t, res, "ScheduleActivity k=1 Charge")

	// Task 3.
	h.activityDone(1, "Charge", `{"txn":"abc"}`)
	res = run(t, d, h.done())
	assertCommands(t, res, "ScheduleActivity k=2 Ship")

	// Task 4: everything replays, the workflow returns, and the only command is
	// the terminal one.
	h.activityDone(2, "Ship", `{"tracking":"XYZ"}`)
	res = run(t, d, h.done())
	assertCommands(t, res, `CompleteWorkflow {"tracking":"XYZ"}`)
	if res.Status != core.StatusCompleted {
		t.Fatalf("status = %q, want COMPLETED", res.Status)
	}
	if res.Blocking {
		t.Fatal("a returning workflow must not be reported as blocking")
	}
	if string(res.Output) != `{"tracking":"XYZ"}` {
		t.Fatalf("output = %s", res.Output)
	}
}

// TestReplayResolvesRecordedActivity is the same idea stated on its own,
// because it is the single most important property in the engine: a completed
// activity is answered from the log and is never scheduled a second time.
func TestReplayResolvesRecordedActivity(t *testing.T) {
	var reserveResult json.RawMessage
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		var err error
		reserveResult, err = ctx.ExecuteActivity("Reserve", nil).Get()
		if err != nil {
			return nil, err
		}
		_, err = ctx.ExecuteActivity("Charge", reserveResult).Get()
		return nil, err
	})

	h := newHistory(t).activityDone(0, "Reserve", `{"hold":"h1"}`)
	res := run(t, d, h.done())

	assertCommands(t, res, "ScheduleActivity k=1 Charge")
	if string(reserveResult) != `{"hold":"h1"}` {
		t.Fatalf("replayed result = %s, want the recorded one", reserveResult)
	}
	// The recorded result is forwarded byte-for-byte as the next activity's
	// input; nothing is re-marshalled on the way through.
	if got := string(res.Commands[0].Input); got != `{"hold":"h1"}` {
		t.Fatalf("forwarded input = %s", got)
	}
}

// TestScheduledButUnfinishedEmitsNoCommand covers the state that would
// otherwise cause double execution: the activity is out on the queue, the
// result has not landed, and the workflow is re-executed anyway (a duplicate
// task, a lease that expired while the activity was slow). Replay must block
// again and schedule nothing.
func TestScheduledButUnfinishedEmitsNoCommand(t *testing.T) {
	d := dispatcherFor(t, threeActivities)

	h := newHistory(t).activityScheduled(0, "Reserve") // scheduled, never completed
	res := run(t, d, h.done())

	if len(res.Commands) != 0 {
		t.Fatalf("expected no commands, got %v", commandSummary(res.Commands))
	}
	if res.Status != core.StatusRunning || !res.Blocking {
		t.Fatalf("expected the workflow to suspend again, got %q", res.Status)
	}
}

// TestNonDeterminismDetected is the divergence case. History records that the
// second call was to "Charge"; the code now asks for "Cancel". The engine has
// two accounts of what this run did and no way to decide between them, so it
// refuses rather than repairing.
func TestNonDeterminismDetected(t *testing.T) {
	// A branch that depends on something outside the workflow's input is
	// exactly how this happens in real life — here, a deployed code change.
	branching := func(second string) Fn {
		return func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
			if _, err := ctx.ExecuteActivity("Reserve", nil).Get(); err != nil {
				return nil, err
			}
			if _, err := ctx.ExecuteActivity(second, nil).Get(); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}

	h := newHistory(t).
		activityDone(0, "Reserve", "null").
		activityScheduled(1, "Charge")

	d := dispatcherFor(t, branching("Cancel"))
	_, err := d.Execute(h.done(), "test", nil)

	var nd *core.NonDeterminismError
	if !errors.As(err, &nd) {
		t.Fatalf("expected *core.NonDeterminismError, got %v", err)
	}
	if nd.K != 1 {
		t.Fatalf("K = %d, want 1 — the divergence is at the second blocking call", nd.K)
	}
	if nd.Recorded != "activity:Charge" || nd.Requested != "activity:Cancel" {
		t.Fatalf("recorded=%q requested=%q", nd.Recorded, nd.Requested)
	}
	// The scheduling event for k=1 is the third event in the history.
	if nd.Seq != 4 {
		t.Fatalf("Seq = %d, want the seq of the recorded scheduling event (4)", nd.Seq)
	}

	// The same history against the code that wrote it must replay cleanly. This
	// is what proves the check is detecting divergence and not just fiddling.
	if _, err := dispatcherFor(t, branching("Charge")).Execute(h.done(), "test", nil); err != nil {
		t.Fatalf("matching code must replay without error, got %v", err)
	}
}

// TestNonDeterminismAcrossKinds catches the subtler version: the same k, a
// different kind of call. Without a signature check the sleep's recorded answer
// would be handed to an activity.
func TestNonDeterminismAcrossKinds(t *testing.T) {
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		_, err := ctx.ExecuteActivity("Charge", nil).Get()
		return nil, err
	})

	h := newHistory(t).timerStarted(0, time.Hour).timerFired(0)
	_, err := d.Execute(h.done(), "test", nil)

	var nd *core.NonDeterminismError
	if !errors.As(err, &nd) {
		t.Fatalf("expected *core.NonDeterminismError, got %v", err)
	}
	if nd.K != 0 || nd.Recorded != sigTimer || nd.Requested != "activity:Charge" {
		t.Fatalf("unexpected divergence: %+v", nd)
	}
}

func TestSleepReplay(t *testing.T) {
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		if _, err := ctx.Sleep(24 * time.Hour).Get(); err != nil {
			return nil, err
		}
		_, err := ctx.ExecuteActivity("SendReminder", nil).Get()
		return nil, err
	})

	// New mode: one timer command, nothing else.
	h := newHistory(t)
	res := run(t, d, h.done())
	assertCommands(t, res, "StartTimer k=0 24h0m0s")

	// The timer exists but has not fired. The workflow must suspend again with
	// no command, or it would create a second timer row.
	h.timerStarted(0, 24*time.Hour)
	res = run(t, d, h.done())
	if len(res.Commands) != 0 {
		t.Fatalf("a pending timer must not be restarted, got %v", commandSummary(res.Commands))
	}

	// Fired: the sleep resolves and the workflow moves on.
	h.timerFired(0)
	res = run(t, d, h.done())
	assertCommands(t, res, "ScheduleActivity k=1 SendReminder")
}

func TestWaitForSignalReplay(t *testing.T) {
	var received json.RawMessage
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		var err error
		received, err = ctx.WaitForSignal("approval").Get()
		if err != nil {
			return nil, err
		}
		_, err = ctx.ExecuteActivity("Ship", nil).Get()
		return nil, err
	})

	h := newHistory(t)
	res := run(t, d, h.done())
	assertCommands(t, res, "WaitSignal k=0 approval")

	// A signal for a different name must not satisfy the wait.
	h.signal("cancellation", `{"by":"ops"}`)
	res = run(t, d, h.done())
	assertCommands(t, res, "WaitSignal k=0 approval")

	h.signal("approval", `{"by":"alice"}`)
	res = run(t, d, h.done())
	assertCommands(t, res, "ScheduleActivity k=1 Ship")
	if string(received) != `{"by":"alice"}` {
		t.Fatalf("signal payload = %s", received)
	}
}

// TestWaitForSignalConsumesInOrder proves two waits on the same name see the
// two deliveries in the order they arrived, rather than both seeing the first.
func TestWaitForSignalConsumesInOrder(t *testing.T) {
	var got []string
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		got = nil
		for i := 0; i < 2; i++ {
			v, err := ctx.WaitForSignal("tick").Get()
			if err != nil {
				return nil, err
			}
			got = append(got, string(v))
		}
		return nil, nil
	})

	h := newHistory(t).signal("tick", `1`).signal("tick", `2`)
	res := run(t, d, h.done())

	if len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("signals consumed as %v, want [1 2]", got)
	}
	if res.Status != core.StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
}

// TestSideEffectNotReinvokedOnReplay is the property that makes SideEffect
// worth having: the non-deterministic snippet runs once in the life of the run,
// and every later execution reads the recorded value.
func TestSideEffectNotReinvokedOnReplay(t *testing.T) {
	calls := 0
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		v, err := ctx.SideEffect(func() any {
			calls++
			return "id-from-first-run"
		}).Get()
		if err != nil {
			return nil, err
		}
		_, err = ctx.ExecuteActivity("Charge", v).Get()
		return nil, err
	})

	// First execution: fn runs, the value is recorded as a command, and the
	// workflow carries straight on in the same task — a side effect does not
	// cost a round trip.
	h := newHistory(t)
	res := run(t, d, h.done())
	assertCommands(t, res,
		`RecordSideEffect k=0 "id-from-first-run"`,
		"ScheduleActivity k=1 Charge",
	)
	if calls != 1 {
		t.Fatalf("fn called %d times on the first execution, want 1", calls)
	}

	// Second execution against the recorded value: fn must NOT run again.
	h.sideEffect(0, `"id-from-first-run"`).activityDone(1, "Charge", "null")
	res = run(t, d, h.done())
	if calls != 1 {
		t.Fatalf("fn called %d times in total; replay must reuse the recorded value", calls)
	}
	if res.Status != core.StatusCompleted {
		t.Fatalf("status = %q", res.Status)
	}
}

// TestNowIsDerivedFromHistory pins the workflow clock to the log. Two
// executions of the same run must see the same instant, which is impossible if
// Now is time.Now.
func TestNowIsDerivedFromHistory(t *testing.T) {
	var seen []time.Time
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		seen = nil
		seen = append(seen, ctx.Now())
		if _, err := ctx.ExecuteActivity("Reserve", nil).Get(); err != nil {
			return nil, err
		}
		seen = append(seen, ctx.Now())
		return nil, nil
	})

	h := newHistory(t).activityDone(0, "Reserve", "null")
	run(t, d, h.done())

	// Event 0 is the start, 1 is the scheduling, 2 is the completion.
	if !seen[0].Equal(at(0)) {
		t.Fatalf("Now before any replayed event = %v, want the start event's time %v", seen[0], at(0))
	}
	if !seen[1].Equal(at(2)) {
		t.Fatalf("Now after replaying k=0 = %v, want the completion event's time %v", seen[1], at(2))
	}

	// Run it a second time against the identical history: identical answers.
	first := append([]time.Time(nil), seen...)
	run(t, d, h.done())
	if !seen[0].Equal(first[0]) || !seen[1].Equal(first[1]) {
		t.Fatal("Now is not stable across two executions of the same history")
	}
}

// TestActivityFailureReachesWorkflow checks that a recorded failure comes back
// as an error the workflow can branch on, with the non-retryable bit intact.
func TestActivityFailureReachesWorkflow(t *testing.T) {
	var caught error
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		_, caught = ctx.ExecuteActivity("Charge", nil).Get()
		return nil, caught
	})

	h := newHistory(t).
		activityScheduled(0, "Charge").
		activityFailed(0, "card declined", true)
	res := run(t, d, h.done())

	if caught == nil {
		t.Fatal("expected the recorded failure to surface as an error")
	}
	if !core.IsNonRetryable(caught) {
		t.Fatalf("the non-retryable bit did not survive the trip through history: %v", caught)
	}
	var ae *ActivityError
	if !errors.As(caught, &ae) || ae.ActivityType != "Charge" || ae.K != 0 {
		t.Fatalf("unexpected error shape: %#v", caught)
	}
	if res.Status != core.StatusFailed {
		t.Fatalf("status = %q, want FAILED", res.Status)
	}
	if len(res.Commands) != 1 || res.Commands[0].Type != core.CmdFailWorkflow {
		t.Fatalf("expected a single FailWorkflow command, got %v", commandSummary(res.Commands))
	}
}

// TestCompensationFailureIsItsOwnStatus: a rollback that could not be completed
// is worse than an ordinary failure and must not be filed as one.
func TestCompensationFailureIsItsOwnStatus(t *testing.T) {
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		return nil, &core.CompensationFailedError{
			WorkflowID: testWFID,
			Activity:   "RefundPayment",
			Attempts:   5,
			Cause:      errors.New("gateway unreachable"),
		}
	})

	res := run(t, d, newHistory(t).done())
	if res.Status != core.StatusCompensatingFailed {
		t.Fatalf("status = %q, want COMPENSATING_FAILED", res.Status)
	}
	if len(res.Commands) != 1 || res.Commands[0].Type != core.CmdFailWorkflow {
		t.Fatalf("commands = %v", commandSummary(res.Commands))
	}
}

// TestPanicBecomesFailure: workflow code runs on a goroutine we own, shared with
// every other workflow on the worker. A panic must fail that one run, not the
// process.
func TestPanicBecomesFailure(t *testing.T) {
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		var m map[string]string
		m["boom"] = "yes" // nil map write
		return nil, nil
	})

	res := run(t, d, newHistory(t).done())
	if res.Status != core.StatusFailed {
		t.Fatalf("status = %q, want FAILED", res.Status)
	}
	if res.Err == "" {
		t.Fatal("expected the panic to be reported in Err")
	}
}

// TestTerminalHistoryProducesNoCommands: a task for a finished run happens when
// a worker crashes between committing the completion and deleting its task row.
// It must be a no-op, not a second completion.
func TestTerminalHistoryProducesNoCommands(t *testing.T) {
	calls := 0
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		calls++
		return nil, nil
	})

	h := newHistory(t).add(core.EvWorkflowCompleted, core.WorkflowCompleted{
		Result: json.RawMessage(`{"done":true}`),
	})
	res := run(t, d, h.done())

	if calls != 0 {
		t.Fatal("a finished run must not be re-executed at all")
	}
	if len(res.Commands) != 0 {
		t.Fatalf("commands = %v", commandSummary(res.Commands))
	}
	if res.Status != core.StatusCompleted || string(res.Output) != `{"done":true}` {
		t.Fatalf("status=%q output=%s", res.Status, res.Output)
	}
}

func TestUnknownWorkflowType(t *testing.T) {
	d := NewDispatcher(NewRegistry())
	_, err := d.Execute(newHistory(t).done(), "nope", nil)
	var nr *NotRegisteredError
	if !errors.As(err, &nr) || nr.WorkflowType != "nope" {
		t.Fatalf("expected *NotRegisteredError, got %v", err)
	}
}

func TestCorruptHistoryIsRejected(t *testing.T) {
	d := dispatcherFor(t, threeActivities)
	events := newHistory(t).done()
	events = append(events, core.Event{
		Seq: 2, Type: core.EvActivityScheduled, Payload: json.RawMessage(`{{{`),
	})
	if _, err := d.Execute(events, "test", nil); err == nil {
		t.Fatal("expected an error for an undecodable payload")
	}
}

// TestModeForStampsIdempotencyMode: the dispatcher does not know about the
// activity registry, so the mode arrives through this hook.
func TestModeForStampsIdempotencyMode(t *testing.T) {
	d := dispatcherFor(t, threeActivities)
	d.ModeFor = func(activityType string) core.IdempotencyMode {
		if activityType == "Reserve" {
			return core.ModeSemantic
		}
		return core.ModeExact
	}

	res := run(t, d, newHistory(t).done())
	if res.Commands[0].Mode != core.ModeSemantic {
		t.Fatalf("mode = %q, want Semantic", res.Commands[0].Mode)
	}

	// Without the hook everything is Exact, which is the safe default.
	plain := dispatcherFor(t, threeActivities)
	res = run(t, plain, newHistory(t).done())
	if res.Commands[0].Mode != core.ModeExact {
		t.Fatalf("mode = %q, want Exact", res.Commands[0].Mode)
	}
}

// TestSuspendedWorkflowLeavesNoGoroutine proves the teardown half of the
// handshake. A workflow that blocks is parked on a channel receive forever; if
// destroy did not unwind it, every sleeping workflow on the worker would cost a
// goroutine for as long as the process lived.
func TestSuspendedWorkflowLeavesNoGoroutine(t *testing.T) {
	d := dispatcherFor(t, threeActivities)

	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		res := run(t, d, newHistory(t).done())
		if !res.Blocking {
			t.Fatal("expected the workflow to suspend")
		}
	}
	after := runtime.NumGoroutine()

	// destroy() waits for the goroutine to have actually returned before
	// Execute returns, so this is an exact comparison and not a heuristic.
	if after > before {
		t.Fatalf("leaked goroutines: %d before, %d after", before, after)
	}
}

// TestDeferredCleanupRunsOnTeardown: unwinding a suspended workflow must run
// its defers, otherwise a workflow could not release anything it had set up.
func TestDeferredCleanupRunsOnTeardown(t *testing.T) {
	unwound := make(chan struct{}, 1)
	d := dispatcherFor(t, func(ctx Context, _ json.RawMessage) (json.RawMessage, error) {
		defer func() { unwound <- struct{}{} }()
		_, err := ctx.ExecuteActivity("Reserve", nil).Get()
		return nil, err
	})

	run(t, d, newHistory(t).done())

	select {
	case <-unwound:
	default:
		t.Fatal("the workflow's defer did not run when the coroutine was destroyed")
	}
}

func assertCommands(t *testing.T, res Result, want ...string) {
	t.Helper()
	got := commandSummary(res.Commands)
	if !equalStrings(got, want) {
		t.Fatalf("commands =\n  %v\nwant\n  %v", got, want)
	}
}
