package runtime

import (
	"context"
	"fmt"

	"github.com/dharaneswar339/durexec/internal/api"
	"github.com/dharaneswar339/durexec/internal/faultinj"
)

// Faults exposes the deliberate-failure switch over HTTP.
//
// Fault injection has to be triggerable from outside the process, because the
// whole point of it is to break a node while it is running and watch what the
// engine does. That is also exactly why it is inert unless the environment
// armed it: internal/faultinj refuses to fire on an injector that
// configuration never enabled, and config.Config refuses to report
// FaultInjection as true unless FAULT_INJECTION really was set. Two locks, in
// series, so a stray line of code cannot inject a failure into a real
// deployment.
type Faults struct {
	Injector *faultinj.Injector
}

var _ api.FaultInjector = (*Faults)(nil)

// FailStep arms the next call of an activity to fail.
//
// A non-retryable failure is the interesting one for the delivery demo: a
// retryable one would simply be retried into success and the saga's
// compensation — the thing worth watching — would never run.
func (f *Faults) FailStep(_ context.Context, req api.FailStepRequest) error {
	if f == nil || !f.Injector.Enabled() {
		return fmt.Errorf("fault injection is not enabled on this node")
	}
	times := req.Attempts
	if times <= 0 {
		times = 1
	}
	if req.NonRetryable {
		f.Injector.FailNextFatal(req.ActivityType, times)
		return nil
	}
	f.Injector.FailNext(req.ActivityType, times)
	return nil
}
