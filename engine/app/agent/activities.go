package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// Registration tells the engine how to treat one activity: what to call it,
// how to run it, and which idempotency rule applies when the same activity is
// scheduled twice.
//
// This type is declared in both demonstration packages because the engine's
// own registry is being written in parallel. The integrator should hoist one
// copy into the engine and delete the other.
type Registration struct {
	Type string
	Mode core.IdempotencyMode

	// Handler does the work. It receives the raw scheduled input.
	Handler func(ctx context.Context, input json.RawMessage) (json.RawMessage, error)

	// IntentText renders the input as the one sentence the semantic check
	// should embed and compare. It is required for core.ModeSemantic and nil
	// for core.ModeExact, which needs no embedding at all.
	IntentText func(input json.RawMessage) (string, error)
}

// Options configures the agent's simulated tools.
type Options struct {
	// Planner is the decision-maker. Production passes a model-backed planner;
	// the evaluation passes a ScriptedPlanner from the fixture.
	Planner Planner

	// Faults is the deliberate-failure switch. Nil, or an injector that
	// configuration never enabled, means the tools always succeed.
	Faults *faultinj.Injector

	// Latency is the simulated round-trip time per activity type.
	Latency map[string]time.Duration

	// Sleep is how simulated latency is spent. Tests supply a no-op.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Booking is one seat this process actually took. The demonstration counts
// these: two bookings for one request is the bug the engine has to prevent.
type Booking struct {
	BookingID string   `json:"booking_id"`
	Call      ToolCall `json:"call"`
}

// Activities holds the agent's simulated tools for one node.
type Activities struct {
	opts Options

	seq atomic.Uint64

	mu       sync.Mutex
	bookings []Booking
}

// NewActivities builds the agent activity set.
func NewActivities(opts Options) *Activities {
	if opts.Latency == nil {
		opts.Latency = DefaultLatency()
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	return &Activities{opts: opts}
}

// DefaultLatency is a plausible spread: a model call is slow, a flight search
// is slower than a booking write.
func DefaultLatency() map[string]time.Duration {
	return map[string]time.Duration{
		ActivityPlan:          900 * time.Millisecond,
		ActivitySearchFlights: 450 * time.Millisecond,
		ActivityBookFlight:    300 * time.Millisecond,
		ActivityFinalize:      80 * time.Millisecond,
	}
}

// Registrations is what the engine's worker registers at startup.
//
// The two modes are chosen per activity, and the split is the argument this
// project is making.
//
// SearchFlights and BookFlight are core.ModeSemantic. Their arguments are a
// sentence a language model wrote — "BLR to DEL Friday morning" — and the same
// request can come back worded differently after a crash. Comparing the bytes
// answers the wrong question: what matters is whether the two calls mean the
// same thing, and only a meaning-aware comparison can tell.
//
// Plan and Finalize are core.ModeExact. Planning has no side effect in the
// world, so repeating it costs a model call and nothing else; deduplicating
// plans by meaning would suppress the legitimate re-planning that recovery
// depends on. Finalize is keyed by a request id, which is an identifier, not
// a sentence.
func (a *Activities) Registrations() []Registration {
	return []Registration{
		{Type: ActivityPlan, Mode: core.ModeExact, Handler: a.PlanStep},
		{
			Type:       ActivitySearchFlights,
			Mode:       core.ModeSemantic,
			Handler:    a.SearchFlights,
			IntentText: toolCallIntent,
		},
		{
			Type:       ActivityBookFlight,
			Mode:       core.ModeSemantic,
			Handler:    a.BookFlight,
			IntentText: toolCallIntent,
		},
		{Type: ActivityFinalize, Mode: core.ModeExact, Handler: a.Finalize},
	}
}

// toolCallIntent extracts the sentence the semantic check compares.
func toolCallIntent(raw json.RawMessage) (string, error) {
	var c ToolCall
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("agent: cannot read tool call for intent text: %w", err)
	}
	text := CanonicalIntent(c.Args)
	if text == "" {
		// With no text there is nothing to embed, and an empty vector would
		// look similar to every other empty vector. Refuse instead.
		return "", fmt.Errorf("agent: tool call for %q has no arguments to compare", c.Tool)
	}
	return text, nil
}

// ---------------------------------------------------------------------------
// Activities.
// ---------------------------------------------------------------------------

// PlanStep asks the planner what to do next.
func (a *Activities) PlanStep(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in PlanInput
	if err := decode(ActivityPlan, raw, &in); err != nil {
		return nil, err
	}
	if a.opts.Planner == nil {
		return nil, &core.NonRetryableError{Cause: fmt.Errorf("agent: no planner configured")}
	}
	if err := a.callExternal(ctx, ActivityPlan); err != nil {
		return nil, err
	}
	plan, err := a.opts.Planner.Plan(ctx, in)
	if err != nil {
		return nil, err
	}
	return encode(plan)
}

// SearchFlights is a read-only tool: repeating it wastes a call and nothing
// else, which is why the interesting duplicate is the booking, not the search.
func (a *Activities) SearchFlights(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in ToolCall
	if err := decode(ActivitySearchFlights, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivitySearchFlights); err != nil {
		return nil, err
	}
	return encode(map[string]any{
		"query": in.Args,
		"results": []map[string]string{
			{"flight": "6E-2113", "depart": "07:40", "arrive": "10:25", "fare": "INR 5,420"},
			{"flight": "AI-503", "depart": "08:15", "arrive": "11:05", "fare": "INR 6,180"},
		},
	})
}

// BookFlight is the tool with a real side effect. Every call takes a seat and
// charges a card, so calling it twice for one request is the failure the whole
// engine exists to prevent.
func (a *Activities) BookFlight(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in ToolCall
	if err := decode(ActivityBookFlight, raw, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Args) == "" {
		return nil, &core.NonRetryableError{Cause: fmt.Errorf("BookFlight: no itinerary given")}
	}
	if err := a.callExternal(ctx, ActivityBookFlight); err != nil {
		return nil, err
	}

	booking := Booking{BookingID: a.id("bk"), Call: in}
	a.mu.Lock()
	a.bookings = append(a.bookings, booking)
	a.mu.Unlock()

	return encode(map[string]string{
		"booking_id": booking.BookingID,
		"flight":     "6E-2113",
		"status":     "CONFIRMED",
		"itinerary":  in.Args,
	})
}

// Finalize writes the answer back to the user.
func (a *Activities) Finalize(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var in FinalizeInput
	if err := decode(ActivityFinalize, raw, &in); err != nil {
		return nil, err
	}
	if err := a.callExternal(ctx, ActivityFinalize); err != nil {
		return nil, err
	}
	summary := in.Answer
	if summary == "" {
		summary = fmt.Sprintf("Completed %q in %d steps.", in.Goal, len(in.Transcript))
	}
	return encode(FinalizeResult{
		ConfirmationID: a.id("cnf"),
		Summary:        summary,
	})
}

// Bookings returns every seat this process has taken, in order. The
// demonstration asserts there is exactly one.
func (a *Activities) Bookings() []Booking {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Booking, len(a.bookings))
	copy(out, a.bookings)
	return out
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

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
