package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// This file is a small, self-contained stand-in for the engine: an event log
// held in a slice and a workflow context that replays it. It exists so the
// double-booking fixture can be driven — in a unit test, in CI, in the demo's
// dry-run mode — without PostgreSQL, a network, or a model.
//
// It is not a second implementation of the engine and makes no attempt to be.
// It reproduces exactly the one behaviour the fixture depends on: an activity
// whose result was journaled is replayed rather than re-executed, and an
// activity whose result was never journaled has to run for real again.

// ErrSimulatedCrash is returned by the incarnation that the fixture kills. It
// stands in for the process simply vanishing.
var ErrSimulatedCrash = errors.New("simulated crash")

// ErrNeverReachedCrash means the first incarnation finished, or ran out of
// steps, without ever performing the tool call whose repeat the scenario is
// about — so there was no crash to recover from and no repeat to look for.
//
// It is a sentinel rather than a plain error because with a live planner it is
// an ordinary thing for the model to do, and a caller measuring the model has
// to be able to count it separately from a genuine malfunction. With a
// scripted planner it cannot happen, and if it does the script has drifted
// from the scenario.
var ErrNeverReachedCrash = errors.New("the crashing incarnation never reached the crash point")

// journalEntry is one completed activity, the way the events table would hold
// it: what ran, and what it returned.
type journalEntry struct {
	ActivityType string
	Result       json.RawMessage
}

// executeFn runs one activity for real.
type executeFn func(ctx context.Context, activityType string, input json.RawMessage) (json.RawMessage, error)

// crashFn decides whether the process dies rather than completing this
// activity. liveIndex counts, from one, how many times this activity type has
// been executed for real in this incarnation.
type crashFn func(activityType string, liveIndex int) bool

// replayCtx is an in-memory workflow context.
type replayCtx struct {
	// journal is the history this incarnation inherited.
	journal []journalEntry
	cursor  int

	execute executeFn
	crash   crashFn

	liveByType map[string]int
	signals    map[string]json.RawMessage

	clock time.Time

	// Journal is the history this incarnation leaves behind, ready to be
	// inherited by the next one.
	Journal []journalEntry

	// ToolCalls is every tool invocation the workflow issued, whether it was
	// replayed or executed. Replayed calls are marked.
	ToolCalls []RecordedCall

	Replayed int
	Live     int
}

// RecordedCall is one tool invocation observed by the simulator.
type RecordedCall struct {
	Call ToolCall
	// Replayed is true when the result came from the log rather than the tool.
	// A replayed call took no seat and charged no card.
	Replayed bool
}

func newReplayCtx(journal []journalEntry, exec executeFn, crash crashFn) *replayCtx {
	inherited := make([]journalEntry, len(journal))
	copy(inherited, journal)
	return &replayCtx{
		journal:    inherited,
		execute:    exec,
		crash:      crash,
		liveByType: map[string]int{},
		signals:    map[string]json.RawMessage{},
		clock:      time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC),
		Journal:    append([]journalEntry(nil), inherited...),
	}
}

func (c *replayCtx) ExecuteActivity(activityType string, input any) Future {
	raw, err := json.Marshal(input)
	if err != nil {
		return simFuture{err: err}
	}

	// Which activities count as "tool calls" is defined by exclusion rather
	// than by a list of names, because the scenario may be driven with tools
	// the simulator has never heard of — a payment, a refund, a message. Plan
	// and Finalize are the only two activities the workflow schedules itself;
	// everything else is something the planner chose to do to the world, and
	// is therefore exactly what has to be recorded.
	isTool := activityType != ActivityPlan && activityType != ActivityFinalize
	record := func(replayed bool) {
		if !isTool {
			return
		}
		var call ToolCall
		if err := json.Unmarshal(raw, &call); err == nil {
			c.ToolCalls = append(c.ToolCalls, RecordedCall{Call: call, Replayed: replayed})
		}
	}

	// Replay: the result is already a durable fact, so the tool must not run.
	if c.cursor < len(c.journal) {
		entry := c.journal[c.cursor]
		c.cursor++
		if entry.ActivityType != activityType {
			// The engine cannot know which version of the code told the truth,
			// so it refuses to continue rather than guess.
			return simFuture{err: &core.NonDeterminismError{
				K:         c.cursor - 1,
				Recorded:  entry.ActivityType,
				Requested: activityType,
			}}
		}
		c.Replayed++
		record(true)
		return simFuture{value: entry.Result}
	}

	// New work.
	c.liveByType[activityType]++
	if c.crash != nil && c.crash(activityType, c.liveByType[activityType]) {
		// The process dies here. Nothing is journaled, so as far as the log is
		// concerned this activity never happened.
		return simFuture{err: fmt.Errorf("%w: during %s", ErrSimulatedCrash, activityType)}
	}

	result, err := c.execute(context.Background(), activityType, raw)
	if err != nil {
		return simFuture{err: err}
	}
	c.Live++
	record(false)
	c.Journal = append(c.Journal, journalEntry{ActivityType: activityType, Result: result})
	return simFuture{value: result}
}

func (c *replayCtx) Sleep(d time.Duration) Future {
	c.clock = c.clock.Add(d)
	return simFuture{value: json.RawMessage(`null`)}
}

func (c *replayCtx) WaitForSignal(name string) Future {
	payload, ok := c.signals[name]
	if !ok {
		// A real workflow parks here until the signal arrives. The simulator
		// has no scheduler to park in, so it says so plainly.
		return simFuture{err: fmt.Errorf("simulator: workflow is waiting for signal %q", name)}
	}
	return simFuture{value: payload}
}

func (c *replayCtx) Now() time.Time { return c.clock }

type simFuture struct {
	value json.RawMessage
	err   error
}

func (f simFuture) Get() (json.RawMessage, error) { return f.value, f.err }

// dispatch turns an Activities set into an executeFn.
func dispatch(a *Activities) executeFn {
	handlers := map[string]func(context.Context, json.RawMessage) (json.RawMessage, error){}
	for _, r := range a.Registrations() {
		handlers[r.Type] = r.Handler
	}
	return func(ctx context.Context, activityType string, input json.RawMessage) (json.RawMessage, error) {
		h, ok := handlers[activityType]
		if !ok {
			return nil, &core.NonRetryableError{
				Cause: fmt.Errorf("simulator: no activity registered as %q", activityType),
			}
		}
		return h(ctx, input)
	}
}

// noLatency spends no time at all, so a simulated run is instant.
func noLatency(context.Context, time.Duration) error { return nil }

// lastCall finds the most recent invocation of one tool, replayed or not.
//
// Replayed calls are included deliberately: the question it answers is "what
// did this incarnation ask for", and an incarnation that replayed a booking
// asked for that booking as surely as one that executed it.
func lastCall(calls []RecordedCall, tool string) (ToolCall, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Call.Tool == tool {
			return calls[i].Call, true
		}
	}
	return ToolCall{}, false
}

// firstCall finds the earliest invocation of one tool in a plain list.
func firstCall(calls []ToolCall, tool string) (ToolCall, bool) {
	for _, c := range calls {
		if c.Tool == tool {
			return c, true
		}
	}
	return ToolCall{}, false
}

// Simulate runs the double-booking scenario end to end with no database.
//
// It runs the workflow twice. The first incarnation books a flight and is then
// killed while its next planning call is in flight. The second inherits the
// first's journal, replays what was durable, and re-plans what was lost — and
// the re-planned booking is worded differently.
//
// The returned Trace is what the evaluation asserts on. bookings is every seat
// actually taken across both incarnations: without semantic deduplication
// there are two, which is the bug.
func (f *DoubleBooking) Simulate() (Trace, []Booking, error) {
	var trace Trace

	// A tool that shadows one of the workflow's own activities would disable
	// planning or finalising and then look like a model failure, so it is
	// refused here rather than discovered three screens of output later.
	for _, t := range f.Tools {
		switch t.Type {
		case "":
			return trace, nil, fmt.Errorf("fixture: a supplied tool has no Type")
		case ActivityPlan, ActivityFinalize, ActivitySearchFlights, ActivityBookFlight:
			return trace, nil, fmt.Errorf(
				"fixture: supplied tool %q collides with one of the agent's own activities", t.Type)
		}
	}

	// watched is the tool whose repeat the scenario is about. Every decision
	// below — where the crash fires, which call is "before" and which is
	// "after" — is about this one tool, so it is resolved once.
	watched := f.CrashAfterTool
	if watched == "" {
		watched = ActivityBookFlight
	}

	// Incarnation one: plans, books, then dies before the next plan lands.
	//
	// The crash predicate reads the context it is about to kill, which is why
	// the variable is declared before the context that holds it. Both
	// conditions have to hold: enough plan calls have completed, AND the tool
	// whose repeat matters has actually taken a seat. Counting plan calls
	// alone would let the crash fire after a search.
	first := NewActivities(Options{Planner: f.BeforeCrash, Sleep: noLatency, Tools: f.Tools})
	var c1 *replayCtx
	c1 = newReplayCtx(nil, dispatch(first), func(activityType string, liveIndex int) bool {
		return activityType == ActivityPlan &&
			liveIndex > f.CrashAfterLivePlanCalls &&
			c1.liveByType[watched] > 0
	})

	if _, err := Run(c1, f.Request("double-booking-demo")); !errors.Is(err, ErrSimulatedCrash) {
		if _, booked := lastCall(c1.ToolCalls, watched); !booked {
			// The crash never fired because its precondition never held: the
			// planner spent its whole step budget without calling the watched
			// tool. Nothing malfunctioned.
			return trace, nil, fmt.Errorf("%w: it never called %s (last error: %v)",
				ErrNeverReachedCrash, watched, err)
		}
		return trace, nil, fmt.Errorf("fixture: first incarnation should have crashed, got %v", err)
	}

	// The LAST call to the watched tool, not the first: it is the one the
	// interrupted plan was following on from, and therefore the one the
	// recovered incarnation might repeat.
	before, ok := lastCall(c1.ToolCalls, watched)
	if !ok {
		return trace, nil, fmt.Errorf("%w: it never called %s", ErrNeverReachedCrash, watched)
	}
	trace.Before = before

	// Incarnation two: a different process, a fresh planner, and whatever of
	// the first incarnation's history actually survived the crash.
	//
	// LoseUncommittedJournal decides what that is. Inheriting the journal
	// models a crash after the work was committed; discarding it models a
	// worker that took the seat and died before committing anything about it,
	// which is the only version of this crash in which a live model is asked
	// the same question twice. See the field's comment in fixture.go.
	inherited := c1.Journal
	if f.LoseUncommittedJournal {
		inherited = nil
	}
	second := NewActivities(Options{Planner: f.AfterCrash, Sleep: noLatency, Tools: f.Tools})
	c2 := newReplayCtx(inherited, dispatch(second), nil)
	approval, err := json.Marshal(Approval{Approved: true, Reviewer: "demo-reviewer"})
	if err != nil {
		return trace, nil, err
	}
	c2.signals[SignalHumanApproval] = approval

	res, err := Run(c2, f.Request("double-booking-demo"))
	if err != nil {
		return trace, nil, fmt.Errorf("fixture: recovered incarnation failed: %w", err)
	}

	live := make([]ToolCall, 0, len(c2.ToolCalls))
	for _, rc := range c2.ToolCalls {
		if !rc.Replayed {
			live = append(live, rc.Call)
		}
	}

	// How many new tool calls the recovered incarnation made is reported, not
	// asserted. A scripted planner makes exactly one, and the fixture tests
	// still check that by checking the wording it produced. A live planner may
	// make none — the honest answer when the transcript already shows the
	// booking — and failing the run for that would throw away the measurement
	// instead of recording it.
	trace.AfterCalls = live
	if after, ok := firstCall(live, watched); ok {
		trace.After = after
	}
	trace.Result = res
	trace.ReplayedCalls = c2.Replayed
	trace.LiveCalls = c2.Live

	bookings := append(first.Bookings(), second.Bookings()...)
	return trace, bookings, nil
}
