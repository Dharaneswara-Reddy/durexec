package activity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// recordingSleeper stands in for the wait between attempts. Nothing in these
// tests sleeps for real: a five-attempt retry sequence runs instantly and the
// delays are asserted exactly rather than observed as elapsed time.
type recordingSleeper struct {
	delays []time.Duration
	fail   error // when set, the wait is treated as cancelled
}

func (s *recordingSleeper) sleep(ctx context.Context, d time.Duration) error {
	s.delays = append(s.delays, d)
	return s.fail
}

func newTestExecutor(t *testing.T, defs ...Definition) (*Executor, *recordingSleeper) {
	t.Helper()
	r := NewRegistry()
	for _, d := range defs {
		if err := r.Register(d); err != nil {
			t.Fatalf("registering %q: %v", d.Name, err)
		}
	}
	s := &recordingSleeper{}
	return &Executor{Registry: r, Rand: maxRand{}, Sleep: s.sleep}, s
}

func TestExecutorSucceedsFirstTime(t *testing.T) {
	e, sleeper := newTestExecutor(t, Definition{
		Name: "Charge",
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"txn":"t1"}`), nil
		},
	})

	out := e.Execute(context.Background(), Request{ActivityType: "Charge", DedupKey: "dx1:abc"})
	if !out.OK() {
		t.Fatalf("unexpected failure: %v", out.Err)
	}
	if string(out.Result) != `{"txn":"t1"}` || out.Attempts != 1 {
		t.Fatalf("out = %+v", out)
	}
	if len(sleeper.delays) != 0 {
		t.Fatalf("a first-time success must not wait: %v", sleeper.delays)
	}
}

// TestExecutorRetriesThenSucceeds pins the whole backoff sequence. With the
// jitter source at the top of its range, full jitter yields exactly the
// geometric base delay, so the series is observable.
func TestExecutorRetriesThenSucceeds(t *testing.T) {
	calls := 0
	e, sleeper := newTestExecutor(t, Definition{
		Name: "Charge",
		Retry: RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    3 * time.Second,
			MaximumAttempts:    5,
		},
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			calls++
			if calls < 4 {
				return nil, errors.New("connection reset")
			}
			return json.RawMessage(`"ok"`), nil
		},
	})

	out := e.Execute(context.Background(), Request{ActivityType: "Charge"})
	if !out.OK() {
		t.Fatalf("unexpected failure: %v", out.Err)
	}
	if out.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", out.Attempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}
	if len(sleeper.delays) != len(want) {
		t.Fatalf("delays = %v, want %v", sleeper.delays, want)
	}
	for i := range want {
		if sleeper.delays[i] != want[i] {
			t.Fatalf("delay %d = %v, want %v (the cap bites on the third)", i, sleeper.delays[i], want[i])
		}
	}
}

func TestExecutorExhaustsBudget(t *testing.T) {
	calls := 0
	e, sleeper := newTestExecutor(t, Definition{
		Name:  "Charge",
		Retry: RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumAttempts: 3},
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			calls++
			return nil, errors.New("gateway timeout")
		},
	})

	out := e.Execute(context.Background(), Request{ActivityType: "Charge"})
	if out.OK() {
		t.Fatal("expected a failure")
	}
	if calls != 3 || out.Attempts != 3 {
		t.Fatalf("calls=%d attempts=%d, want 3 and 3", calls, out.Attempts)
	}
	// Three attempts means two waits between them, not three.
	if len(sleeper.delays) != 2 {
		t.Fatalf("delays = %v, want 2", sleeper.delays)
	}
	var af *ActivityFailedError
	if !errors.As(out.Err, &af) || af.ActivityType != "Charge" || af.Attempts != 3 {
		t.Fatalf("unexpected error shape: %#v", out.Err)
	}
	if out.NonRetryable {
		t.Fatal("an exhausted budget is not the same as a non-retryable failure")
	}
}

// TestExecutorStopsImmediatelyOnNonRetryable: a validation error must reach the
// workflow at once, so the saga can start compensating rather than waiting out
// a backoff for an answer that cannot change.
func TestExecutorStopsImmediatelyOnNonRetryable(t *testing.T) {
	calls := 0
	e, sleeper := newTestExecutor(t, Definition{
		Name:  "Charge",
		Retry: RetryPolicy{InitialInterval: time.Minute, BackoffCoefficient: 2, MaximumAttempts: 10},
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			calls++
			return nil, &core.NonRetryableError{Cause: errors.New("card number invalid")}
		},
	})

	out := e.Execute(context.Background(), Request{ActivityType: "Charge"})
	if calls != 1 {
		t.Fatalf("called %d times, want exactly 1", calls)
	}
	if len(sleeper.delays) != 0 {
		t.Fatalf("must not wait at all, waited %v", sleeper.delays)
	}
	if !out.NonRetryable {
		t.Fatal("the outcome must record that this was non-retryable, so history can too")
	}
	// The original error type survives inside the wrapper, so a caller can
	// still ask core.IsNonRetryable.
	if !core.IsNonRetryable(out.Err) {
		t.Fatalf("core.IsNonRetryable lost the cause: %v", out.Err)
	}
}

func TestExecutorHonoursContextCancellation(t *testing.T) {
	t.Run("cancelled before the first attempt", func(t *testing.T) {
		calls := 0
		e, _ := newTestExecutor(t, Definition{
			Name: "Charge",
			Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
				calls++
				return json.RawMessage(`"ok"`), nil
			},
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		out := e.Execute(ctx, Request{ActivityType: "Charge"})
		if calls != 0 {
			t.Fatal("a cancelled worker must make no outbound calls")
		}
		if out.OK() || !errors.Is(out.Err, context.Canceled) {
			t.Fatalf("err = %v, want a context cancellation", out.Err)
		}
	})

	t.Run("cancelled during the backoff", func(t *testing.T) {
		calls := 0
		r := NewRegistry()
		if err := r.Register(Definition{
			Name:  "Charge",
			Retry: RetryPolicy{InitialInterval: time.Hour, BackoffCoefficient: 2, MaximumAttempts: 10},
			Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
				calls++
				return nil, errors.New("gateway timeout")
			},
		}); err != nil {
			t.Fatal(err)
		}
		s := &recordingSleeper{fail: context.Canceled}
		e := &Executor{Registry: r, Rand: maxRand{}, Sleep: s.sleep}

		out := e.Execute(context.Background(), Request{ActivityType: "Charge"})
		if calls != 1 {
			t.Fatalf("called %d times; the wait was abandoned so there is no second attempt", calls)
		}
		// The activity's own failure is reported, not the cancellation: that is
		// what an operator needs to see. The task simply becomes visible again
		// when its lease expires.
		if out.Err == nil || errors.Is(out.Err, context.Canceled) {
			t.Fatalf("err = %v, want the activity's failure", out.Err)
		}
	})
}

func TestExecutorPassesInfoToTheActivity(t *testing.T) {
	var seen []Info
	e, _ := newTestExecutor(t, Definition{
		Name:  "Charge",
		Retry: RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 1, MaximumAttempts: 3},
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			info, ok := InfoFromContext(ctx)
			if !ok {
				return nil, errors.New("no activity info in context")
			}
			seen = append(seen, info)
			return nil, errors.New("still failing")
		},
	})

	e.Execute(context.Background(), Request{ActivityType: "Charge", DedupKey: "dx1:abc"})

	if len(seen) != 3 {
		t.Fatalf("saw %d attempts, want 3", len(seen))
	}
	for i, info := range seen {
		// The attempt number changes so an activity can widen a timeout on a
		// late retry. The dedup key must NOT: every retry has to look like the
		// same call to the downstream service, which is the entire mechanism.
		if info.Attempt != i+1 {
			t.Errorf("attempt %d reported %d", i+1, info.Attempt)
		}
		if info.DedupKey != "dx1:abc" {
			t.Errorf("attempt %d saw dedup key %q; it must never change", i+1, info.DedupKey)
		}
		if info.ActivityType != "Charge" {
			t.Errorf("attempt %d saw activity type %q", i+1, info.ActivityType)
		}
	}
}

func TestExecutorUnknownActivity(t *testing.T) {
	e, sleeper := newTestExecutor(t)
	out := e.Execute(context.Background(), Request{ActivityType: "Ghost"})

	var nr *NotRegisteredError
	if !errors.As(out.Err, &nr) || nr.ActivityType != "Ghost" {
		t.Fatalf("unexpected error: %v", out.Err)
	}
	if !out.NonRetryable || out.Attempts != 0 {
		t.Fatalf("out = %+v; waiting cannot make an unregistered name appear", out)
	}
	if len(sleeper.delays) != 0 {
		t.Fatalf("must not wait: %v", sleeper.delays)
	}
}

// TestExecutorSurvivesAPanickingActivity: a worker runs activities for many
// unrelated workflows, so one bad deployment must not become an outage.
func TestExecutorSurvivesAPanickingActivity(t *testing.T) {
	e, _ := newTestExecutor(t, Definition{
		Name:  "Charge",
		Retry: RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 1, MaximumAttempts: 2},
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			panic("nil pointer somewhere in the SDK")
		},
	})

	out := e.Execute(context.Background(), Request{ActivityType: "Charge"})
	if out.OK() {
		t.Fatal("expected a failure")
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d; a panic is treated as an ordinary retryable failure", out.Attempts)
	}
}

// TestExecutorNormalisesAnEmptyResult: history stores JSON, and empty bytes are
// not valid JSON.
func TestExecutorNormalisesAnEmptyResult(t *testing.T) {
	e, _ := newTestExecutor(t, Definition{
		Name: "Log",
		Fn: func(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
			return nil, nil
		},
	})
	out := e.Execute(context.Background(), Request{ActivityType: "Log"})
	if !out.OK() || string(out.Result) != "null" {
		t.Fatalf("out = %+v, want a null result", out)
	}
}

// TestExecutorWritesNothing is a structural assertion, not a behavioural one:
// the executor takes no store of any kind, so it is impossible for it to
// journal an outcome. That separation is what lets the caller fold the outcome
// into the same transaction that completes the task.
func TestExecutorWritesNothing(t *testing.T) {
	e := NewExecutor(NewRegistry())
	if e.Registry == nil {
		t.Fatal("registry not wired")
	}
	// Defaults fill themselves in, so a zero-configured executor is usable.
	if e.rand() == nil {
		t.Fatal("expected a default randomness source")
	}
	if e.sleep() == nil {
		t.Fatal("expected a default sleeper")
	}
}

func TestRealSleepReturnsEarlyOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An hour-long backoff must not delay a shutdown by an hour.
	if err := RealSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
