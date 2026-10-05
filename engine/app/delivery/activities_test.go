package delivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// noSleep records what the simulated latency would have been and returns
// immediately, so the suite never actually waits.
func noSleep(recorded *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*recorded = append(*recorded, d)
		return nil
	}
}

func TestEveryDeliveryActivityIsExactMode(t *testing.T) {
	regs := NewActivities(Options{}).Registrations()
	if len(regs) != 7 {
		t.Fatalf("registered %d activities, want 7", len(regs))
	}
	for _, r := range regs {
		// Payment amounts and stock keeping units are identifiers and integers,
		// so similarity is the wrong question to ask about them.
		if r.Mode != core.ModeExact {
			t.Errorf("%s registered as %s, want %s", r.Type, r.Mode, core.ModeExact)
		}
		if r.IntentText != nil {
			t.Errorf("%s supplies intent text, which only semantic activities need", r.Type)
		}
		if r.Handler == nil {
			t.Errorf("%s has no handler", r.Type)
		}
	}
}

func TestActivitiesSpendTheirConfiguredLatency(t *testing.T) {
	var slept []time.Duration
	acts := NewActivities(Options{
		Latency: Latency{ActivityChargePayment: 250 * time.Millisecond},
		Sleep:   noSleep(&slept),
	})

	in, _ := json.Marshal(ChargePaymentInput{OrderID: "o1", AmountCents: 100, Currency: "INR"})
	if _, err := acts.ChargePayment(context.Background(), in); err != nil {
		t.Fatalf("ChargePayment: %v", err)
	}
	if len(slept) != 1 || slept[0] != 250*time.Millisecond {
		t.Fatalf("latency = %v, want one 250ms wait", slept)
	}
}

func TestActivitiesFailWhenAndOnlyWhenTheInjectorSaysSo(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		arm        string
		times      int
		fatal      bool
		wantFails  int
		wantErrIs  func(error) bool
		totalCalls int
	}{
		{name: "no injector", enabled: false, arm: "", times: 0, wantFails: 0, totalCalls: 3},
		{name: "injector disabled by config", enabled: false, arm: ActivityNotifyRestaurant, times: 3, wantFails: 0, totalCalls: 3},
		{name: "fails twice then recovers", enabled: true, arm: ActivityNotifyRestaurant, times: 2, wantFails: 2, totalCalls: 3},
		{
			name: "fatal failure is not retryable", enabled: true, arm: ActivityNotifyRestaurant,
			times: 1, fatal: true, wantFails: 1, totalCalls: 1,
			wantErrIs: core.IsNonRetryable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var slept []time.Duration
			inj := faultinj.New(tc.enabled)
			if tc.arm != "" {
				if tc.fatal {
					inj.FailNextFatal(tc.arm, tc.times)
				} else {
					inj.FailNext(tc.arm, tc.times)
				}
			}
			acts := NewActivities(Options{Faults: inj, Latency: Latency{}, Sleep: noSleep(&slept)})

			in, _ := json.Marshal(NotifyRestaurantInput{OrderID: "o1", RestaurantID: "r1"})
			fails := 0
			for n := 0; n < tc.totalCalls; n++ {
				_, err := acts.NotifyRestaurant(context.Background(), in)
				if err == nil {
					continue
				}
				fails++
				if !faultinj.IsInjected(err) {
					t.Fatalf("call %d failed for a reason other than injection: %v", n, err)
				}
				if tc.wantErrIs != nil && !tc.wantErrIs(err) {
					t.Fatalf("call %d: error has the wrong retry classification: %v", n, err)
				}
			}
			if fails != tc.wantFails {
				t.Fatalf("failed %d times, want %d", fails, tc.wantFails)
			}
		})
	}
}

func TestChargePaymentRejectsANonPositiveAmount(t *testing.T) {
	acts := NewActivities(Options{Latency: Latency{}})
	in, _ := json.Marshal(ChargePaymentInput{OrderID: "o1", AmountCents: 0, Currency: "INR"})

	_, err := acts.ChargePayment(context.Background(), in)
	if err == nil {
		t.Fatal("a zero-value charge was accepted")
	}
	// Retrying a validation error would loop forever; the workflow needs to
	// compensate instead.
	if !core.IsNonRetryable(err) {
		t.Fatalf("validation failure is retryable: %v", err)
	}
}

func TestRefundRefusesWithoutAPaymentID(t *testing.T) {
	acts := NewActivities(Options{Latency: Latency{}})
	in, _ := json.Marshal(RefundPaymentInput{OrderID: "o1"})

	_, err := acts.RefundPayment(context.Background(), in)
	if err == nil {
		t.Fatal("refunded without a payment id")
	}
	if !core.IsNonRetryable(err) {
		t.Fatalf("expected a non-retryable refusal, got %v", err)
	}
}

func TestActivitiesProduceDistinctIdentifiers(t *testing.T) {
	acts := NewActivities(Options{Latency: Latency{}})
	in, _ := json.Marshal(ReserveInventoryInput{OrderID: "o1"})

	seen := map[string]bool{}
	for n := 0; n < 5; n++ {
		raw, err := acts.ReserveInventory(context.Background(), in)
		if err != nil {
			t.Fatalf("ReserveInventory: %v", err)
		}
		var out ReserveInventoryResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if seen[out.HoldID] {
			t.Fatalf("duplicate hold id %q", out.HoldID)
		}
		seen[out.HoldID] = true
	}
}

func TestSleepRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); err == nil {
		t.Fatal("a cancelled context still waited")
	}
}
