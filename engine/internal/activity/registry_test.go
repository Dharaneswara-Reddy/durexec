package activity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

func noopFn(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage("null"), nil
}

func TestRegistryDefaults(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Definition{Name: "Charge", Fn: noopFn}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d, ok := r.Get("Charge")
	if !ok {
		t.Fatal("Charge not found")
	}
	// Exact is the default because it is the safe one: it only suppresses a
	// call it can prove is a repeat. Semantic matching has to be asked for.
	if d.Mode != core.ModeExact {
		t.Fatalf("mode = %q, want Exact by default", d.Mode)
	}
	if d.Retry != DefaultRetryPolicy() {
		t.Fatalf("retry = %+v, want the default policy", d.Retry)
	}
}

func TestRegistryKeepsExplicitValues(t *testing.T) {
	r := NewRegistry()
	custom := RetryPolicy{InitialInterval: 5 * time.Second, BackoffCoefficient: 3, MaximumAttempts: 2}
	if err := r.Register(Definition{
		Name:         "ChargeCard",
		Fn:           noopFn,
		Retry:        custom,
		Mode:         core.ModeSemantic,
		Compensation: "RefundPayment",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d, _ := r.Get("ChargeCard")
	if d.Retry != custom || d.Mode != core.ModeSemantic {
		t.Fatalf("explicit values were overwritten: %+v", d)
	}
	if got := r.Mode("ChargeCard"); got != core.ModeSemantic {
		t.Fatalf("Mode() = %q", got)
	}
	comp, ok := r.Compensation("ChargeCard")
	if !ok || comp != "RefundPayment" {
		t.Fatalf("Compensation() = %q, %v", comp, ok)
	}
}

func TestRegistryRejections(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Definition{Fn: noopFn}); err == nil {
		t.Error("an unnamed activity must be rejected")
	}
	if err := r.Register(Definition{Name: "Charge"}); err == nil {
		t.Error("an activity with no function must be rejected")
	}
	if err := r.Register(Definition{Name: "Charge", Fn: noopFn}); err != nil {
		t.Fatal(err)
	}
	var already *AlreadyRegisteredError
	if err := r.Register(Definition{Name: "Charge", Fn: noopFn}); !errors.As(err, &already) {
		t.Errorf("expected *AlreadyRegisteredError, got %v", err)
	}
}

// TestRegistryModeForUnknownActivity: Mode is shaped to be handed straight to
// workflow.Dispatcher.ModeFor, which must never fail on the replay path.
func TestRegistryModeForUnknownActivity(t *testing.T) {
	if got := NewRegistry().Mode("Ghost"); got != core.ModeExact {
		t.Fatalf("Mode(unknown) = %q, want the safe default Exact", got)
	}
	if _, ok := NewRegistry().Compensation("Ghost"); ok {
		t.Fatal("an unknown activity has no compensation")
	}
}

func TestRegistryNamesAreSorted(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"Ship", "Charge", "Reserve"} {
		if err := r.Register(Definition{Name: n, Fn: noopFn}); err != nil {
			t.Fatal(err)
		}
	}
	got := r.Names()
	want := []string{"Charge", "Reserve", "Ship"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}
