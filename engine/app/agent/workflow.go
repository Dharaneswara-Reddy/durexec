// Package agent is the second demonstration application: a travel-booking
// agent that plans, calls tools, pauses for a human, and finishes.
//
// This is the workflow the project's contribution is aimed at. Its tool calls
// are issued by a language model, so the arguments are English sentences
// rather than identifiers. Two calls can mean exactly the same thing and share
// not a single byte, which is precisely the case that byte-equal idempotency
// keys cannot see. See fixture.go for the concrete scenario.
package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// The workflow-facing interface.
//
// internal/workflow is being written in parallel, so this package declares the
// smallest surface it needs. Go interfaces are structural, so the engine's own
// context satisfies this as soon as the method set lines up.
// ---------------------------------------------------------------------------

// Ctx is the workflow's only way to reach the outside world. Every call is
// journaled before it happens and replayed from the journal afterwards.
type Ctx interface {
	// ExecuteActivity schedules one activity and returns a handle to its result.
	ExecuteActivity(activityType string, input any) Future
	// Sleep is a durable timer: a database row, not a goroutine.
	Sleep(d time.Duration) Future
	// WaitForSignal blocks until an external caller delivers the named signal.
	// A workflow can sit here for days across any number of process restarts.
	WaitForSignal(name string) Future
	// Now is the replay-safe clock.
	Now() time.Time
}

// Future is the handle to one pending durable operation.
type Future interface {
	Get() (json.RawMessage, error)
}

// ---------------------------------------------------------------------------
// Names.
// ---------------------------------------------------------------------------

// WorkflowType is the name the engine registers this workflow under.
const WorkflowType = "TravelAgent"

const (
	// ActivityPlan asks the language model what to do next. Its result is
	// journaled, which is why a crash *before* it completes is the interesting
	// one: the plan is lost and has to be made again from the transcript.
	ActivityPlan = "Plan"

	// ActivitySearchFlights and ActivityBookFlight are the tools. Their
	// arguments are natural language, so they are the semantic-mode activities.
	ActivitySearchFlights = "SearchFlights"
	ActivityBookFlight    = "BookFlight"

	// ActivityFinalize writes the answer back to the user.
	ActivityFinalize = "Finalize"
)

// SignalHumanApproval is the gate a person has to open before anything is
// actually booked.
const SignalHumanApproval = "HumanApproval"

// DefaultMaxIterations bounds the plan/tool loop. An agent that cannot make
// progress must stop and ask a person rather than spend money forever.
const DefaultMaxIterations = 4

// ---------------------------------------------------------------------------
// Inputs and outputs.
// ---------------------------------------------------------------------------

// Request is the workflow input.
type Request struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`

	// Goal is what the user asked for, in their own words.
	Goal string `json:"goal"`

	// MaxIterations bounds the plan/tool loop; zero means DefaultMaxIterations.
	MaxIterations int `json:"max_iterations,omitempty"`
}

// ToolCall is one tool invocation. Args is deliberately a plain string: it is
// the model's own phrasing of what it wants, and it is the value the semantic
// deduplication check embeds and compares.
type ToolCall struct {
	Tool string `json:"tool"`
	Args string `json:"args"`
}

// Turn is one completed plan-and-act cycle, kept so the next planning call can
// see what already happened.
type Turn struct {
	Step        int             `json:"step"`
	Thought     string          `json:"thought"`
	Call        ToolCall        `json:"call"`
	Observation json.RawMessage `json:"observation"`
}

// PlanInput is what the planner sees. It is the goal plus everything that has
// happened so far — and nothing else, which is why a recovered process can
// reach a differently-worded conclusion from the same facts.
type PlanInput struct {
	Goal       string `json:"goal"`
	Step       int    `json:"step"`
	Transcript []Turn `json:"transcript"`
}

// Plan is the planner's decision for one step.
type Plan struct {
	Step    int    `json:"step"`
	Thought string `json:"thought"`
	Tool    string `json:"tool,omitempty"`
	Args    string `json:"args,omitempty"`
	Done    bool   `json:"done"`
	Answer  string `json:"answer,omitempty"`
}

// Approval is the payload of the HumanApproval signal.
type Approval struct {
	Approved bool   `json:"approved"`
	Reviewer string `json:"reviewer"`
	Note     string `json:"note,omitempty"`
}

// FinalizeInput is handed to the last activity.
type FinalizeInput struct {
	RequestID  string   `json:"request_id"`
	Goal       string   `json:"goal"`
	Answer     string   `json:"answer"`
	Transcript []Turn   `json:"transcript"`
	Approval   Approval `json:"approval"`
}

// FinalizeResult is what the last activity returns.
type FinalizeResult struct {
	ConfirmationID string `json:"confirmation_id"`
	Summary        string `json:"summary"`
}

// Result is the workflow output.
type Result struct {
	RequestID      string    `json:"request_id"`
	Answer         string    `json:"answer"`
	ConfirmationID string    `json:"confirmation_id,omitempty"`
	Transcript     []Turn    `json:"transcript"`
	Iterations     int       `json:"iterations"`
	Approved       bool      `json:"approved"`
	Reviewer       string    `json:"reviewer,omitempty"`
	RejectionNote  string    `json:"rejection_note,omitempty"`
	BudgetExceeded bool      `json:"budget_exceeded"`
	FinishedAt     time.Time `json:"finished_at"`
}

// ---------------------------------------------------------------------------
// The workflow.
// ---------------------------------------------------------------------------

// Run is the workflow function: plan, call a tool, repeat up to N times, then
// wait for a person to approve and finalise.
func Run(ctx Ctx, req Request) (Result, error) {
	budget := req.MaxIterations
	if budget <= 0 {
		budget = DefaultMaxIterations
	}

	res := Result{RequestID: req.RequestID}
	var transcript []Turn
	var answer string
	finished := false

	for step := 0; step < budget; step++ {
		res.Iterations = step + 1

		var plan Plan
		if err := call(ctx, ActivityPlan, PlanInput{
			Goal:       req.Goal,
			Step:       step,
			Transcript: transcript,
		}, &plan); err != nil {
			return res, err
		}

		if plan.Done || plan.Tool == "" {
			answer = plan.Answer
			finished = true
			break
		}

		// The tool call. Its input carries the model's own words, and those
		// words are what the semantic check will compare against every earlier
		// call to the same tool in this run.
		observation, err := ctx.ExecuteActivity(plan.Tool, ToolCall{
			Tool: plan.Tool,
			Args: plan.Args,
		}).Get()
		if err != nil {
			return res, err
		}

		transcript = append(transcript, Turn{
			Step:        step,
			Thought:     plan.Thought,
			Call:        ToolCall{Tool: plan.Tool, Args: plan.Args},
			Observation: observation,
		})
	}

	res.Transcript = transcript
	res.Answer = answer
	// Running out of steps is not an error, but it is a fact the reviewer
	// needs, so it travels with the result to the approval gate.
	res.BudgetExceeded = !finished

	// The human gate. This is a durable wait: the workflow stops here and the
	// process is free to exit. Whichever node owns the shard when the signal
	// finally arrives resumes from the journal.
	approval, err := waitForApproval(ctx)
	if err != nil {
		return res, err
	}
	res.Approved = approval.Approved
	res.Reviewer = approval.Reviewer

	if !approval.Approved {
		// A rejection is a legitimate outcome, not an engine failure.
		res.RejectionNote = approval.Note
		res.FinishedAt = ctx.Now()
		return res, nil
	}

	var final FinalizeResult
	if err := call(ctx, ActivityFinalize, FinalizeInput{
		RequestID:  req.RequestID,
		Goal:       req.Goal,
		Answer:     answer,
		Transcript: transcript,
		Approval:   approval,
	}, &final); err != nil {
		return res, err
	}
	res.ConfirmationID = final.ConfirmationID
	if final.Summary != "" {
		res.Answer = final.Summary
	}
	res.FinishedAt = ctx.Now()

	return res, nil
}

// RunJSON adapts Run to the engine's generic workflow signature.
func RunJSON(ctx Ctx, input json.RawMessage) (json.RawMessage, error) {
	var req Request
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, &core.NonRetryableError{Cause: fmt.Errorf("agent: bad workflow input: %w", err)}
	}
	res, err := Run(ctx, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func waitForApproval(ctx Ctx) (Approval, error) {
	raw, err := ctx.WaitForSignal(SignalHumanApproval).Get()
	if err != nil {
		return Approval{}, err
	}
	var a Approval
	if len(raw) == 0 || string(raw) == "null" {
		return a, nil
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, &core.NonRetryableError{
			Cause: fmt.Errorf("agent: undecodable %s signal: %w", SignalHumanApproval, err),
		}
	}
	return a, nil
}

func call(ctx Ctx, activityType string, input any, out any) error {
	raw, err := ctx.ExecuteActivity(activityType, input).Get()
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &core.NonRetryableError{
			Cause: fmt.Errorf("agent: %s returned undecodable result: %w", activityType, err),
		}
	}
	return nil
}
