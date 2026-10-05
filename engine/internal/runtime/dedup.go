package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// The semantic check runs HERE — in the worker, between the dispatcher
// producing commands and the committer turning them into events.
//
// It cannot run inside the dispatcher, because the dispatcher is the replay
// path: it must be a pure function of history, and an embedding call is
// neither pure nor local. It cannot run inside the activity worker either,
// because by then the workflow has already committed its intention to act, and
// a verdict reached after that point cannot un-schedule anything.
//
// This is the only position where the verdict can be computed once, folded
// into the SAME transaction as the decision that produced it, and then read
// back from history forever after without touching the model again.

// semanticParams carries the run identity a check needs.
type semanticParams struct {
	ShardID    core.ShardID
	WorkflowID string
	RunID      core.RunID
}

// semanticOutcome is what the check pass decided, expressed as edits to make
// to the decision rather than as effects already applied.
type semanticOutcome struct {
	// verdicts are CmdRecordDedup commands to fold into the same commit, so a
	// verdict and the decision it governs land together or not at all.
	verdicts []core.Command

	// reuse maps a program-order index whose activity was judged DUPLICATE to
	// the result being reused. Its activity task is dropped and a completion
	// is journaled instead, so replay resolves the future without ever running
	// the side effect.
	reuse map[int]json.RawMessage

	// escalate lists the indexes whose similarity fell in the uncertain band.
	escalate []int

	// block lists the indexes the ESCALATION_BLOCKS policy is holding back.
	// Their activity task is dropped and NOTHING is journaled in its place, so
	// the workflow stays parked on an unresolved future until a person
	// adjudicates. It is empty whenever the policy is off, which is the
	// default.
	block map[int]bool
}

// recordedVerdicts reports which indexes already have a journaled verdict.
// Replay must never re-run a check: the recorded answer is the answer.
func recordedVerdicts(hist []core.Event) map[int]bool {
	seen := make(map[int]bool)
	for _, ev := range hist {
		if ev.Type != core.EvDedupEvaluated {
			continue
		}
		var p core.DedupEvaluated
		if json.Unmarshal(ev.Payload, &p) == nil {
			seen[p.K] = true
		}
	}
	return seen
}

// kBySeq maps a scheduling event's seq back to its program-order index. The
// checker needs it because core.Intent records ScheduledSeq while
// core.DedupEvaluated reports MatchedK, and core provides no bridge.
func kBySeq(hist []core.Event) map[core.Seq]int {
	m := make(map[core.Seq]int)
	for _, ev := range hist {
		if ev.Type != core.EvActivityScheduled {
			continue
		}
		var p core.ActivityScheduled
		if json.Unmarshal(ev.Payload, &p) == nil {
			m[ev.Seq] = p.K
		}
	}
	return m
}

// resultAt returns the recorded result of a completed activity at index k.
func resultAt(hist []core.Event, k int) (json.RawMessage, bool) {
	for _, ev := range hist {
		if ev.Type != core.EvActivityCompleted {
			continue
		}
		var p core.ActivityCompleted
		if json.Unmarshal(ev.Payload, &p) == nil && p.K == k {
			return p.Result, true
		}
	}
	return nil, false
}

// resolveSemantic checks every activity this task scheduled in Semantic mode
// that does not already have a journaled verdict.
func (e *Engine) resolveSemantic(
	ctx context.Context, hist []core.Event, res *workflow.Result, sp semanticParams,
) (semanticOutcome, error) {
	out := semanticOutcome{reuse: map[int]json.RawMessage{}, block: map[int]bool{}}
	if e.Checker == nil {
		return out, nil
	}
	already := recordedVerdicts(hist)
	seqIndex := kBySeq(hist)

	for _, cmd := range res.Commands {
		if cmd.Type != core.CmdScheduleActivity || cmd.Mode != core.ModeSemantic {
			continue
		}
		if already[cmd.K] {
			continue // replay: the verdict is in history, use it, do not recompute
		}

		ev, err := e.Checker.Check(ctx, semantic.CheckRequest{
			ShardID:      sp.ShardID,
			WorkflowID:   sp.WorkflowID,
			RunID:        sp.RunID,
			K:            cmd.K,
			ActivityType: cmd.ActivityType,
			Input:        cmd.Input,
			KBySeq:       seqIndex,
		})
		if err != nil {
			// A checker failure must not fail the workflow. The model being
			// down is an availability problem, not a correctness one, and the
			// engine's baseline behaviour without any check at all is simply
			// to run the activity.
			return out, fmt.Errorf("semantic check at k=%d: %w", cmd.K, err)
		}

		verdict := core.Command{Type: core.CmdRecordDedup, K: cmd.K, Dedup: &ev}
		out.verdicts = append(out.verdicts, verdict)

		switch ev.Verdict {
		case core.VerdictDistinct:
			// Genuinely new work. Leave the schedule command untouched.

		case core.VerdictDuplicate:
			// The engine has already performed this action in this run. Reuse
			// the recorded result and never touch the outside world again.
			prior, ok := resultAt(hist, ev.MatchedK)
			if !ok {
				// The matched call has no journaled result, so there is
				// nothing to reuse. Refusing to skip is the safe direction:
				// a false positive silently drops a real action, which is the
				// asymmetric failure this whole mechanism is designed around.
				continue
			}
			out.reuse[cmd.K] = prior

		case core.VerdictEscalate:
			// The uncertain band: the score sits between TauLow and TauHigh,
			// which is precisely where the model is least able to tell a
			// repeat from genuinely new work.
			//
			// DEFAULT POLICY — the activity RUNS. The schedule command is left
			// untouched, exactly as for DISTINCT.
			//
			// The reason is that the two ways of being wrong here are not
			// equally bad, and the project's own risk table says so. A false
			// positive — calling something a duplicate when it was not — makes
			// the engine silently drop a real action: a flight never booked, a
			// refund never sent, and no error anywhere to notice it by. That
			// is rated severe. A false negative — running something that was
			// in fact a repeat — is no worse than what the engine does with no
			// semantic check at all, because the activity's exact idempotency
			// key still travels with the call and the downstream service still
			// gets its chance to recognise the repeat. The harm is therefore
			// asymmetric, and executing inside the band is what guarantees
			// this mechanism can never be WORSE than the exact-match baseline
			// on the dangerous axis. It can only ever be better, on the other.
			//
			// Nothing is hidden by that choice. The verdict is journaled in
			// the same transaction either way, so every escalation is in the
			// event log with its similarity score and both thresholds, and the
			// evaluation can measure exactly how wide the band is and what
			// fell into it.
			out.escalate = append(out.escalate, cmd.K)
			e.Runtime.countEscalation(cmd.ActivityType, escalationPolicy(e.EscalationBlocks))

			if !e.EscalationBlocks {
				break
			}

			// STRICTER POLICY (ESCALATION_BLOCKS=true, design decision D5) —
			// the activity does not run and the run waits for a person.
			//
			// The activity task is dropped and, unlike DUPLICATE, no
			// completion is journaled in its place. The scheduling event
			// stays, so on the next replay the workflow finds this k
			// "scheduled but unresolved" and parks there instead of emitting
			// a second command. The ESCALATE verdict committed alongside it
			// is the durable record that the run is awaiting adjudication:
			// an activity that was scheduled, has no task and has no result,
			// with a journaled reason why.
			//
			// A human then sends the adjudication signal, and adjudicate.go
			// turns that answer into either a released activity task or a
			// reused result. This policy exists so the experiment can run the
			// same workload both ways and compare them.
			out.block[cmd.K] = true
		}
	}
	return out, nil
}

// escalationPolicy labels the counter with which of the two policies was in
// force, so the two arms of the experiment are distinguishable in one series.
func escalationPolicy(blocks bool) string {
	if blocks {
		return "blocks"
	}
	return "executes"
}

// applyOutcome edits a built decision so a DUPLICATE verdict skips the side
// effect: the activity task is dropped and a completion carrying the reused
// result is journaled in its place, so the next replay resolves the future
// from history exactly as if the activity had run.
//
// A blocked ESCALATE index has its task dropped too, but gets no replacement
// event: the whole point is that the future stays unresolved until a person
// answers.
func applyOutcome(d *core.Decision, out semanticOutcome, now int64) error {
	if len(out.reuse) == 0 && len(out.block) == 0 {
		return nil
	}
	seqForK := map[int]core.Seq{}
	for _, ev := range d.Events {
		if ev.Type != core.EvActivityScheduled {
			continue
		}
		var p core.ActivityScheduled
		if json.Unmarshal(ev.Payload, &p) == nil {
			seqForK[p.K] = ev.Seq
		}
	}

	// dropped is every index whose activity must not be dispatched, for
	// either reason. Building the set first means the task filter below is one
	// map lookup instead of a scan per task.
	dropped := make(map[core.Seq]bool, len(out.reuse)+len(out.block))
	for k := range out.reuse {
		if s, ok := seqForK[k]; ok {
			dropped[s] = true
		}
	}
	for k := range out.block {
		if s, ok := seqForK[k]; ok {
			dropped[s] = true
		}
	}

	kept := d.NewTasks[:0]
	for _, t := range d.NewTasks {
		if t.Type == core.TaskActivity && dropped[t.ScheduledEventSeq] {
			continue
		}
		kept = append(kept, t)
	}
	d.NewTasks = kept

	if len(out.reuse) == 0 {
		return nil
	}

	next := d.LastSeq
	// Sorted, because map iteration order is random and these events are
	// written to an append-only log: two nodes replaying the same task must
	// produce the same seq for the same k, or the log disagrees with itself.
	ks := make([]int, 0, len(out.reuse))
	for k := range out.reuse {
		ks = append(ks, k)
	}
	sort.Ints(ks)

	for _, k := range ks {
		result := out.reuse[k]
		next++
		payload, err := json.Marshal(core.ActivityCompleted{K: k, Result: result})
		if err != nil {
			return err
		}
		last := d.Events[len(d.Events)-1]
		d.Events = append(d.Events, core.Event{
			ShardID: d.ShardID, WorkflowID: d.WorkflowID, RunID: d.RunID,
			Seq: next, Type: core.EvActivityCompleted, Payload: payload,
			CreatedAt: last.CreatedAt,
		})
	}
	d.LastSeq = next
	return nil
}
