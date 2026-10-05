package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"

	"github.com/dharaneswar339/durexec/internal/activity"
)

// DecisionParams is the identity and bookkeeping that Execute deliberately does
// not know. Keeping it out of the dispatcher is what lets replay be tested with
// nothing but a slice of events; it is supplied here, at the edge, where the
// worker already has all of it in hand.
type DecisionParams struct {
	ShardID    core.ShardID
	WorkflowID string
	RunID      core.RunID

	// Token is the fencing token this worker believes it holds for the shard.
	// It is re-checked FOR UPDATE inside the commit transaction, so a worker
	// that was fenced out while it was thinking cannot write.
	Token core.FencingToken

	// LastSeq is the highest seq already in this run's history. New events are
	// numbered from LastSeq+1, and the primary key on (shard, workflow, run,
	// seq) is what makes two workers racing on the same task collide instead of
	// both succeeding.
	LastSeq core.Seq

	// CompleteTaskID is the workflow task row this decision completes.
	CompleteTaskID int64

	// Now is real wall-clock time, taken from a core.Clock by the caller.
	//
	// This is emphatically NOT the workflow's Now(). The workflow's clock is
	// frozen to history so its decisions replay identically; but a timer has to
	// fire at a real moment in the future, and an event's created_at has to
	// reflect when it was really written. Using the replay clock for FireAt
	// would schedule a three-week sleep relative to an instant three weeks ago,
	// and it would fire immediately.
	Now time.Time
}

// BuildDecision turns the outcome of one workflow task into the single atomic
// unit the committer applies: the events to append, the tasks and timers to
// create, and the status to move the run to. Either all of it lands or none of
// it does, which is what makes "schedule an activity" and "record that we
// scheduled it" impossible to get out of step.
func BuildDecision(p DecisionParams, res Result) (core.Decision, error) {
	d := core.Decision{
		ShardID:        p.ShardID,
		WorkflowID:     p.WorkflowID,
		RunID:          p.RunID,
		Token:          p.Token,
		Status:         res.Status,
		LastSeq:        p.LastSeq,
		CompleteTaskID: p.CompleteTaskID,
	}

	seq := p.LastSeq
	nextSeq := func() core.Seq {
		seq++
		return seq
	}
	appendEvent := func(t core.EventType, payload any) (core.Seq, error) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("workflow: encoding %s payload: %w", t, err)
		}
		s := nextSeq()
		d.Events = append(d.Events, core.Event{
			ShardID:    p.ShardID,
			WorkflowID: p.WorkflowID,
			RunID:      p.RunID,
			Seq:        s,
			Type:       t,
			Payload:    raw,
			CreatedAt:  p.Now,
		})
		return s, nil
	}

	for _, cmd := range res.Commands {
		switch cmd.Type {
		case core.CmdScheduleActivity:
			// The dedup key is stamped here because it is derived from the run
			// identity, which the dispatcher does not have. It is a pure
			// function of (workflow, run, k), so it is byte-identical on every
			// replay and after every crash — which is exactly the property an
			// external service needs from an Idempotency-Key.
			dedup := cmd.DedupKey
			if dedup == "" {
				dedup = activity.DedupKey(p.WorkflowID, p.RunID, cmd.K)
			}
			mode := cmd.Mode
			if mode == "" {
				mode = core.ModeExact
			}
			s, err := appendEvent(core.EvActivityScheduled, core.ActivityScheduled{
				K:            cmd.K,
				ActivityType: cmd.ActivityType,
				Input:        cmd.Input,
				DedupKey:     dedup,
				Mode:         mode,
			})
			if err != nil {
				return core.Decision{}, err
			}
			// The activity task carries the seq of its own scheduling event.
			// That is the only link the activity worker needs: it loads that
			// event to learn what to run, and writes its result back against
			// the same k.
			d.NewTasks = append(d.NewTasks, core.Task{
				ShardID:           p.ShardID,
				Type:              core.TaskActivity,
				WorkflowID:        p.WorkflowID,
				RunID:             p.RunID,
				ScheduledEventSeq: s,
				VisibleAt:         p.Now,
			})

		case core.CmdStartTimer:
			fireAt := p.Now.Add(cmd.Duration)
			s, err := appendEvent(core.EvTimerStarted, core.TimerStarted{
				K:        cmd.K,
				Duration: cmd.Duration,
				FireAt:   fireAt,
			})
			if err != nil {
				return core.Decision{}, err
			}
			// A durable sleep is a row, not a goroutine. Ten thousand sleeping
			// workflows cost ten thousand rows and no memory at all, and they
			// survive every restart in between.
			d.NewTimers = append(d.NewTimers, core.Timer{
				ShardID:    p.ShardID,
				WorkflowID: p.WorkflowID,
				RunID:      p.RunID,
				TimerSeq:   s,
				FireAt:     fireAt,
			})

		case core.CmdRecordSideEffect:
			if _, err := appendEvent(core.EvSideEffect, core.SideEffectRecorded{
				K:     cmd.K,
				Value: cmd.Value,
			}); err != nil {
				return core.Decision{}, err
			}

		case core.CmdRecordDedup:
			// Emitted by the semantic-deduplication path, not by this package.
			// Handled so a caller can fold a verdict into the same atomic
			// commit as the decision that produced it.
			if cmd.Dedup == nil {
				return core.Decision{}, fmt.Errorf("workflow: %s command at k=%d carries no verdict",
					cmd.Type, cmd.K)
			}
			if _, err := appendEvent(core.EvDedupEvaluated, cmd.Dedup); err != nil {
				return core.Decision{}, err
			}

		case core.CmdWaitSignal:
			// Nothing to journal. A signal arrives from outside and is written
			// by whoever sends it; the workflow simply re-discovers that it is
			// waiting on every task until one does. Writing a "waiting" event
			// would append a new row on every task forever, since nothing would
			// ever suppress it.

		case core.CmdCompleteWorkflow:
			if _, err := appendEvent(core.EvWorkflowCompleted, core.WorkflowCompleted{
				Result: cmd.Result,
			}); err != nil {
				return core.Decision{}, err
			}

		case core.CmdFailWorkflow:
			if _, err := appendEvent(core.EvWorkflowFailed, core.WorkflowFailed{
				Error: cmd.Err,
			}); err != nil {
				return core.Decision{}, err
			}

		default:
			return core.Decision{}, fmt.Errorf("workflow: unknown command type %q at k=%d",
				cmd.Type, cmd.K)
		}
	}

	d.LastSeq = seq
	return d, nil
}
