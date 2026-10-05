package runtime

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// Some facts have to be written into history BEFORE the workflow function is
// re-executed, because the workflow's next decision depends on them.
//
// A durable sleep is the clearest example. The timer scanner's job is only to
// notice that a timer row is due and to enqueue a workflow task; it writes no
// event, because writing history is the workflow worker's job and splitting
// that would give two components the right to append to the same log. So the
// worker, on picking up that task, is the one that journals TimerFired. Until
// it does, the workflow replays, finds its sleep "started but not fired", and
// parks again.
//
// The adjudication answers are the same shape of problem: a person has
// replied to an escalation, the reply is sitting in history as a signal, and
// the workflow cannot move until that reply has been turned into something the
// replay index understands.
//
// Everything produced here is called the PREFACE: events that are appended
// ahead of whatever the workflow task itself decides, in the very same commit,
// so the two either land together or not at all.

// AdjudicationSignal is the signal name a person sends to answer an escalated
// duplicate check.
//
// It begins with two underscores to mark it as the engine's own, like the
// cancellation signal, so it cannot collide with a name workflow code waits
// on. Adjudication travels as an ordinary signal rather than as a new endpoint
// or table precisely because signals are already durable, already ordered by
// the log and already replayed — reusing them means the human's answer
// inherits every property the engine already proved.
const AdjudicationSignal = "__adjudicate"

// AdjudicatedPrefix marks the verdict written when a person releases a held
// activity. core.DedupEvaluated has no field for "a human decided this", and
// core is frozen, so it is recorded in the model name the same way the
// semantic package records its own degraded path: the log still says which
// model was consulted, and it now also says that a person overruled the band.
// This is reported as a gap in core.
const AdjudicatedPrefix = "adjudicated!"

// Adjudication is the payload of the adjudication signal.
type Adjudication struct {
	// K is the program-order index of the held activity.
	K int `json:"k"`
	// Approved true means "this really is new work, run it". False means
	// "this is a repeat, reuse what we already have".
	Approved bool `json:"approved"`
	// Reviewer and Note are recorded for the audit trail. Nothing branches on
	// them; they exist so the log can answer who decided and why.
	Reviewer string `json:"reviewer,omitempty"`
	Note     string `json:"note,omitempty"`
}

// preface is the set of edits a worker must make to history before replaying.
type preface struct {
	events []core.Event
	tasks  []core.Task
}

// empty reports whether there is nothing to write.
func (p preface) empty() bool { return len(p.events) == 0 && len(p.tasks) == 0 }

// buildPreface produces the events and tasks that must precede this workflow
// task's own decisions, numbering events from firstSeq.
//
// It is a pure function of history, the clock and the starting sequence
// number, which is what lets it be table-tested with no database and no
// workflow code at all.
func buildPreface(hist []core.Event, firstSeq core.Seq, now time.Time) preface {
	var p preface
	seq := firstSeq
	next := func() core.Seq {
		s := seq
		seq++
		return s
	}

	idx := indexRun(hist)

	// Timers that are due but not yet recorded as fired.
	for _, k := range sortedKeys(idx.timers) {
		t := idx.timers[k]
		if idx.resolved[k] {
			continue
		}
		if t.fireAt.After(now) {
			// Not due yet. A workflow task can arrive early for other reasons
			// — a signal, a retried task row — and firing a timer ahead of
			// time would hand the workflow an answer it has not earned.
			continue
		}
		payload, err := json.Marshal(core.TimerFired{K: k})
		if err != nil {
			continue
		}
		p.events = append(p.events, core.Event{
			Seq: next(), Type: core.EvTimerFired, Payload: payload, CreatedAt: now,
		})
		idx.resolved[k] = true
	}

	// Human answers to escalated duplicate checks.
	for _, a := range idx.adjudications {
		sched, ok := idx.scheduled[a.K]
		if !ok || idx.resolved[a.K] {
			// Either the index names an activity that was never scheduled, or
			// the activity has already finished. Both mean there is nothing
			// left to release.
			continue
		}
		v := idx.verdicts[a.K]
		if v == nil || v.Verdict != core.VerdictEscalate {
			// Only an activity still held by an ESCALATE verdict is waiting
			// for an answer. Anything else has already been settled, and
			// acting again would run it a second time.
			continue
		}

		if a.Approved {
			// Released. The replacement verdict is what stops this from
			// happening twice: it supersedes the ESCALATE, so the next
			// workflow task sees an activity that is no longer held and
			// enqueues nothing.
			released := *v
			released.Verdict = core.VerdictDistinct
			released.Model = AdjudicatedPrefix + v.Model
			payload, err := json.Marshal(released)
			if err != nil {
				continue
			}
			p.events = append(p.events, core.Event{
				Seq: next(), Type: core.EvDedupEvaluated, Payload: payload, CreatedAt: now,
			})
			p.tasks = append(p.tasks, core.Task{
				Type: core.TaskActivity, ScheduledEventSeq: sched, VisibleAt: now,
			})
			idx.verdicts[a.K] = &released
			continue
		}

		// Confirmed a repeat. The recorded result of the matched call is
		// journaled as this call's result, so the workflow resolves its future
		// from history and the side effect never happens — the same shape as a
		// DUPLICATE verdict, reached by a person instead of by a threshold.
		prior, ok := resultAt(hist, v.MatchedK)
		if !ok {
			// Nothing to reuse. Rather than invent a result, report the
			// rejection as the activity's failure, so the workflow's own error
			// handling — usually a saga compensation — decides what happens
			// next instead of the engine guessing.
			payload, err := json.Marshal(core.ActivityFailed{
				K:            a.K,
				Error:        "held as a duplicate by " + reviewerName(a) + ": " + a.Note,
				Attempt:      0,
				NonRetryable: true,
			})
			if err != nil {
				continue
			}
			p.events = append(p.events, core.Event{
				Seq: next(), Type: core.EvActivityFailed, Payload: payload, CreatedAt: now,
			})
			idx.resolved[a.K] = true
			continue
		}
		payload, err := json.Marshal(core.ActivityCompleted{K: a.K, Result: prior})
		if err != nil {
			continue
		}
		p.events = append(p.events, core.Event{
			Seq: next(), Type: core.EvActivityCompleted, Payload: payload, CreatedAt: now,
		})
		idx.resolved[a.K] = true
	}

	return p
}

func reviewerName(a Adjudication) string {
	if a.Reviewer == "" {
		return "a reviewer"
	}
	return a.Reviewer
}

// runIndex is the small slice of history the preface needs: what was
// scheduled, what already has an answer, what the last verdict at each index
// said, and which timers exist.
type runIndex struct {
	scheduled map[int]core.Seq
	resolved  map[int]bool
	verdicts  map[int]*core.DedupEvaluated
	timers    map[int]timerEntry

	// adjudications are in delivery order, because a reviewer may answer the
	// same index twice and the log fixes which answer came first.
	adjudications []Adjudication
}

type timerEntry struct {
	fireAt time.Time
}

func indexRun(hist []core.Event) runIndex {
	idx := runIndex{
		scheduled: map[int]core.Seq{},
		resolved:  map[int]bool{},
		verdicts:  map[int]*core.DedupEvaluated{},
		timers:    map[int]timerEntry{},
	}
	for i := range hist {
		ev := &hist[i]
		switch ev.Type {
		case core.EvActivityScheduled, core.EvCompensationScheduled:
			var p core.ActivityScheduled
			if json.Unmarshal(ev.Payload, &p) == nil {
				idx.scheduled[p.K] = ev.Seq
			}
		case core.EvActivityCompleted, core.EvCompensationCompleted:
			var p core.ActivityCompleted
			if json.Unmarshal(ev.Payload, &p) == nil {
				idx.resolved[p.K] = true
			}
		case core.EvActivityFailed, core.EvCompensationFailed:
			var p core.ActivityFailed
			if json.Unmarshal(ev.Payload, &p) == nil {
				idx.resolved[p.K] = true
			}
		case core.EvTimerStarted:
			var p core.TimerStarted
			if json.Unmarshal(ev.Payload, &p) == nil {
				idx.timers[p.K] = timerEntry{fireAt: p.FireAt}
			}
		case core.EvTimerFired:
			var p core.TimerFired
			if json.Unmarshal(ev.Payload, &p) == nil {
				idx.resolved[p.K] = true
			}
		case core.EvDedupEvaluated:
			var p core.DedupEvaluated
			if json.Unmarshal(ev.Payload, &p) == nil {
				cp := p
				// Last one wins: a released activity carries a second verdict
				// written after the first, and it is the later one that says
				// whether the activity is still held.
				idx.verdicts[p.K] = &cp
			}
		case core.EvSignalReceived:
			var p core.SignalReceived
			if json.Unmarshal(ev.Payload, &p) != nil || p.Name != AdjudicationSignal {
				continue
			}
			var a Adjudication
			if json.Unmarshal(p.Payload, &a) == nil {
				idx.adjudications = append(idx.adjudications, a)
			}
		}
	}
	return idx
}

// sortedKeys keeps the order of generated events stable. Map order is random
// in Go, and these events are given sequence numbers in an append-only log, so
// two workers replaying the same task have to agree on which came first.
func sortedKeys(m map[int]timerEntry) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// stamp fills in the identity fields the preface deliberately left blank.
// buildPreface knows nothing about which run it is working on, which is what
// makes it testable from a slice of events alone.
func (p *preface) stamp(shard core.ShardID, workflowID string, run core.RunID) {
	for i := range p.events {
		p.events[i].ShardID = shard
		p.events[i].WorkflowID = workflowID
		p.events[i].RunID = run
	}
	for i := range p.tasks {
		p.tasks[i].ShardID = shard
		p.tasks[i].WorkflowID = workflowID
		p.tasks[i].RunID = run
	}
}

// lastSeq is the highest seq the preface used, or start when it wrote nothing.
func (p preface) lastSeq(start core.Seq) core.Seq {
	last := start
	for _, ev := range p.events {
		if ev.Seq > last {
			last = ev.Seq
		}
	}
	return last
}

// intentText is the sentence the semantic check will compare a later call
// against. It is produced by the same function the checker uses, so a stored
// row and a live query can never be built from two different
// canonicalisations of the same arguments.
func intentText(sched core.ActivityScheduled) (string, error) {
	return semantic.IntentText(sched.ActivityType, sched.Input)
}
