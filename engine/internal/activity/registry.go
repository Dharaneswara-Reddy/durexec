package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/dharaneswar339/durexec/internal/core"
)

// Fn is an activity implementation: the only place in the engine where code is
// allowed to talk to the outside world.
//
// It takes a real context.Context, unlike a workflow, because an activity is
// meant to do I/O and I/O needs cancellation and deadlines. The context carries
// an Info value with the dedup key for this call — see InfoFromContext.
type Fn func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

// Definition is everything the engine knows about one activity.
type Definition struct {
	// Name is what workflow code asks for and what is written into history.
	Name string

	// Fn is the implementation.
	Fn Fn

	// Retry governs failures. A zero policy means DefaultRetryPolicy.
	Retry RetryPolicy

	// Mode is the idempotency check applied before scheduling. Exact is the
	// default and compares dedup keys; Semantic is opt-in because it asks a
	// model whether two differently-worded requests mean the same thing, and
	// being wrong about that is expensive in a way an exact match cannot be.
	Mode core.IdempotencyMode

	// Compensation names the activity that undoes this one — "RefundPayment"
	// for "ChargeCard". It is a name rather than a function because a
	// compensation is run as an ordinary activity through the engine, so that
	// rolling back is journaled, retried and crash-safe in exactly the same way
	// the forward step was. A rollback run in a Go defer would be none of those
	// things: a crash mid-rollback would simply lose it.
	//
	// Empty means the activity has nothing to undo, which is the right answer
	// for anything read-only.
	Compensation string
}

// NotRegisteredError is defined in retry.go and returned by Registry.Get's
// callers; kept there so the error taxonomy of the package sits in one file.

// AlreadyRegisteredError is returned rather than panicking so a worker
// assembling its registry from several packages can report the clash cleanly.
type AlreadyRegisteredError struct{ ActivityType string }

func (e *AlreadyRegisteredError) Error() string {
	return fmt.Sprintf("activity %q is already registered", e.ActivityType)
}

// Registry maps an activity name to its definition. Like the workflow registry,
// the name is the durable identifier: it is what history records, so renaming a
// registered activity breaks every in-flight run that has scheduled one.
type Registry struct {
	mu   sync.RWMutex
	defs map[string]Definition
}

func NewRegistry() *Registry {
	return &Registry{defs: make(map[string]Definition)}
}

// Register adds an activity, filling in the defaults so that the rest of the
// package never has to test for an unset mode or a zero policy.
func (r *Registry) Register(d Definition) error {
	if d.Name == "" {
		return fmt.Errorf("activity: cannot register an unnamed activity")
	}
	if d.Fn == nil {
		return fmt.Errorf("activity: cannot register %q with a nil function", d.Name)
	}
	if d.Mode == "" {
		// Exact is the default because it is the safe one: it only ever
		// suppresses a call it can prove is a repeat. Semantic matching has to
		// be asked for deliberately.
		d.Mode = core.ModeExact
	}
	if d.Retry.IsZero() {
		d.Retry = DefaultRetryPolicy()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.defs[d.Name]; exists {
		return &AlreadyRegisteredError{ActivityType: d.Name}
	}
	r.defs[d.Name] = d
	return nil
}

// MustRegister is the form used from package initialisers.
func (r *Registry) MustRegister(d Definition) {
	if err := r.Register(d); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(name string) (Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.defs[name]
	return d, ok
}

// Mode is shaped to be handed straight to workflow.Dispatcher.ModeFor. An
// unknown activity reports Exact rather than failing, because the dispatcher's
// job at that moment is to record an intention; discovering that no worker
// implements it is the activity path's problem, and it has a better error for
// it.
func (r *Registry) Mode(name string) core.IdempotencyMode {
	if d, ok := r.Get(name); ok && d.Mode != "" {
		return d.Mode
	}
	return core.ModeExact
}

// Compensation returns the name of the activity that undoes this one.
func (r *Registry) Compensation(name string) (string, bool) {
	d, ok := r.Get(name)
	if !ok || d.Compensation == "" {
		return "", false
	}
	return d.Compensation, true
}

// Names returns the registered activities in sorted order, so startup logs and
// the admin endpoint do not jitter between runs.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.defs))
	for name := range r.defs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
