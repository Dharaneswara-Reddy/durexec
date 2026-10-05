package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Planner is the thing that decides what the agent does next. In production it
// is a language model; in the evaluation it is a script, so the interesting
// failure can be reproduced on demand instead of waited for.
type Planner interface {
	Plan(ctx context.Context, in PlanInput) (Plan, error)
}

// ErrScriptExhausted means a ScriptedPlanner was asked for more steps than it
// was given. It is an error rather than a silent "done" so a test that drifts
// from its fixture fails loudly instead of quietly passing.
var ErrScriptExhausted = errors.New("scripted planner: no more steps in the script")

// ScriptedPlanner replays a fixed list of plans, one per call.
//
// The important property is what it does NOT guarantee. It is deterministic
// within one process, and deliberately not identical across processes: each
// incarnation gets its own planner with its own script. That is what makes it
// a faithful stand-in for a language model, which will happily answer the same
// question twice with different words, and it is the whole reason the engine
// cannot rely on byte-equal arguments to recognise a repeated intent.
type ScriptedPlanner struct {
	// Name identifies the incarnation in test failures and demo output.
	Name string

	mu     sync.Mutex
	script []Plan
	calls  int
}

// NewScriptedPlanner returns a planner that will hand out plans in order.
func NewScriptedPlanner(name string, plans ...Plan) *ScriptedPlanner {
	script := make([]Plan, len(plans))
	copy(script, plans)
	return &ScriptedPlanner{Name: name, script: script}
}

// Plan returns the next scripted step. The transcript is ignored on purpose:
// the script already encodes what a model would have concluded from it.
func (p *ScriptedPlanner) Plan(_ context.Context, in PlanInput) (Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.calls >= len(p.script) {
		return Plan{}, fmt.Errorf("%w: %q was asked for step %d but holds only %d",
			ErrScriptExhausted, p.Name, p.calls, len(p.script))
	}
	next := p.script[p.calls]
	p.calls++
	if next.Step == 0 {
		next.Step = in.Step
	}
	return next, nil
}

// Calls reports how many plans this incarnation has handed out.
func (p *ScriptedPlanner) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Remaining reports how many scripted steps are left.
func (p *ScriptedPlanner) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.script) - p.calls
}

// Reset rewinds the script so the same planner can drive the demo again.
func (p *ScriptedPlanner) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = 0
}

// CanonicalIntent normalises a tool argument before it is embedded.
//
// It only removes differences that carry no meaning — surrounding space, runs
// of whitespace, letter case. It deliberately does not try to normalise words:
// mapping "BLR" to "Bangalore" with a lookup table is exactly the hand-written
// rule this project argues does not scale, and the whole point is that the
// embedding should find that equivalence on its own.
func CanonicalIntent(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
