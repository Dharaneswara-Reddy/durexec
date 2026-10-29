package extsim

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/dharaneswar339/durexec/internal/workflow"
)

// recordingCtx is a workflow.Context that records what was scheduled and then
// refuses to answer. It exists so the probe's own control flow can be tested
// without a database, a dispatcher or a real activity worker.
type recordingCtx struct {
	calls     int
	lastType  string
	lastInput any
}

func (c *recordingCtx) ExecuteActivity(activityType string, input any) workflow.Future {
	c.calls++
	c.lastType = activityType
	c.lastInput = input
	return failedFuture{}
}

func (c *recordingCtx) Sleep(time.Duration) workflow.Future   { return failedFuture{} }
func (c *recordingCtx) WaitForSignal(string) workflow.Future  { return failedFuture{} }
func (c *recordingCtx) SideEffect(func() any) workflow.Future { return failedFuture{} }
func (c *recordingCtx) Now() time.Time                        { return time.Unix(0, 0).UTC() }

type failedFuture struct{}

func (failedFuture) Get() (json.RawMessage, error) {
	return nil, errors.New("recordingCtx: no answer")
}

// recordingOKCtx answers activities successfully, so a workflow's own control
// flow after a successful external effect can be tested.
type recordingOKCtx struct {
	calls int
}

func (c *recordingOKCtx) ExecuteActivity(activityType string, input any) workflow.Future {
	c.calls++
	out, _ := json.Marshal(PayOutput{EffectID: "eff-1", Amount: 1, Payee: "p", DedupKeyUsed: "k"})
	return okFuture{out}
}
func (c *recordingOKCtx) Sleep(time.Duration) workflow.Future   { return okFuture{[]byte(`null`)} }
func (c *recordingOKCtx) WaitForSignal(string) workflow.Future  { return okFuture{[]byte(`null`)} }
func (c *recordingOKCtx) SideEffect(func() any) workflow.Future { return okFuture{[]byte(`null`)} }
func (c *recordingOKCtx) Now() time.Time                        { return time.Unix(0, 0).UTC() }

type okFuture struct{ v json.RawMessage }

func (f okFuture) Get() (json.RawMessage, error) { return f.v, nil }
