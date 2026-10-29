package extsim

import (
	"encoding/json"
	"fmt"

	"github.com/dharaneswar339/durexec/internal/activity"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// activityRegistrar is the one method RegisterProbe needs from the activity
// registry. Taking an interface rather than *activity.Registry keeps this
// file testable without building a whole registry, and matches how the rest
// of the codebase narrows its dependencies.
type activityRegistrar interface {
	Register(activity.Definition) error
}

// ProbeWorkflowType is the registered name of the only workflow in this
// repository that performs a real external effect over the network.
//
// It exists because of a gap this phase had to close: ExternalPayment was a
// well-built activity that nothing could schedule. The engine registers
// activities, but only a WORKFLOW can schedule one, and no shipped workflow
// named it — so the activity, its dedup key and its journal interaction had
// never run inside engined. The existing tests drive it by constructing a
// decision directly, which exercises the activity but not the engine's own
// claim, retry, journal and recovery path.
//
// It is registered only when EXTSIM_URL is configured. See RegisterProbe.
const ProbeWorkflowType = "ExternalEffectProbe"

// ProbeInput controls the shape of one probe run.
type ProbeInput struct {
	// Amount and Payee are forwarded to the external service and form the
	// payload identity recorded in its ledger.
	Amount int64  `json:"amount"`
	Payee  string `json:"payee"`

	// Payments is how many DISTINCT external effects the run should perform,
	// default 1. Each gets its own program-order index k and therefore its own
	// dedup key, so a run with two payments must produce exactly two effects —
	// which is what distinguishes "deduplicated correctly" from "collapsed
	// two different payments into one".
	Payments int `json:"payments"`

	// FailAfter, when true, makes the workflow fail AFTER its payments have
	// succeeded. This is the saga case: a later step fails once an external
	// effect has already landed, and the question is what the engine does
	// about an effect it cannot take back. ExternalPayment declares no
	// compensation — deliberately, see PayDefinition — so the honest expected
	// outcome is a failed workflow with the effect still standing.
	FailAfter bool `json:"fail_after"`
}

// ProbeOutput is what the run records.
type ProbeOutput struct {
	Effects []PayOutput `json:"effects"`
}

// ProbeWorkflow schedules ExternalPayment and nothing else.
//
// It is deliberately minimal and contains no timers, no signals and no
// branching: every event in its history is either the start, a payment, or the
// terminal event, so a history read after a crash can be interpreted without
// untangling it from unrelated steps.
func ProbeWorkflow(ctx workflow.Context, input json.RawMessage) (json.RawMessage, error) {
	var in ProbeInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, fmt.Errorf("extsim probe: undecodable input: %w", err)
		}
	}
	if in.Payments <= 0 {
		in.Payments = 1
	}
	if in.Amount == 0 {
		in.Amount = 100
	}
	if in.Payee == "" {
		in.Payee = "probe-payee"
	}

	out := ProbeOutput{}
	// Sequential, not concurrent. Each Get suspends the workflow until the
	// result is journaled, so the n-th payment is only scheduled once the
	// (n-1)-th is durable. That makes the boundary a crash lands on
	// unambiguous: the number of journaled results tells you exactly how far
	// the run had got.
	for i := 0; i < in.Payments; i++ {
		raw, err := ctx.ExecuteActivity(PayActivityName, PayInput{
			Amount: in.Amount + int64(i),
			Payee:  in.Payee,
		}).Get()
		if err != nil {
			return nil, err
		}
		var pay PayOutput
		if err := json.Unmarshal(raw, &pay); err != nil {
			return nil, fmt.Errorf("extsim probe: undecodable payment output: %w", err)
		}
		out.Effects = append(out.Effects, pay)
	}

	if in.FailAfter {
		// A plain error, so the engine treats it as a terminal workflow
		// failure and runs whatever compensations are registered. For
		// ExternalPayment that is none, which is the point of the test.
		return nil, fmt.Errorf("extsim probe: deliberate failure after %d external effect(s) had already landed", len(out.Effects))
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// RegisterProbe adds the external-effect activity and the probe workflow.
//
// SAFETY. This is called only when EXTSIM_URL is set, and config.Validate
// requires that variable to be an explicit http(s) URL. Three further things
// keep it from becoming a production dependency by accident:
//
//  1. Registering an activity does not cause it to run. Only a workflow that
//     names ExternalPayment can schedule it, and the only such workflow is the
//     one registered here, under the same flag.
//  2. No shipped workflow (DeliveryOrder, TravelAgent) references either name,
//     so behaviour of the real workflows is bit-identical whether or not this
//     is called.
//  3. engined logs a WARN naming both as test-only at start-up, so a
//     deployment that has them cannot look like one that does not.
func RegisterProbe(acts activityRegistrar, wfs *workflow.Registry, baseURL string) error {
	if err := acts.Register(PayDefinition(nil, baseURL)); err != nil {
		return fmt.Errorf("register %s: %w", PayActivityName, err)
	}
	if err := wfs.Register(ProbeWorkflowType, ProbeWorkflow); err != nil {
		return fmt.Errorf("register %s: %w", ProbeWorkflowType, err)
	}
	return nil
}
