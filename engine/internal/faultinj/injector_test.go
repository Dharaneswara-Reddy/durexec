package faultinj

import (
	"errors"
	"sync"
	"testing"

	"github.com/dharaneswar339/durexec/internal/core"
)

func TestCheckFiresExactlyTheConfiguredNumberOfTimes(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		arm        int
		calls      int
		wantErrors int
	}{
		{name: "disabled injector never fires", enabled: false, arm: 3, calls: 5, wantErrors: 0},
		{name: "not armed", enabled: true, arm: 0, calls: 5, wantErrors: 0},
		{name: "once", enabled: true, arm: 1, calls: 5, wantErrors: 1},
		{name: "three of five", enabled: true, arm: 3, calls: 5, wantErrors: 3},
		{name: "fewer calls than armed", enabled: true, arm: 4, calls: 2, wantErrors: 2},
		{name: "negative disarms", enabled: true, arm: -1, calls: 3, wantErrors: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inj := New(tc.enabled)
			inj.FailNext("ChargePayment", tc.arm)

			got := 0
			for n := 0; n < tc.calls; n++ {
				if err := inj.Check("ChargePayment"); err != nil {
					got++
					if !IsInjected(err) {
						t.Fatalf("call %d: error %v is not recognised as injected", n, err)
					}
				}
			}
			if got != tc.wantErrors {
				t.Fatalf("fired %d times, want %d", got, tc.wantErrors)
			}
			if inj.Fired() != int64(tc.wantErrors) {
				t.Fatalf("Fired()=%d, want %d", inj.Fired(), tc.wantErrors)
			}
			// Once every armed failure has been consumed the injector must
			// fall back to its zero-overhead state.
			if tc.calls >= tc.arm && inj.Armed() {
				t.Fatalf("injector still armed after exhausting the fault")
			}
		})
	}
}

func TestCheckOnlyAffectsTheNamedActivity(t *testing.T) {
	inj := New(true)
	inj.FailNext("NotifyRestaurant", 2)

	if err := inj.Check("ChargePayment"); err != nil {
		t.Fatalf("unrelated activity failed: %v", err)
	}
	if err := inj.Check("NotifyRestaurant"); err == nil {
		t.Fatal("armed activity did not fail")
	}
	if err := inj.Check("NotifyRestaurant"); err == nil {
		t.Fatal("armed activity did not fail on its second call")
	}
	if err := inj.Check("NotifyRestaurant"); err != nil {
		t.Fatalf("armed activity fired a third time: %v", err)
	}
}

func TestFaultErrorClassification(t *testing.T) {
	inj := New(true)
	inj.FailNext("A", 1)
	inj.FailNextFatal("B", 1)

	errA := inj.Check("A")
	var retryable *core.RetryableError
	if !errors.As(errA, &retryable) {
		t.Fatalf("FailNext should produce a retryable error, got %#v", errA)
	}

	errB := inj.Check("B")
	if !core.IsNonRetryable(errB) {
		t.Fatalf("FailNextFatal should produce a non-retryable error, got %#v", errB)
	}

	var fault *Fault
	if !errors.As(errB, &fault) || fault.ActivityType != "B" {
		t.Fatalf("expected a *Fault naming activity B, got %#v", errB)
	}
}

func TestCrashPointsArmAndDisarm(t *testing.T) {
	inj := New(true)

	if inj.ShouldCrash(AfterActivityJournaled) {
		t.Fatal("crashed with nothing armed")
	}

	inj.CrashAt(AfterActivityJournaled)
	if !inj.ShouldCrash(AfterActivityJournaled) {
		t.Fatal("armed crash point did not fire")
	}
	if inj.ShouldCrash(AfterActivityJournaled) {
		t.Fatal("crash point fired twice from a single arming")
	}

	inj.CrashAt(BeforeNextPlanCompletes)
	if inj.ShouldCrash(AfterActivityJournaled) {
		t.Fatal("wrong crash point fired")
	}
	if !inj.ShouldCrash(BeforeNextPlanCompletes) {
		t.Fatal("BeforeNextPlanCompletes did not fire")
	}
}

// A disabled injector must be inert even against direct arming calls, because
// that is the only thing standing between a stray FailNext and production.
func TestDisabledInjectorCannotBeArmed(t *testing.T) {
	inj := Disabled()
	inj.FailNext("ChargePayment", 10)
	inj.FailNextFatal("ChargePayment", 10)
	inj.CrashAt(BeforeDecisionCommit)

	if inj.Enabled() || inj.Armed() {
		t.Fatal("disabled injector reports itself as enabled or armed")
	}
	if err := inj.Check("ChargePayment"); err != nil {
		t.Fatalf("disabled injector fired: %v", err)
	}
	if inj.ShouldCrash(BeforeDecisionCommit) {
		t.Fatal("disabled injector crashed")
	}
}

func TestNilInjectorIsSafe(t *testing.T) {
	var inj *Injector
	if inj.Enabled() || inj.Armed() || inj.Fired() != 0 {
		t.Fatal("nil injector is not inert")
	}
	if err := inj.Check("anything"); err != nil {
		t.Fatalf("nil injector fired: %v", err)
	}
	if inj.ShouldCrash(AfterLeaseRenew) {
		t.Fatal("nil injector crashed")
	}
	inj.Reset()
}

func TestResetDisarms(t *testing.T) {
	inj := New(true)
	inj.FailNext("A", 5)
	inj.CrashAt(AfterLeaseRenew)
	inj.Reset()

	if inj.Armed() {
		t.Fatal("still armed after Reset")
	}
	if err := inj.Check("A"); err != nil {
		t.Fatalf("fired after Reset: %v", err)
	}
}

// The injector is consulted from every activity worker, so the total number of
// failures must be exact under concurrency, not merely approximate.
func TestConcurrentCheckFiresExactlyOnce(t *testing.T) {
	const goroutines = 64
	inj := New(true)
	inj.FailNext("A", 1)

	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0

	wg.Add(goroutines)
	for n := 0; n < goroutines; n++ {
		go func() {
			defer wg.Done()
			if err := inj.Check("A"); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if failures != 1 {
		t.Fatalf("fired %d times under concurrency, want exactly 1", failures)
	}
}
