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

	isTool := activityType == ActivityBookFlight || activityType == ActivitySearchFlights
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

	// Incarnation one: plans, books, then dies before the next plan lands.
	first := NewActivities(Options{Planner: f.BeforeCrash, Sleep: noLatency})
	c1 := newReplayCtx(nil, dispatch(first), func(activityType string, liveIndex int) bool {
		return activityType == ActivityPlan && liveIndex > f.CrashAfterLivePlanCalls
	})

	if _, err := Run(c1, f.Request("double-booking-demo")); !errors.Is(err, ErrSimulatedCrash) {
		return trace, nil, fmt.Errorf("fixture: first incarnation should have crashed, got %v", err)
	}
	if len(c1.ToolCalls) != 1 {
		return trace, nil, fmt.Errorf("fixture: first incarnation made %d tool calls, want 1", len(c1.ToolCalls))
	}
	trace.Before = c1.ToolCalls[0].Call

	// Incarnation two: a different process, a fresh planner, the same journal.
	second := NewActivities(Options{Planner: f.AfterCrash, Sleep: noLatency})
	c2 := newReplayCtx(c1.Journal, dispatch(second), nil)
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
	if len(live) != 1 {
		return trace, nil, fmt.Errorf("fixture: recovered incarnation made %d new tool calls, want 1", len(live))
	}

	trace.After = live[0]
	trace.Result = res
	trace.ReplayedCalls = c2.Replayed
	trace.LiveCalls = c2.Live

	bookings := append(first.Bookings(), second.Bookings()...)
	return trace, bookings, nil
}
