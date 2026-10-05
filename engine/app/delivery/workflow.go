// Package delivery is the first of the two demonstration applications: a food
// delivery order, expressed as a saga.
//
// It exists to show the ordinary case. Every step here has a natural, exact
// idempotency key — an order id, a payment id — so nothing in this workflow
// needs the semantic deduplication that the agent application demonstrates.
// That contrast is the point: the new mechanism is opt-in, and this is what a
// workflow looks like when you do not opt in.
package delivery

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
// smallest surface it needs rather than importing the real one. Go interfaces
// are structural, so the engine's own context satisfies this as soon as the
// method set lines up; the integrator only has to delete these two
// declarations and import the real package.
// ---------------------------------------------------------------------------

// Ctx is the workflow's only way to reach the outside world. Everything it
// offers is durable: each call is journaled before it happens and replayed
// from the journal afterwards, which is what makes the workflow function
// restartable from the top after a crash.
type Ctx interface {
	// ExecuteActivity schedules one activity and returns a handle to its result.
	ExecuteActivity(activityType string, input any) Future
	// Sleep is a durable timer: a row in the database, not a goroutine, so it
	// survives the process that started it.
	Sleep(d time.Duration) Future
	// WaitForSignal blocks until an external caller delivers the named signal.
	WaitForSignal(name string) Future
	// Now is the replay-safe clock. Calling time.Now directly in workflow code
	// would give a different answer on replay and break determinism.
	Now() time.Time
}

// Future is the handle to one pending durable operation. Get blocks the
// workflow goroutine until the result is journaled.
type Future interface {
	Get() (json.RawMessage, error)
}

// ---------------------------------------------------------------------------
// Activity names and idempotency modes.
// ---------------------------------------------------------------------------

const (
	ActivityChargePayment    = "ChargePayment"
	ActivityReserveInventory = "ReserveInventory"
	ActivityNotifyRestaurant = "NotifyRestaurant"
	ActivityDispatchDriver   = "DispatchDriver"

	ActivityRefundPayment         = "RefundPayment"
	ActivityReleaseInventory      = "ReleaseInventory"
	ActivityCancelRestaurantOrder = "CancelRestaurantOrder"
)

// WorkflowType is the name the engine registers this workflow under.
const WorkflowType = "DeliveryOrder"

// SignalCancelOrder lets a customer cancel while the order is in flight.
const SignalCancelOrder = "CancelOrder"

// defaultPrepTime is how long the workflow waits after the restaurant accepts
// before it dispatches a driver, so the driver does not arrive to wait.
const defaultPrepTime = 12 * time.Minute

// ---------------------------------------------------------------------------
// Inputs and outputs.
// ---------------------------------------------------------------------------

// LineItem is one dish on the order.
type LineItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// Order is the workflow input.
type Order struct {
	OrderID      string     `json:"order_id"`
	CustomerID   string     `json:"customer_id"`
	RestaurantID string     `json:"restaurant_id"`
	Items        []LineItem `json:"items"`

	// AmountCents is an integer on purpose. Money is never a float, and it is
	// also the reason every activity here uses exact idempotency.
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`

	// PrepTime overrides defaultPrepTime; zero means use the default.
	PrepTime time.Duration `json:"prep_time_ns,omitempty"`
}

// Result is the workflow output.
type Result struct {
	OrderID   string    `json:"order_id"`
	PaymentID string    `json:"payment_id,omitempty"`
	HoldID    string    `json:"hold_id,omitempty"`
	TicketID  string    `json:"ticket_id,omitempty"`
	DriverID  string    `json:"driver_id,omitempty"`
	PlacedAt  time.Time `json:"placed_at"`

	// Compensated and FailedStep describe a rolled-back order. They are part
	// of the result rather than only an error because the demo UI needs to
	// show which step failed and that the rollback ran.
	Compensated  bool     `json:"compensated"`
	FailedStep   string   `json:"failed_step,omitempty"`
	CompensatedB []string `json:"compensations_run,omitempty"`
}

// Activity payloads. These are separate types rather than reusing Order so an
// activity only receives the fields it actually needs.
type (
	ChargePaymentInput struct {
		OrderID     string `json:"order_id"`
		CustomerID  string `json:"customer_id"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
	}
	ChargePaymentResult struct {
		PaymentID string `json:"payment_id"`
	}

	ReserveInventoryInput struct {
		OrderID      string     `json:"order_id"`
		RestaurantID string     `json:"restaurant_id"`
		Items        []LineItem `json:"items"`
	}
	ReserveInventoryResult struct {
		HoldID string `json:"hold_id"`
	}

	NotifyRestaurantInput struct {
		OrderID      string     `json:"order_id"`
		RestaurantID string     `json:"restaurant_id"`
		Items        []LineItem `json:"items"`
	}
	NotifyRestaurantResult struct {
		TicketID string `json:"ticket_id"`
	}

	DispatchDriverInput struct {
		OrderID      string `json:"order_id"`
		RestaurantID string `json:"restaurant_id"`
		CustomerID   string `json:"customer_id"`
	}
	DispatchDriverResult struct {
		DriverID string `json:"driver_id"`
		ETA      string `json:"eta"`
	}

	RefundPaymentInput struct {
		OrderID     string `json:"order_id"`
		PaymentID   string `json:"payment_id"`
		AmountCents int64  `json:"amount_cents"`
	}
	ReleaseInventoryInput struct {
		OrderID string `json:"order_id"`
		HoldID  string `json:"hold_id"`
	}
	CancelRestaurantOrderInput struct {
		OrderID  string `json:"order_id"`
		TicketID string `json:"ticket_id"`
	}
)

// ---------------------------------------------------------------------------
// The saga.
// ---------------------------------------------------------------------------

// compensation is one undo action, remembered with the input it will need.
type compensation struct {
	ActivityType string
	Input        any
}

// saga is a stack of undo actions. It is a stack and not a list because the
// steps are not independent: releasing inventory before refunding is fine, but
// refunding before releasing would briefly leave a paid-for order with no
// stock behind it. Unwinding in the exact reverse of the order things happened
// is the only ordering that is always safe.
type saga struct {
	workflowID string
	steps      []compensation
}

// register pushes an undo action.
//
// It is only ever called AFTER the forward step it undoes has returned
// successfully, and that ordering is the whole safety argument. If we
// registered RefundPayment before calling ChargePayment, then a charge that
// failed before the payment processor ever saw it would still leave a refund
// on the stack — and the rollback would refund money that was never taken.
// The rule is: you owe a compensation only once you know the side effect
// actually happened.
func (s *saga) register(activityType string, input any) {
	s.steps = append(s.steps, compensation{ActivityType: activityType, Input: input})
}

// run unwinds the stack last-in-first-out.
//
// A compensation that fails is terminal: the engine has no way to put the
// world back by itself, so it stops and names the step that needs a human
// rather than pretending the rollback succeeded.
func (s *saga) run(ctx Ctx) ([]string, error) {
	ran := make([]string, 0, len(s.steps))
	for i := len(s.steps) - 1; i >= 0; i-- {
		step := s.steps[i]
		if _, err := ctx.ExecuteActivity(step.ActivityType, step.Input).Get(); err != nil {
			return ran, &core.CompensationFailedError{
				WorkflowID: s.workflowID,
				Activity:   step.ActivityType,
				Attempts:   1,
				Cause:      err,
			}
		}
		ran = append(ran, step.ActivityType)
	}
	return ran, nil
}

// ---------------------------------------------------------------------------
// The workflow.
// ---------------------------------------------------------------------------

// Run is the workflow function: charge, reserve, notify, wait for the food to
// be cooked, dispatch. Any failure unwinds everything that already happened.
func Run(ctx Ctx, order Order) (Result, error) {
	res := Result{OrderID: order.OrderID, PlacedAt: ctx.Now()}
	undo := saga{workflowID: order.OrderID}

	// Step 1 — take the money.
	var charged ChargePaymentResult
	if err := call(ctx, ActivityChargePayment, ChargePaymentInput{
		OrderID:     order.OrderID,
		CustomerID:  order.CustomerID,
		AmountCents: order.AmountCents,
		Currency:    order.Currency,
	}, &charged); err != nil {
		// Nothing has happened yet, so there is nothing to undo.
		return failed(res, ActivityChargePayment), err
	}
	res.PaymentID = charged.PaymentID
	// Registered only now: before this line we did not know the charge landed.
	undo.register(ActivityRefundPayment, RefundPaymentInput{
		OrderID:     order.OrderID,
		PaymentID:   charged.PaymentID,
		AmountCents: order.AmountCents,
	})

	// Step 2 — hold the stock.
	var reserved ReserveInventoryResult
	if err := call(ctx, ActivityReserveInventory, ReserveInventoryInput{
		OrderID:      order.OrderID,
		RestaurantID: order.RestaurantID,
		Items:        order.Items,
	}, &reserved); err != nil {
		return compensate(ctx, res, ActivityReserveInventory, &undo, err)
	}
	res.HoldID = reserved.HoldID
	undo.register(ActivityReleaseInventory, ReleaseInventoryInput{
		OrderID: order.OrderID,
		HoldID:  reserved.HoldID,
	})

	// Step 3 — tell the kitchen.
	var notified NotifyRestaurantResult
	if err := call(ctx, ActivityNotifyRestaurant, NotifyRestaurantInput{
		OrderID:      order.OrderID,
		RestaurantID: order.RestaurantID,
		Items:        order.Items,
	}, &notified); err != nil {
		return compensate(ctx, res, ActivityNotifyRestaurant, &undo, err)
	}
	res.TicketID = notified.TicketID
	undo.register(ActivityCancelRestaurantOrder, CancelRestaurantOrderInput{
		OrderID:  order.OrderID,
		TicketID: notified.TicketID,
	})

	// A durable sleep, not time.Sleep: this workflow may sit here for a
	// quarter of an hour, across process restarts and shard handovers, and it
	// must not hold a goroutine or a database connection while it waits.
	prep := order.PrepTime
	if prep <= 0 {
		prep = defaultPrepTime
	}
	if _, err := ctx.Sleep(prep).Get(); err != nil {
		return compensate(ctx, res, "PrepTimer", &undo, err)
	}

	// Step 4 — send a driver. Nothing is registered after this one: it is the
	// last step, so there is no later failure for it to compensate.
	var dispatched DispatchDriverResult
	if err := call(ctx, ActivityDispatchDriver, DispatchDriverInput{
		OrderID:      order.OrderID,
		RestaurantID: order.RestaurantID,
		CustomerID:   order.CustomerID,
	}, &dispatched); err != nil {
		return compensate(ctx, res, ActivityDispatchDriver, &undo, err)
	}
	res.DriverID = dispatched.DriverID

	return res, nil
}

// RunJSON adapts Run to the engine's generic workflow signature.
func RunJSON(ctx Ctx, input json.RawMessage) (json.RawMessage, error) {
	var order Order
	if err := json.Unmarshal(input, &order); err != nil {
		return nil, &core.NonRetryableError{Cause: fmt.Errorf("delivery: bad workflow input: %w", err)}
	}
	res, err := Run(ctx, order)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// call runs one activity and decodes its result.
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
			Cause: fmt.Errorf("delivery: %s returned undecodable result: %w", activityType, err),
		}
	}
	return nil
}

func failed(res Result, step string) Result {
	res.FailedStep = step
	return res
}

// compensate unwinds and returns the original failure, not the rollback's
// success. The order still failed; the rollback only limited the damage.
func compensate(ctx Ctx, res Result, failedStep string, undo *saga, cause error) (Result, error) {
	res.FailedStep = failedStep
	ran, err := undo.run(ctx)
	res.CompensatedB = ran
	res.Compensated = err == nil
	if err != nil {
		// A failed rollback outranks the original failure: it is the one that
		// needs a person, so it is the error the caller sees.
		return res, err
	}
	return res, cause
}
