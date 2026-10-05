package delivery

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// A fake Ctx. It records what the workflow asked for and hands back canned
// answers, so the workflow's control flow can be tested with no database, no
// network and no real time passing.
// ---------------------------------------------------------------------------

type fakeFuture struct {
	value json.RawMessage
	err   error
}

func (f fakeFuture) Get() (json.RawMessage, error) { return f.value, f.err }

type fakeCtx struct {
	// calls is the ordered list of activity types the workflow scheduled.
	calls []string
	// sleeps records durable timers rather than actually waiting.
	sleeps []time.Duration
	// waited records signal names the workflow blocked on.
	waited []string

	// failures maps an activity type to the error it should return.
	failures map[string]error
	// results overrides the canned success value for an activity type.
	results map[string]any

	clock time.Time
}

func newFakeCtx() *fakeCtx {
	return &fakeCtx{
		failures: map[string]error{},
		results:  map[string]any{},
		clock:    time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func (c *fakeCtx) ExecuteActivity(activityType string, _ any) Future {
	c.calls = append(c.calls, activityType)
	if err, ok := c.failures[activityType]; ok {
		return fakeFuture{err: err}
	}
	value, ok := c.results[activityType]
	if !ok {
		value = defaultResult(activityType)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fakeFuture{err: err}
	}
	return fakeFuture{value: raw}
}

func (c *fakeCtx) Sleep(d time.Duration) Future {
	c.sleeps = append(c.sleeps, d)
	// A durable timer resolves by advancing the workflow's own clock; no
	// wall-clock time passes in the test.
	c.clock = c.clock.Add(d)
	return fakeFuture{value: json.RawMessage(`null`)}
}

func (c *fakeCtx) WaitForSignal(name string) Future {
	c.waited = append(c.waited, name)
	return fakeFuture{value: json.RawMessage(`null`)}
}

func (c *fakeCtx) Now() time.Time { return c.clock }

func defaultResult(activityType string) any {
	switch activityType {
	case ActivityChargePayment:
		return ChargePaymentResult{PaymentID: "pay_1"}
	case ActivityReserveInventory:
		return ReserveInventoryResult{HoldID: "hold_1"}
	case ActivityNotifyRestaurant:
		return NotifyRestaurantResult{TicketID: "ticket_1"}
	case ActivityDispatchDriver:
		return DispatchDriverResult{DriverID: "driver_1", ETA: "18m"}
	default:
		return map[string]string{"ok": "true"}
	}
}

func testOrder() Order {
	return Order{
		OrderID:      "order-7",
		CustomerID:   "cust-3",
		RestaurantID: "rest-9",
		Items:        []LineItem{{SKU: "dosa", Quantity: 2}},
		AmountCents:  2450,
		Currency:     "INR",
		PrepTime:     5 * time.Minute,
	}
}

// ---------------------------------------------------------------------------

func TestWorkflowRunsItsFourStepsInOrder(t *testing.T) {
	ctx := newFakeCtx()

	res, err := Run(ctx, testOrder())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		ActivityChargePayment,
		ActivityReserveInventory,
		ActivityNotifyRestaurant,
		ActivityDispatchDriver,
	}
	if !reflect.DeepEqual(ctx.calls, want) {
		t.Fatalf("activity order = %v, want %v", ctx.calls, want)
	}
	if res.Compensated || res.FailedStep != "" {
		t.Fatalf("a successful order reported a failure: %+v", res)
	}
	if res.PaymentID != "pay_1" || res.HoldID != "hold_1" || res.TicketID != "ticket_1" || res.DriverID != "driver_1" {
		t.Fatalf("results not threaded through: %+v", res)
	}
}

// The wait between the kitchen accepting and a driver being sent must be a
// durable timer, not a goroutine sleeping in this process.
func TestWorkflowUsesADurableTimerBeforeDispatch(t *testing.T) {
	ctx := newFakeCtx()
	order := testOrder()
	order.PrepTime = 7 * time.Minute

	if _, err := Run(ctx, order); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ctx.sleeps) != 1 || ctx.sleeps[0] != 7*time.Minute {
		t.Fatalf("sleeps = %v, want one 7m durable timer", ctx.sleeps)
	}
}

func TestWorkflowUsesDefaultPrepTimeWhenUnset(t *testing.T) {
	ctx := newFakeCtx()
	order := testOrder()
	order.PrepTime = 0

	if _, err := Run(ctx, order); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ctx.sleeps) != 1 || ctx.sleeps[0] != defaultPrepTime {
		t.Fatalf("sleeps = %v, want one %s timer", ctx.sleeps, defaultPrepTime)
	}
}

// The core saga property: compensations run in the exact reverse of the order
// the forward steps succeeded in, and only for steps that actually happened.
func TestCompensationsRunInStrictLIFOOrder(t *testing.T) {
	boom := errors.New("simulated outage")

	tests := []struct {
		name            string
		failAt          string
		wantCalls       []string
		wantCompensated []string
	}{
		{
			name:   "payment fails, nothing to undo",
			failAt: ActivityChargePayment,
			wantCalls: []string{
				ActivityChargePayment,
			},
			wantCompensated: nil,
		},
		{
			name:   "inventory fails, refund only",
			failAt: ActivityReserveInventory,
			wantCalls: []string{
				ActivityChargePayment,
				ActivityReserveInventory,
				ActivityRefundPayment,
			},
			wantCompensated: []string{ActivityRefundPayment},
		},
		{
			name:   "restaurant fails, release then refund",
			failAt: ActivityNotifyRestaurant,
			wantCalls: []string{
				ActivityChargePayment,
				ActivityReserveInventory,
				ActivityNotifyRestaurant,
				ActivityReleaseInventory,
				ActivityRefundPayment,
			},
			wantCompensated: []string{ActivityReleaseInventory, ActivityRefundPayment},
		},
		{
			name:   "driver fails, cancel then release then refund",
			failAt: ActivityDispatchDriver,
			wantCalls: []string{
				ActivityChargePayment,
				ActivityReserveInventory,
				ActivityNotifyRestaurant,
				ActivityDispatchDriver,
				ActivityCancelRestaurantOrder,
				ActivityReleaseInventory,
				ActivityRefundPayment,
			},
			wantCompensated: []string{
				ActivityCancelRestaurantOrder,
				ActivityReleaseInventory,
				ActivityRefundPayment,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newFakeCtx()
			ctx.failures[tc.failAt] = boom

			res, err := Run(ctx, testOrder())
			if !errors.Is(err, boom) {
				t.Fatalf("Run returned %v, want the original failure", err)
			}
			if !reflect.DeepEqual(ctx.calls, tc.wantCalls) {
				t.Fatalf("call order =\n  %v\nwant\n  %v", ctx.calls, tc.wantCalls)
			}
			if res.FailedStep != tc.failAt {
				t.Errorf("FailedStep = %q, want %q", res.FailedStep, tc.failAt)
			}
			if len(tc.wantCompensated) == 0 {
				if len(res.CompensatedB) != 0 {
					t.Fatalf("undid %v when nothing had happened", res.CompensatedB)
				}
				return
			}
			if !res.Compensated {
				t.Error("Compensated = false after a successful rollback")
			}
			if !reflect.DeepEqual(res.CompensatedB, tc.wantCompensated) {
				t.Fatalf("compensations = %v, want %v", res.CompensatedB, tc.wantCompensated)
			}
		})
	}
}

// Registering a compensation before its forward step succeeded would refund
// money that was never taken. This asserts the money case directly.
func TestAFailedChargeIsNeverRefunded(t *testing.T) {
	ctx := newFakeCtx()
	ctx.failures[ActivityChargePayment] = errors.New("card declined")

	if _, err := Run(ctx, testOrder()); err == nil {
		t.Fatal("expected the declined charge to fail the workflow")
	}
	for _, c := range ctx.calls {
		if c == ActivityRefundPayment {
			t.Fatalf("issued a refund for a charge that never succeeded: %v", ctx.calls)
		}
	}
}

// A rollback that itself fails cannot be papered over: it becomes the error
// the caller sees, because it is the one that needs a person.
func TestFailedCompensationIsTerminal(t *testing.T) {
	ctx := newFakeCtx()
	ctx.failures[ActivityNotifyRestaurant] = errors.New("tablet offline")
	ctx.failures[ActivityReleaseInventory] = errors.New("stock service down")

	res, err := Run(ctx, testOrder())

	var compErr *core.CompensationFailedError
	if !errors.As(err, &compErr) {
		t.Fatalf("Run returned %#v, want *core.CompensationFailedError", err)
	}
	if compErr.Activity != ActivityReleaseInventory {
		t.Errorf("named activity = %q, want %q", compErr.Activity, ActivityReleaseInventory)
	}
	if compErr.WorkflowID != "order-7" {
		t.Errorf("WorkflowID = %q, want %q", compErr.WorkflowID, "order-7")
	}
	if res.Compensated {
		t.Error("Compensated = true after the rollback failed")
	}
	// The refund must not have been attempted: unwinding stopped where it broke.
	for _, c := range ctx.calls {
		if c == ActivityRefundPayment {
			t.Fatalf("kept unwinding past a failed compensation: %v", ctx.calls)
		}
	}
}

func TestRunJSONRoundTrip(t *testing.T) {
	ctx := newFakeCtx()
	in, err := json.Marshal(testOrder())
	if err != nil {
		t.Fatal(err)
	}

	raw, err := RunJSON(ctx, in)
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("result is not decodable: %v", err)
	}
	if res.OrderID != "order-7" || res.DriverID != "driver_1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestRunJSONRejectsBadInputWithoutRetrying(t *testing.T) {
	ctx := newFakeCtx()
	_, err := RunJSON(ctx, json.RawMessage(`{"amount_cents": "free"}`))
	if err == nil {
		t.Fatal("malformed input was accepted")
	}
	if !core.IsNonRetryable(err) {
		t.Fatalf("malformed input produced a retryable error: %v", err)
	}
	if len(ctx.calls) != 0 {
		t.Fatalf("scheduled activities for an undecodable order: %v", ctx.calls)
	}
}

// PlacedAt must come from the replay-safe clock, not from time.Now, or the
// same run would produce a different answer every time it replayed.
func TestPlacedAtComesFromTheWorkflowClock(t *testing.T) {
	ctx := newFakeCtx()
	want := ctx.clock

	res, err := Run(ctx, testOrder())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.PlacedAt.Equal(want) {
		t.Fatalf("PlacedAt = %s, want the workflow clock %s", res.PlacedAt, want)
	}
}
