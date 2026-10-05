package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Driving a live cluster is how the tool turns "the engine could do this" into
// "the engine did this, at 14:02, and here is the run id". Every workflow it
// starts is prefixed so that an examiner reading the database can tell the
// tool's traffic apart from the demonstration's.

// Driven is the outcome of one workflow this tool started.
type Driven struct {
	ID       string
	Type     string
	ShardID  int32
	RunID    string
	Started  bool
	Terminal bool
	Status   string
	Elapsed  time.Duration
	Hops     []string
	Err      string
	// SignalSent records that a signal was delivered, and SignalAt when,
	// relative to the start. FR-5 is about a signal that arrives before the
	// workflow is waiting for it, so the timing is the evidence.
	SignalSent bool
	SignalAt   time.Duration
}

func (d Driven) Failed() bool { return d.Err != "" }

// terminalStatuses are the statuses after which nothing further will be
// scheduled. COMPENSATING is deliberately absent: a rollback in progress is
// still work in flight.
func isTerminal(status string) bool {
	switch status {
	case statusCompleted, statusFailed, statusCompensatingFailed:
		return true
	}
	return false
}

// deliveryOrder builds a food-delivery workflow input with a short durable
// sleep, so the timer half of FR-4 can be observed in seconds rather than the
// twelve minutes the demonstration application defaults to.
func deliveryOrder(id string, sleep time.Duration) map[string]any {
	return map[string]any{
		"order_id":      id,
		"customer_id":   "acceptance-customer",
		"restaurant_id": "acceptance-restaurant",
		"items": []map[string]any{
			{"sku": "acceptance-dish", "quantity": 1},
		},
		"amount_cents": 2450,
		"currency":     "GBP",
		// The delivery workflow sleeps for prep_time_ns between telling the
		// kitchen and dispatching a driver. Setting it is what makes this run
		// a durable-timer test rather than a twelve-minute wait.
		"prep_time_ns": sleep.Nanoseconds(),
	}
}

// agentRequest builds a travel-agent workflow input. It is the second
// application FR-16 names, running on the same binary with no recompilation.
func agentRequest(id string) map[string]any {
	return map[string]any{
		"request_id":     id,
		"user_id":        "acceptance-user",
		"goal":           "book one flight from London to Edinburgh next Tuesday morning",
		"max_iterations": 2,
	}
}

// Start opens a workflow and records how it was routed.
func (c *Cluster) Start(ctx context.Context, d *Driven, input any) {
	began := time.Now()
	wf, resp, err := c.StartWorkflow(ctx, d.ID, d.Type, input)
	if resp != nil {
		d.Hops = resp.Hops
	}
	if err != nil {
		d.Err = err.Error()
		return
	}
	d.Started = true
	d.RunID = wf.RunID
	d.ShardID = wf.ShardID
	d.Status = wf.Status
	d.Elapsed = time.Since(began)
}

// WaitTerminal polls until the workflow reaches a terminal state or the budget
// runs out. Polling rather than subscribing is deliberate: the query API is
// the only status surface a client has, so the tool uses exactly what a client
// would and inherits the same view.
func (c *Cluster) WaitTerminal(ctx context.Context, d *Driven, budget, interval time.Duration) {
	if !d.Started {
		return
	}
	began := time.Now()
	deadline := began.Add(budget)
	for {
		wf, _, err := c.GetWorkflow(ctx, d.ID)
		if err == nil {
			d.Status = wf.Status
			if isTerminal(wf.Status) {
				d.Terminal = true
				d.Elapsed = time.Since(began)
				return
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			d.Elapsed = time.Since(began)
			if err != nil && d.Err == "" {
				d.Err = err.Error()
			}
			return
		}
		select {
		case <-ctx.Done():
			d.Elapsed = time.Since(began)
			return
		case <-time.After(interval):
		}
	}
}

// SendSignal delivers a signal and records when, relative to the start.
func (c *Cluster) SendSignal(ctx context.Context, d *Driven, began time.Time, name string, payload any) error {
	resp, err := c.Signal(ctx, d.ID, name, payload)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusAccepted && resp.Status != http.StatusOK {
		return fmt.Errorf("POST /v1/workflows/%s/signals answered %d: %s",
			d.ID, resp.Status, firstLine(resp.Body))
	}
	d.SignalSent = true
	d.SignalAt = time.Since(began)
	return nil
}

// failStepBody arms a deliberate failure. It exists so the compensation path
// can be exercised on demand, and it is off by default: the injector is keyed
// by activity type and is process-wide, so arming one on a shared cluster
// would fail that activity for everybody's workflows, not just this tool's.
type failStepBody struct {
	WorkflowID   string `json:"workflow_id"`
	ActivityType string `json:"activity_type"`
	K            int    `json:"k"`
	Attempts     int    `json:"attempts"`
	NonRetryable bool   `json:"non_retryable"`
	Error        string `json:"error"`
}

// ArmFailure asks the owning node to fail one activity type. A 501 means the
// node was started without fault injection, which is a prerequisite problem
// and is reported as such rather than as a failure of the engine.
func (c *Cluster) ArmFailure(ctx context.Context, workflowID, activityType string) error {
	resp, err := c.Do(ctx, http.MethodPost, "/v1/admin/fail-step", failStepBody{
		WorkflowID:   workflowID,
		ActivityType: activityType,
		Attempts:     1,
		NonRetryable: true,
		Error:        "acceptance-tool injected failure to exercise compensation",
	})
	if err != nil {
		return err
	}
	switch resp.Status {
	case http.StatusAccepted, http.StatusOK:
		return nil
	case http.StatusNotImplemented:
		return fmt.Errorf("fault injection is not configured on this node (FAULT_INJECTION is unset)")
	default:
		return fmt.Errorf("POST /v1/admin/fail-step answered %d: %s", resp.Status, firstLine(resp.Body))
	}
}
