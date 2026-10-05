// Package workflow is the deterministic heart of the engine.
//
// A workflow is an ordinary Go function. It calls activities, sleeps, waits for
// signals and returns a result, and it reads as if all of that happened in one
// uninterrupted run. It did not. The function is executed many times — once for
// every "workflow task" the engine schedules — and each execution starts again
// at line one. What makes that work is that every decision the function makes
// must depend only on its input and on the recorded history of the run, never
// on the wall clock, a random number, a map iteration order, or anything else
// that could differ between two executions.
//
// The engine enforces that in two ways. First, Context is the only thing a
// workflow is handed, and there is no method on it that performs I/O; anything
// that touches the outside world has to become an activity, whose result is
// written to history before the workflow is allowed to see it. Second, when a
// re-execution asks for something different from what history records, the
// engine refuses to guess and returns a *core.NonDeterminismError.
//
// Nothing in this package opens a connection or reads a clock. It takes a slice
// of core.Event in and produces a slice of core.Command out, which is exactly
// why deterministic replay can be tested with no database at all.
package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Fn is the signature every workflow function has. Input and output are raw
// JSON rather than concrete Go types because the engine stores them in history
// and must be able to hand identical bytes back on every replay; a typed value
// would have to be re-marshalled, and marshalling is not guaranteed to be
// byte-stable across Go versions or struct changes.
type Fn func(ctx Context, input json.RawMessage) (json.RawMessage, error)

// NotRegisteredError means this worker has never heard of the workflow type it
// was asked to run. That is usually a deployment problem — a task for a new
// workflow type reached an old binary — so it is reported rather than retried
// into oblivion.
type NotRegisteredError struct{ WorkflowType string }

func (e *NotRegisteredError) Error() string {
	return fmt.Sprintf("workflow type %q is not registered", e.WorkflowType)
}

// AlreadyRegisteredError is returned rather than panicking so that a worker
// assembling its registry from several packages can report the clash cleanly.
type AlreadyRegisteredError struct{ WorkflowType string }

func (e *AlreadyRegisteredError) Error() string {
	return fmt.Sprintf("workflow type %q is already registered", e.WorkflowType)
}

// Registry maps a workflow type name to its implementation.
//
// The name, not the function pointer, is what is written to history, because a
// run started today may be replayed by a binary compiled next year. That also
// means renaming a registered workflow type breaks every in-flight run of it.
type Registry struct {
	mu  sync.RWMutex
	fns map[string]Fn
}

func NewRegistry() *Registry {
	return &Registry{fns: make(map[string]Fn)}
}

// Register adds a workflow. Registration is guarded by a mutex only because
// workers may build their registry from init functions across packages; it is
// not on any hot path.
func (r *Registry) Register(name string, fn Fn) error {
	if name == "" {
		return fmt.Errorf("workflow: cannot register an unnamed workflow")
	}
	if fn == nil {
		return fmt.Errorf("workflow: cannot register %q with a nil function", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.fns[name]; exists {
		return &AlreadyRegisteredError{WorkflowType: name}
	}
	r.fns[name] = fn
	return nil
}

// MustRegister is the form used from package initialisers, where there is no
// caller left to hand an error to.
func (r *Registry) MustRegister(name string, fn Fn) {
	if err := r.Register(name, fn); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(name string) (Fn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.fns[name]
	return fn, ok
}

// Names returns the registered types in sorted order, for the admin endpoint
// and for startup logging. Sorted rather than map order so the output does not
// jitter between runs.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.fns))
	for name := range r.fns {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
