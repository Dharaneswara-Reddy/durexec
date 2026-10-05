package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// Registration tells the engine how to treat one activity: what to call it,
// how to run it, and — the part that matters for this project — which
// idempotency rule applies when the same activity is scheduled twice.
//
// This type is declared in both demonstration packages because the engine's
// own registry is being written in parallel. The integrator should hoist one
// copy into the engine and delete the other.
type Registration struct {
	Type string
	Mode core.IdempotencyMode

	// Handler does the work. It receives the raw scheduled input.
	Handler func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

	// IntentText renders the input as the one natural-language sentence the
	// semantic check should compare. It is nil for exact-mode activities,
	// which need no embedding at all. No delivery activity sets it.
	IntentText func(input json.RawMessage) (string, error)
}

// Latency is the simulated round-trip time of each external service. Values
// are per activity type so the demo can make one dependency slow without
// slowing the whole workflow down.
type Latency map[string]time.Duration

// DefaultLatency is a plausible spread: a card network is slow, an internal
// stock service is fast, a restaurant tablet is somewhere in between.
func DefaultLatency() Latency {
	return Latency{
		ActivityChargePayment:         220 * time.Millisecond,
		ActivityReserveInventory:      40 * time.Millisecond,
		ActivityNotifyRestaurant:      120 * time.Millisecond,
		ActivityDispatchDriver:        180 * time.Millisecond,
		ActivityRefundPayment:         240 * time.Millisecond,
		ActivityReleaseInventory:      40 * time.Millisecond,
		ActivityCancelRestaurantOrder: 120 * time.Millisecond,
	}
}

// Options configures the simulated external services.
type Options struct {
	// Faults is the deliberate-failure switch. Nil, or an injector that
	// configuration never enabled, means the activities always succeed.
	Faults *faultinj.Injector

	// Latency overrides DefaultLatency. A missing entry means no delay.
	Latency Latency

	// Sleep is how simulated latency is spent. Tests supply a no-op so the
	// suite runs instantly; production leaves it nil and gets a real wait
	// that respects context cancellation.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Activities holds the simulated external services for one node.
type Activities struct {
	opts Options

	// seq makes the generated identifiers unique without pulling in a UUID
	// dependency. Activity results are journaled, so a replayed workflow sees
	// the first value forever regardless of what a re-execution would produce.
	seq atomic.Uint64
}

// NewActivities builds the delivery activity set.
func NewActivities(opts Options) *Activities {
	if opts.Latency == nil {
		opts.Latency = DefaultLatency()
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	return &Activities{opts: opts}
}

// Registrations is what the engine's worker registers at startup.
//
// Every delivery activity is core.ModeExact, and that is a deliberate
// statement rather than a default nobody thought about. These arguments are
// identifiers and integers: an order id, a stock keeping unit, an amount in
// cents. Two invocations either carry the same bytes or they do not, and the
// answer is never a matter of degree. Running a similarity model over
// "amount_cents: 2450" would add a probability to a question that already has
// a certain answer, and the failure mode would be a charge silently skipped
// because it looked close enough to another one. Semantic matching is for
// arguments written in a human language; money is not one.
func (a *Activities) Registrations() []Registration {
	return []Registration{
		{Type: ActivityChargePayment, Mode: core.ModeExact, Handler: a.ChargePayment},
		{Type: ActivityReserveInventory, Mode: core.ModeExact, Handler: a.ReserveInventory},
		{Type: ActivityNotifyRestaurant, Mode: core.ModeExact, Handler: a.NotifyRestaurant},
		{Type: ActivityDispatchDriver, Mode: core.ModeExact, Handler: a.DispatchDriver},

		{Type: ActivityRefundPayment, Mode: core.ModeExact, Handler: a.RefundPayment},
		{Type: ActivityReleaseInventory, Mode: core.ModeExact, Handler: a.ReleaseInventory},
		{Type: ActivityCancelRestaurantOrder, Mode: core.ModeExact, Handler: a.CancelRestaurantOrder},
	}
}

// ---------------------------------------------------------------------------
// Forward activities.
// ---------------------------------------------------------------------------

func (a *Activities) ChargePayment(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in ChargePaymentInput
	if err := decode(ActivityChargePayment, raw, &in); err != nil {
		return nil, err
	}
	if in.AmountCents <= 0 {
		// A validation problem will never fix itself, so it must not be
		// retried — the workflow needs to compensate, not loop.
		return nil, &core.NonRetryableError{
			Cause: fmt.Errorf("ChargePayment: amount must be positive, got %d", in.AmountCents),
		}
	}
	if err := a.callExternal(ctx, ActivityChargePayment); err != nil {
		return nil, err
	}
	return encode(ChargePaymentResult{PaymentID: a.id("pay")})
}

func (a *Activities) ReserveInventory(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in ReserveInventoryInput
	if err := decode(ActivityReserveInventory, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivityReserveInventory); err != nil {
		return nil, err
	}
	return encode(ReserveInventoryResult{HoldID: a.id("hold")})
}

func (a *Activities) NotifyRestaurant(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in NotifyRestaurantInput
	if err := decode(ActivityNotifyRestaurant, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivityNotifyRestaurant); err != nil {
		return nil, err
	}
	return encode(NotifyRestaurantResult{TicketID: a.id("ticket")})
}

func (a *Activities) DispatchDriver(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in DispatchDriverInput
	if err := decode(ActivityDispatchDriver, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivityDispatchDriver); err != nil {
		return nil, err
	}
	return encode(DispatchDriverResult{DriverID: a.id("driver"), ETA: "18m"})
}

// ---------------------------------------------------------------------------
// Compensating activities.
// ---------------------------------------------------------------------------

func (a *Activities) RefundPayment(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in RefundPaymentInput
	if err := decode(ActivityRefundPayment, raw, &in); err != nil {
		return nil, err
	}
	if in.PaymentID == "" {
		// The saga only registers this compensation once a charge has
		// returned a payment id, so an empty one means the engine has lost
		// track of what it is undoing. Refuse rather than guess.
		return nil, &core.NonRetryableError{
			Cause: fmt.Errorf("RefundPayment: no payment id to refund for order %q", in.OrderID),
		}
	}
	if err := a.callExternal(ctx, ActivityRefundPayment); err != nil {
		return nil, err
	}
	return encode(map[string]string{"refund_id": a.id("refund")})
}

func (a *Activities) ReleaseInventory(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in ReleaseInventoryInput
	if err := decode(ActivityReleaseInventory, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivityReleaseInventory); err != nil {
		return nil, err
	}
	return encode(map[string]string{"released": in.HoldID})
}

func (a *Activities) CancelRestaurantOrder(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in CancelRestaurantOrderInput
	if err := decode(ActivityCancelRestaurantOrder, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivityCancelRestaurantOrder); err != nil {
		return nil, err
	}
	return encode(map[string]string{"cancelled": in.TicketID})
}

// ---------------------------------------------------------------------------
// Simulation helpers.
// ---------------------------------------------------------------------------

// callExternal is the single seam every activity goes through: spend the
// configured latency, then ask the fault injector whether this call is one the
// demo wants to fail. Keeping it in one place means the demo's "fail this
// step" button works for every activity without any per-activity wiring.
func (a *Activities) callExternal(ctx context.Context, activityType string) error {
	if d := a.opts.Latency[activityType]; d > 0 {
		if err := a.opts.Sleep(ctx, d); err != nil {
			return err
		}
	}
	return a.opts.Faults.Check(activityType)
}

func (a *Activities) id(prefix string) string {
	return fmt.Sprintf("%s_%06d", prefix, a.seq.Add(1))
}

func decode(activityType string, raw json.RawMessage, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return &core.NonRetryableError{
			Cause: fmt.Errorf("%s: undecodable input: %w", activityType, err),
		}
	}
	return nil
}

func encode(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// sleepContext waits without leaking a timer if the worker is shutting down.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
