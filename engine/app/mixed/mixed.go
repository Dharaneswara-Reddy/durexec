// Package mixed is a set of workflow SHAPES for evaluating the engine with
// more than one kind of work at once. The delivery saga exercises a linear
// chain with compensations and a timer; these add the shapes it does not:
//
//	MixPipeline   a long sequential chain (20 activities by default), so the
//	              history and its replay grow long
//	MixFanOut     many activities started together and joined, so one
//	              decision schedules many tasks
//	MixApproval   a human-in-the-loop step: the run waits for a signal from
//	              outside before it can finish
//	MixRetry      activities that fail and are retried with backoff before
//	              they succeed
//	DomainAgent   an agent that plans an action, executes it, re-plans after
//	              a simulated loss and issues the action again in other words,
//	              across forty kinds of task; the engine's duplicate check
//	              decides whether the second call runs
//
// All activities simulate work with a short sleep; none calls anything real.
package mixed

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// Workflow type names.
const (
	WorkflowPipeline = "MixPipeline"
	WorkflowFanOut   = "MixFanOut"
	WorkflowApproval = "MixApproval"
	WorkflowRetry    = "MixRetry"
	WorkflowAgent    = "DomainAgent"
)

// Activity names.
const (
	ActStep   = "MixStep"
	ActMerge  = "MixMerge"
	ActSubmit = "MixSubmit"
	ActApply  = "MixApply"
	ActFlaky  = "MixFlaky"
	ActPlan   = "MixPlan"
)

// SignalApprove is the signal MixApproval waits for.
const SignalApprove = "approve"

// AgentToolPrefix prefixes the agent's commit tools, so "AgentBookFlight" is
// the commit tool for the flight domain. The prefix keeps them apart from the
// travel demo's own BookFlight.
const AgentToolPrefix = "Agent"

// Input is shared by the shapes; each reads the fields it needs.
type Input struct {
	Steps     int    `json:"steps,omitempty"`      // pipeline length or fan-out width
	WorkMS    int    `json:"work_ms,omitempty"`    // simulated work per activity
	FailTimes int    `json:"fail_times,omitempty"` // MixRetry: failures before success
	Calls     int    `json:"calls,omitempty"`      // MixRetry: how many flaky calls
	Tool      string `json:"tool,omitempty"`       // DomainAgent: commit tool
	First     string `json:"first,omitempty"`      // DomainAgent: the action as first planned
	Second    string `json:"second,omitempty"`     // DomainAgent: the action as re-planned
}

// ToolCall is the commit tool's input; Args is the sentence the duplicate
// check compares.
type ToolCall struct {
	Args string `json:"args"`
}

// AgentResult reports whether the re-planned call reused the first result.
type AgentResult struct {
	FirstConfirmation  string `json:"first_confirmation"`
	SecondConfirmation string `json:"second_confirmation"`
	Suppressed         bool   `json:"suppressed"`
}

func read(raw json.RawMessage) (Input, error) {
	var in Input
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return in, fmt.Errorf("mixed: bad input: %w", err)
		}
	}
	return in, nil
}

func stepInput(in Input, i int) map[string]int { return map[string]int{"i": i, "work_ms": in.WorkMS} }

// Pipeline runs Steps activities one after another.
func Pipeline(ctx workflow.Context, raw json.RawMessage) (json.RawMessage, error) {
	in, err := read(raw)
	if err != nil {
		return nil, err
	}
	if in.Steps <= 0 {
		in.Steps = 20
	}
	for i := 0; i < in.Steps; i++ {
		if _, err := ctx.ExecuteActivity(ActStep, stepInput(in, i)).Get(); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]int{"steps": in.Steps})
}

// FanOut starts Steps activities together, waits for all, then merges.
func FanOut(ctx workflow.Context, raw json.RawMessage) (json.RawMessage, error) {
	in, err := read(raw)
	if err != nil {
		return nil, err
	}
	if in.Steps <= 0 {
		in.Steps = 8
	}
	futs := make([]workflow.Future, in.Steps)
	for i := range futs {
		futs[i] = ctx.ExecuteActivity(ActStep, stepInput(in, i))
	}
	for _, f := range futs {
		if _, err := f.Get(); err != nil {
			return nil, err
		}
	}
	if _, err := ctx.ExecuteActivity(ActMerge, map[string]int{"n": in.Steps, "work_ms": in.WorkMS}).Get(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int{"width": in.Steps})
}

// Approval submits a request, waits for the approve signal, then applies it.
func Approval(ctx workflow.Context, raw json.RawMessage) (json.RawMessage, error) {
	in, err := read(raw)
	if err != nil {
		return nil, err
	}
	if _, err := ctx.ExecuteActivity(ActSubmit, stepInput(in, 0)).Get(); err != nil {
		return nil, err
	}
	decision, err := ctx.WaitForSignal(SignalApprove).Get()
	if err != nil {
		return nil, err
	}
	if _, err := ctx.ExecuteActivity(ActApply, stepInput(in, 1)).Get(); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"decision": decision})
}

// Retry makes Calls activities that each fail FailTimes times first.
func Retry(ctx workflow.Context, raw json.RawMessage) (json.RawMessage, error) {
	in, err := read(raw)
	if err != nil {
		return nil, err
	}
	if in.Calls <= 0 {
		in.Calls = 3
	}
	if in.FailTimes <= 0 {
		in.FailTimes = 2
	}
	for i := 0; i < in.Calls; i++ {
		if _, err := ctx.ExecuteActivity(ActFlaky, map[string]int{"i": i, "fail_times": in.FailTimes, "work_ms": in.WorkMS}).Get(); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]int{"calls": in.Calls})
}

// Agent executes the planned action, re-plans, and issues the action again as
// re-worded. Whether the second call runs is the engine's duplicate check's
// decision, journaled in history.
func Agent(ctx workflow.Context, raw json.RawMessage) (json.RawMessage, error) {
	in, err := read(raw)
	if err != nil {
		return nil, err
	}
	tool := AgentToolPrefix + in.Tool
	first, err := ctx.ExecuteActivity(tool, ToolCall{Args: in.First}).Get()
	if err != nil {
		return nil, err
	}
	if _, err := ctx.ExecuteActivity(ActPlan, stepInput(in, 1)).Get(); err != nil {
		return nil, err
	}
	second, err := ctx.ExecuteActivity(tool, ToolCall{Args: in.Second}).Get()
	if err != nil {
		return nil, err
	}
	var a, b struct {
		Confirmation string `json:"confirmation"`
	}
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	return json.Marshal(AgentResult{FirstConfirmation: a.Confirmation, SecondConfirmation: b.Confirmation,
		Suppressed: a.Confirmation != "" && a.Confirmation == b.Confirmation})
}

// ---------------------------------------------------------------------------
// Activities.
// ---------------------------------------------------------------------------

func work(ctx context.Context, raw json.RawMessage) error {
	var p struct {
		WorkMS int `json:"work_ms"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.WorkMS <= 0 {
		return nil
	}
	select {
	case <-time.After(time.Duration(p.WorkMS) * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func simple(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := work(ctx, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// ErrTransient is what MixFlaky returns before it succeeds.
var ErrTransient = fmt.Errorf("mixed: simulated transient failure")

func flaky(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var p struct {
		FailTimes int `json:"fail_times"`
	}
	_ = json.Unmarshal(raw, &p)
	info, _ := activity.InfoFromContext(ctx)
	if info.Attempt <= p.FailTimes {
		return nil, ErrTransient
	}
	return simple(ctx, raw)
}

// commit is every agent tool: it "performs" the action and returns a
// confirmation derived from the dedup key, which is unique per scheduled
// call. If the duplicate check suppresses a call, the workflow receives the
// earlier call's confirmation instead and no second confirmation is ever made.
func commit(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if err := work(ctx, json.RawMessage(`{"work_ms":20}`)); err != nil {
		return nil, err
	}
	info, _ := activity.InfoFromContext(ctx)
	return json.Marshal(map[string]string{"confirmation": "cf-" + info.DedupKey})
}

// IntentText renders an agent tool call as the sentence the duplicate check
// compares, lower-cased and single-spaced as the agent's own tools do.
func IntentText(raw json.RawMessage) (string, error) {
	var c ToolCall
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("mixed: cannot read tool call: %w", err)
	}
	return canonical(c.Args), nil
}

func canonical(s string) string {
	out := make([]rune, 0, len(s))
	space := true
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if !space {
				out = append(out, ' ')
			}
			space = true
			continue
		}
		space = false
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	if len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// Registration describes one activity for the node's registry.
type Registration struct {
	Type       string
	Mode       core.IdempotencyMode
	Fn         activity.Fn
	Retry      activity.RetryPolicy
	IntentText func(json.RawMessage) (string, error)
}

// Registrations lists every activity of the mixed shapes, with one commit
// tool per agent domain in tools.
func Registrations(tools []string) []Registration {
	fast := activity.RetryPolicy{InitialInterval: 200 * time.Millisecond, BackoffCoefficient: 2,
		MaximumInterval: 2 * time.Second, MaximumAttempts: 6}
	regs := []Registration{
		{Type: ActStep, Mode: core.ModeExact, Fn: simple},
		{Type: ActMerge, Mode: core.ModeExact, Fn: simple},
		{Type: ActSubmit, Mode: core.ModeExact, Fn: simple},
		{Type: ActApply, Mode: core.ModeExact, Fn: simple},
		{Type: ActPlan, Mode: core.ModeExact, Fn: simple},
		{Type: ActFlaky, Mode: core.ModeExact, Fn: flaky, Retry: fast},
	}
	for _, t := range tools {
		regs = append(regs, Registration{Type: AgentToolPrefix + t, Mode: core.ModeSemantic, Fn: commit, IntentText: IntentText})
	}
	return regs
}

// Workflows lists the workflow functions by type name.
func Workflows() map[string]workflow.Fn {
	return map[string]workflow.Fn{
		WorkflowPipeline: Pipeline,
		WorkflowFanOut:   FanOut,
		WorkflowApproval: Approval,
		WorkflowRetry:    Retry,
		WorkflowAgent:    Agent,
	}
}
