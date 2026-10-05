package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"

	"github.com/dharaneswar339/durexec/internal/core"
	"time"
)

// Info is what an activity implementation is told about the call it is
// currently serving. It reaches the implementation through the context rather
// than through the signature, so that an activity that does not care about it
// stays a plain function of its input.
type Info struct {
	// ActivityType is the registered name.
	ActivityType string

	// DedupKey is the value to forward to an external service as its
	// Idempotency-Key. It is identical on every attempt and after every crash;
	// see DedupKey for why that is the whole point.
	DedupKey string

	// Attempt is 1-based and counts this call. It is deliberately NOT part of
	// the dedup key — it is here for logging and for an activity that wants to
	// behave differently on a late retry, such as widening a timeout.
	Attempt int
}

type infoKey struct{}

// WithInfo attaches call metadata to a context.
func WithInfo(ctx context.Context, in Info) context.Context {
	return context.WithValue(ctx, infoKey{}, in)
}

// InfoFromContext reads it back inside an activity implementation.
func InfoFromContext(ctx context.Context) (Info, bool) {
	in, ok := ctx.Value(infoKey{}).(Info)
	return in, ok
}

// Request is one activity invocation, as reconstructed from the recorded
// ActivityTaskScheduled event. Everything in it came out of the log, which is
// why an activity worker needs no memory of what it was doing before a crash.
type Request struct {
	ActivityType string
	Input        json.RawMessage
	DedupKey     string
}

// Outcome is what the Executor reports back. It is a value, not a write: the
// executor never touches a store, so the caller is free to fold this into the
// same transaction as everything else, or to discard it if the worker has since
// lost its lease.
type Outcome struct {
	ActivityType string

	// Result is set when the activity succeeded.
	Result json.RawMessage

	// Err is the final failure after all retries, wrapped in an
	// *ActivityFailedError. Nil on success.
	Err error

	// Attempts is how many times the implementation was actually called.
	Attempts int

	// NonRetryable records why we stopped: true means the failure was declared
	// unretryable and the budget was never spent, false means the budget ran
	// out or the context was cancelled. The caller journals this so that
	// replaying the workflow can tell a validation error from a timeout.
	NonRetryable bool
}

// OK reports success.
func (o Outcome) OK() bool { return o.Err == nil }

// Sleeper is how the executor waits between attempts. It is injected so that
// tests can run a five-attempt retry sequence instantly and still assert on the
// exact delays that were requested — a real sleep would make the test slow and
// timing-dependent, which is precisely what this project is trying to avoid.
type Sleeper func(ctx context.Context, d time.Duration) error

// RealSleep waits, or returns early if the context is cancelled. Returning the
// context error rather than swallowing it is what lets a shutdown abandon a
// long backoff immediately instead of after a minute.
func RealSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Executor runs one activity, retrying it according to its policy.
//
// It writes nothing. Not the attempt, not the result, not the failure. That is
// not an oversight: the executor is the piece that touches the outside world,
// and it must not also be the piece that decides what history says, because
// then a crash between the two would leave them disagreeing. The caller takes
// the Outcome and journals it inside the same transaction that completes the
// task, so either both happen or neither does.
type Executor struct {
	Registry *Registry

	// Rand seeds the jitter. Nil means the shared global source.
	Rand Rand

	// Sleep waits between attempts. Nil means RealSleep.
	Sleep Sleeper
}

func NewExecutor(r *Registry) *Executor { return &Executor{Registry: r} }

func (e *Executor) rand() Rand {
	if e.Rand != nil {
		return e.Rand
	}
	return DefaultRand()
}

func (e *Executor) sleep() Sleeper {
	if e.Sleep != nil {
		return e.Sleep
	}
	return RealSleep
}

// Execute runs the activity to a conclusion: success, a failure that must not
// be retried, an exhausted retry budget, or a cancelled context.
func (e *Executor) Execute(ctx context.Context, req Request) Outcome {
	out := Outcome{ActivityType: req.ActivityType}

	def, ok := e.Registry.Get(req.ActivityType)
	if !ok {
		// Nothing to retry: no amount of waiting makes an unregistered name
		// appear in this process. This is usually a deployment skew — a task
		// for a new activity reached an old binary — and the workflow should
		// find out quickly so it can compensate.
		out.Err = &ActivityFailedError{
			ActivityType: req.ActivityType,
			Attempts:     0,
			Cause:        &NotRegisteredError{ActivityType: req.ActivityType},
		}
		out.NonRetryable = true
		return out
	}
	if def.Fn == nil {
		out.Err = &ActivityFailedError{ActivityType: req.ActivityType, Attempts: 0, Cause: errNilFn}
		out.NonRetryable = true
		return out
	}

	policy := def.Retry
	if policy.IsZero() {
		policy = DefaultRetryPolicy()
	}

	for attempt := 1; ; attempt++ {
		// Checked before every attempt, so a worker that is shutting down or
		// has lost its lease stops making outbound calls rather than finishing
		// a retry sequence nobody is waiting for.
		if err := ctx.Err(); err != nil {
			out.Attempts = attempt - 1
			out.Err = &ActivityFailedError{
				ActivityType: req.ActivityType,
				Attempts:     out.Attempts,
				Cause:        err,
			}
			return out
		}

		callCtx := WithInfo(ctx, Info{
			ActivityType: req.ActivityType,
			DedupKey:     req.DedupKey,
			Attempt:      attempt,
		})
		result, err := invoke(callCtx, def.Fn, req.Input)
		out.Attempts = attempt

		if err == nil {
			if len(result) == 0 {
				// An activity with nothing to say still has to record
				// something, because history stores JSON and empty bytes are
				// not valid JSON.
				result = json.RawMessage("null")
			}
			out.Result = result
			return out
		}

		if !ShouldRetry(err, attempt, policy) {
			out.Err = &ActivityFailedError{
				ActivityType: req.ActivityType,
				Attempts:     attempt,
				Cause:        err,
			}
			out.NonRetryable = core.IsNonRetryable(err)
			return out
		}

		delay, again := policy.NextDelayWith(e.rand(), attempt)
		if !again {
			out.Err = &ActivityFailedError{
				ActivityType: req.ActivityType,
				Attempts:     attempt,
				Cause:        err,
			}
			return out
		}
		if serr := e.sleep()(ctx, delay); serr != nil {
			// Cancelled mid-backoff. Report the activity's own failure as the
			// cause rather than the cancellation, because that is the thing an
			// operator needs to see; the task simply becomes visible again when
			// the lease expires and another worker picks it up.
			out.Err = &ActivityFailedError{
				ActivityType: req.ActivityType,
				Attempts:     attempt,
				Cause:        err,
			}
			return out
		}
	}
}

// invoke calls the implementation and converts a panic into an error.
//
// A panicking activity must not take down the worker: that process is running
// activities for many unrelated workflows, and killing it would turn one bad
// deployment into an outage. The panic is treated as an ordinary retryable
// failure, because a panic caused by a transient condition (a nil response body
// from a flaky server, say) really can succeed on the next attempt.
func invoke(ctx context.Context, fn Fn, input json.RawMessage) (result json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = fmt.Errorf("activity panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return fn(ctx, input)
}
