package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Every blocking call a workflow makes is numbered. The number, k, is simply a
// counter that starts at zero and goes up by one each time the workflow asks
// for something it might have to wait for — an activity, a sleep, a signal, a
// side effect. Because the workflow code is deterministic, the same call gets
// the same k on every single execution of that run. k is therefore the join
// key between "what the code is asking for now" and "what history says
// happened", and it is the reason replay needs no clever matching at all.
//
// The signature strings below are the second half of that join. Knowing that
// something happened at k=3 is not enough; we also have to know it was the same
// *kind* of thing, otherwise a code change that swaps a sleep for an activity
// would silently reuse the sleep's recorded answer. A signature is compared on
// every replayed call, and a mismatch is a divergence.

const (
	sigTimer      = "timer"
	sigSideEffect = "sideEffect"
)

func sigActivity(activityType string) string { return "activity:" + activityType }
func sigSignal(name string) string           { return "signal:" + name }

// histEntry is everything history knows about one k.
//
// scheduled and resolved are separate because the two states mean very
// different things to the dispatcher. "Scheduled but not resolved" means the
// work is already out there — an activity task is on the queue, a timer row is
// in the table — so the workflow must block again and, crucially, must NOT emit
// a second command. Emitting one would schedule the same activity twice.
type histEntry struct {
	seq          core.Seq // seq of the scheduling event, quoted in divergence errors
	signature    string   // what history records was requested at this k
	activityType string   // plain name, for building a readable ActivityError
	at           time.Time

	resolved bool
	value    json.RawMessage
	err      error
}

// signalRecord is one delivered signal. Signals are the one thing history does
// not stamp with a k (see the note in indexHistory), so they are queued by name
// and consumed in arrival order.
type signalRecord struct {
	payload json.RawMessage
	at      time.Time
}

// history is the replay index: everything the dispatcher needs, arranged for
// lookup by k, built once per workflow task.
type history struct {
	entries map[int]*histEntry
	signals map[string][]signalRecord
	dedup   map[int]*core.DedupEvaluated

	// start is the CreatedAt of WorkflowExecutionStarted. It is the workflow's
	// notion of "now" before any other event has been replayed.
	start time.Time
	// last is the CreatedAt of the newest event in the history.
	last time.Time

	// terminal is non-empty when history already contains a completion or
	// failure. A workflow task for a finished run is not an error — a duplicate
	// task row survives a crash between the commit and the delete — but it must
	// produce no further commands.
	terminal core.ExecutionStatus
	output   json.RawMessage
	failure  string
}

func (h *history) entry(k int) (*histEntry, bool) {
	e, ok := h.entries[k]
	return e, ok
}

func (h *history) ensure(k int) *histEntry {
	if e, ok := h.entries[k]; ok {
		return e
	}
	e := &histEntry{}
	h.entries[k] = e
	return e
}

// indexHistory turns the flat event log into the by-k index above.
//
// Events are assumed to arrive in seq order, which is what core.EventStore.Load
// promises. That matters for two reasons: a completion must be able to see the
// activity type recorded by its own scheduling event, and signals must be
// queued in the order they were actually delivered.
func indexHistory(events []core.Event) (*history, error) {
	h := &history{
		entries: make(map[int]*histEntry),
		signals: make(map[string][]signalRecord),
		dedup:   make(map[int]*core.DedupEvaluated),
	}

	for i := range events {
		ev := &events[i]
		if ev.CreatedAt.After(h.last) {
			h.last = ev.CreatedAt
		}

		switch ev.Type {
		case core.EvWorkflowStarted:
			h.start = ev.CreatedAt

		case core.EvActivityScheduled, core.EvCompensationScheduled:
			var p core.ActivityScheduled
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			e := h.ensure(p.K)
			e.seq = ev.Seq
			e.signature = sigActivity(p.ActivityType)
			e.activityType = p.ActivityType
			e.at = ev.CreatedAt

		case core.EvActivityCompleted, core.EvCompensationCompleted:
			var p core.ActivityCompleted
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			e := h.ensure(p.K)
			e.resolved = true
			e.value = p.Result
			e.at = ev.CreatedAt

		case core.EvActivityFailed, core.EvCompensationFailed:
			var p core.ActivityFailed
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			e := h.ensure(p.K)
			e.resolved = true
			e.at = ev.CreatedAt
			// A failure only reaches history once the retry policy has given
			// up, so by the time the workflow sees it, it is final for that
			// call. The workflow's own error handling — usually a saga
			// compensation — is what happens next.
			e.err = newActivityError(e.activityType, p.K, p.Error, p.NonRetryable)

		case core.EvTimerStarted:
			var p core.TimerStarted
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			e := h.ensure(p.K)
			e.seq = ev.Seq
			e.signature = sigTimer
			e.at = ev.CreatedAt

		case core.EvTimerFired:
			var p core.TimerFired
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			e := h.ensure(p.K)
			e.resolved = true
			e.value = jsonNull
			e.at = ev.CreatedAt

		case core.EvSideEffect:
			var p core.SideEffectRecorded
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			e := h.ensure(p.K)
			e.seq = ev.Seq
			e.signature = sigSideEffect
			e.resolved = true
			e.value = p.Value
			e.at = ev.CreatedAt

		case core.EvSignalReceived:
			var p core.SignalReceived
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			// Signals carry no k, and cannot: the event is written by whoever
			// sent the signal, and a sender has no idea which line of workflow
			// code will eventually consume it. So the join is by name and
			// arrival order instead. That is still deterministic — the log
			// fixes the order once and for all — but it does mean a signal
			// wait is the one blocking call whose kind cannot be checked for
			// divergence. See the integration notes.
			h.signals[p.Name] = append(h.signals[p.Name], signalRecord{
				payload: p.Payload,
				at:      ev.CreatedAt,
			})

		case core.EvDedupEvaluated:
			var p core.DedupEvaluated
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			// The semantic dedup verdict is journaled so replay never has to
			// call the embedding model again. This package does not act on it;
			// it indexes it so the verdict is available to the caller and so an
			// unrecognised event type is never silently dropped.
			cp := p
			h.dedup[p.K] = &cp

		case core.EvWorkflowCompleted:
			var p core.WorkflowCompleted
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			h.terminal = core.StatusCompleted
			h.output = p.Result

		case core.EvWorkflowFailed:
			var p core.WorkflowFailed
			if err := decode(ev, &p); err != nil {
				return nil, err
			}
			h.terminal = core.StatusFailed
			h.failure = p.Error

		default:
			return nil, fmt.Errorf("workflow: history contains unknown event type %q at seq %d",
				ev.Type, ev.Seq)
		}
	}

	if h.start.IsZero() {
		h.start = h.last
	}
	return h, nil
}

func decode(ev *core.Event, dst any) error {
	if len(ev.Payload) == 0 {
		return fmt.Errorf("workflow: event %s at seq %d has an empty payload", ev.Type, ev.Seq)
	}
	if err := json.Unmarshal(ev.Payload, dst); err != nil {
		return fmt.Errorf("workflow: event %s at seq %d has an undecodable payload: %w",
			ev.Type, ev.Seq, err)
	}
	return nil
}

var jsonNull = json.RawMessage("null")
