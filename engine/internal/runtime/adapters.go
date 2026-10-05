package runtime

import (
	"encoding/json"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/app/delivery"
	"github.com/dharaneswar339/durexec/internal/workflow"
)

// The two demonstration applications each declare their own Ctx and Future
// interfaces rather than importing internal/workflow. That was deliberate:
// they were written at the same time as the engine, and an application that
// depends only on the shape of what it is handed can be compiled, tested and
// reasoned about before the engine exists at all.
//
// The cost is paid here, once, and it is small. Future lines up exactly —
// Get() (json.RawMessage, error) — so workflow.Future already satisfies
// delivery.Future and agent.Future with no work at all. Ctx does not, and not
// because the methods differ: ExecuteActivity, Sleep and WaitForSignal have
// identical parameters but return the application's own Future type, and Go
// matches method signatures by exact type, not by assignability. So each
// method is re-declared to return the application's interface and immediately
// hand back the engine's value.
//
// This is why the adaptation lives in the integration layer and not in either
// of the packages it joins. The applications stay independent of the engine,
// the engine stays ignorant of the applications, and the two are married in
// the one package whose job is exactly that.

// deliveryCtx presents a workflow.Context as a delivery.Ctx.
//
// Now is promoted from the embedded interface unchanged, because its signature
// involves no Future and therefore already matches.
type deliveryCtx struct{ workflow.Context }

var _ delivery.Ctx = deliveryCtx{}

func (c deliveryCtx) ExecuteActivity(activityType string, input any) delivery.Future {
	return c.Context.ExecuteActivity(activityType, input)
}

func (c deliveryCtx) Sleep(d time.Duration) delivery.Future { return c.Context.Sleep(d) }

func (c deliveryCtx) WaitForSignal(name string) delivery.Future {
	return c.Context.WaitForSignal(name)
}

// agentCtx presents a workflow.Context as an agent.Ctx.
type agentCtx struct{ workflow.Context }

var _ agent.Ctx = agentCtx{}

func (c agentCtx) ExecuteActivity(activityType string, input any) agent.Future {
	return c.Context.ExecuteActivity(activityType, input)
}

func (c agentCtx) Sleep(d time.Duration) agent.Future { return c.Context.Sleep(d) }

func (c agentCtx) WaitForSignal(name string) agent.Future { return c.Context.WaitForSignal(name) }

// DeliveryWorkflow is the delivery saga in the shape the registry takes.
func DeliveryWorkflow(ctx workflow.Context, input json.RawMessage) (json.RawMessage, error) {
	return delivery.RunJSON(deliveryCtx{ctx}, input)
}

// AgentWorkflow is the tool-calling agent in the shape the registry takes.
func AgentWorkflow(ctx workflow.Context, input json.RawMessage) (json.RawMessage, error) {
	return agent.RunJSON(agentCtx{ctx}, input)
}

// RegisterDemoWorkflows adds both demonstration workflows under the names
// their own packages declare. The name is what history records, so it has to
// come from the application, never from a string typed here.
func RegisterDemoWorkflows(r *workflow.Registry) error {
	if err := r.Register(delivery.WorkflowType, DeliveryWorkflow); err != nil {
		return err
	}
	return r.Register(agent.WorkflowType, AgentWorkflow)
}
