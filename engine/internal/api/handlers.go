package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// maxBodyBytes caps a request body. Every body this API accepts is a handful
// of fields plus a workflow input; a megabyte is generous, and the cap means a
// malformed or hostile client cannot make a node allocate without bound.
const maxBodyBytes = 1 << 20

// defaultListLimit and maxListLimit bound the collection endpoints, so a
// client that forgets to page cannot pull an entire shard into memory.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

// ---- request and response shapes ------------------------------------------

type StartWorkflowBody struct {
	WorkflowID   string          `json:"workflow_id"`
	WorkflowType string          `json:"workflow_type"`
	Input        json.RawMessage `json:"input,omitempty"`
}

// WorkflowResponse is the one representation of a workflow, used by the start,
// get and list endpoints alike so a client parses a single shape.
type WorkflowResponse struct {
	WorkflowID   string               `json:"workflow_id"`
	RunID        core.RunID           `json:"run_id"`
	ShardID      core.ShardID         `json:"shard_id"`
	WorkflowType string               `json:"workflow_type,omitempty"`
	Status       core.ExecutionStatus `json:"status"`
	LastSeq      core.Seq             `json:"last_seq"`
	// AlreadyOpen tells a caller its start request collided with a run that
	// was already going. It is information, not a failure.
	AlreadyOpen bool `json:"already_open,omitempty"`
}

type HistoryResponse struct {
	WorkflowID string       `json:"workflow_id"`
	RunID      core.RunID   `json:"run_id"`
	ShardID    core.ShardID `json:"shard_id"`
	Count      int          `json:"count"`
	Events     []core.Event `json:"events"`
}

type ListWorkflowsResponse struct {
	Count      int                `json:"count"`
	Executions []WorkflowResponse `json:"executions"`
}

type SignalBody struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type CancelBody struct {
	Reason string `json:"reason,omitempty"`
}

type AcceptedResponse struct {
	Accepted   bool         `json:"accepted"`
	WorkflowID string       `json:"workflow_id"`
	ShardID    core.ShardID `json:"shard_id"`
}

// VerdictRecord is one journaled semantic dedup decision, read straight out of
// the event log.
//
// There is no separate verdicts table and no query interface for one, on
// purpose. The verdict is a core.DedupEvaluated event inside the run's
// history, and this endpoint reads it from there. That is the strongest
// possible demonstration of the design claim: a skipped action is explainable
// from the log alone, with no side store that could disagree with it.
type VerdictRecord struct {
	ShardID    core.ShardID `json:"shard_id"`
	WorkflowID string       `json:"workflow_id"`
	RunID      core.RunID   `json:"run_id"`
	Seq        core.Seq     `json:"seq"`
	At         time.Time    `json:"at"`
	core.DedupEvaluated
}

type VerdictsResponse struct {
	Count    int             `json:"count"`
	Verdicts []VerdictRecord `json:"verdicts"`
}

type ShardView struct {
	ShardID   core.ShardID      `json:"shard_id"`
	Owner     string            `json:"owner,omitempty"`
	Token     core.FencingToken `json:"fencing_token"`
	ExpiresAt time.Time         `json:"expires_at"`
	// Expired is computed against this node's clock so an operator can see a
	// dead lease without doing the date arithmetic themselves.
	Expired bool `json:"expired"`
	Self    bool `json:"self"`
}

type ShardsResponse struct {
	Node   string      `json:"node"`
	Count  int         `json:"count"`
	Shards []ShardView `json:"shards"`
}

type HealthResponse struct {
	Status string `json:"status"`
	Node   string `json:"node"`
	// Shards is how many shards this node holds a lease on right now, which
	// is normally a fraction of the cluster's total and is zero on a node that
	// has just started or has been fenced out of everything. It is not a
	// health verdict: a node owning nothing is still serving, still forwarding
	// and still a candidate for the next lease.
	Shards int `json:"owned_shards"`
	// ClusterShards is the total the cluster is configured for, so a reader
	// can see 2-of-16 for what it is without a second request. The two were
	// the same number for every node until owned_shards started being
	// measured rather than assumed.
	ClusterShards int `json:"cluster_shards"`
}

// ---- workflow lifecycle ---------------------------------------------------

// Start outcomes. The vocabulary is spelled once, here, so that the same
// event cannot appear under two names and split one time series in two — the
// same reason internal/runtime names its failure reasons in one const block.
//
// Every value is a fixed string. None is derived from a request field or from
// an error message, because the label values of a counter become series in the
// time-series database: a label built from err.Error() would mint a new series
// for every distinct error string, including ones carrying a workflow id, and
// a metrics endpoint is the last place a per-request identifier should appear.
const (
	// OutcomeAccepted is a run this node created and persisted.
	OutcomeAccepted = "accepted"
	// OutcomeAlreadyOpen is a duplicate start for a run already open. It is
	// reported as success to the client, and is counted separately because a
	// client retrying is a different story from a client starting new work.
	OutcomeAlreadyOpen = "already_open"
	// OutcomeInvalid is a request rejected before any routing or storage
	// decision: the caller's fault, and never the engine's.
	OutcomeInvalid = "invalid"
	// OutcomeNotOwned is a request this node did not serve itself because the
	// shard belongs elsewhere — forwarded to the owner, or refused naming it.
	OutcomeNotOwned = "not_owned"
	// OutcomeUnavailable is a node with no starter wired at all.
	OutcomeUnavailable = "unavailable"
	// OutcomeRefused is a start the engine attempted and could not commit,
	// which is the case the PostgreSQL outage tests produced 183 times.
	OutcomeRefused = "refused"
)

// observeStart records one start outcome, if a node has an observer wired.
//
// The nil check lives here rather than at each call site so that adding an
// outcome cannot accidentally reintroduce a nil dereference in a test server
// built without metrics.
func (s *Server) observeStart(workflowType, outcome string) {
	if s.deps.StartObserver == nil || outcome == "" {
		return
	}
	// An empty workflow_type would be a label value of "", which is a legal
	// but confusing series. Name it instead.
	if workflowType == "" {
		workflowType = "unspecified"
	}
	s.deps.StartObserver.WorkflowStartObserved(workflowType, outcome)
}

// startOutcome maps the error from a start attempt to one of the bounded
// outcome labels above.
//
// This function is the boundary that keeps unbounded strings out of the label
// space. Every return is a constant from the block above; nothing is derived
// from err.Error(). That matters more here than it looks: NotOwnerError
// formats a shard id and an owner name, and CompensationFailedError and
// NonDeterminismError format workflow ids, so a label built from an error
// message would mint a fresh time series per workflow and publish run
// identifiers on /metrics.
//
// A nil error returns "", which observeStart reads as "do not count". The
// success path is counted by the caller as OutcomeAccepted, and counting it
// here as well would double every accepted start.
func startOutcome(err error) string {
	if err == nil {
		return ""
	}

	// Both ownership cases collapse to OutcomeNotOwned, and deliberately so.
	// What they share is the fact that matters operationally: the engine did
	// not attempt to persist anything, so the request is not evidence of a
	// durability problem. Counting a lease that lapsed mid-start as "refused"
	// would make every failover look like a burst of commit failures, which
	// is exactly the misreading this metric exists to prevent — during a
	// takeover these are expected and the client's retry reaches the new
	// owner.
	//
	// errors.As for NotOwnerError because it is a struct pointer carrying the
	// shard and current owner; errors.Is for ErrLostOwnership because it is a
	// sentinel. Both unwrap, so a wrapped error is still classified.
	var notOwner *core.NotOwnerError
	if errors.As(err, &notOwner) {
		return OutcomeNotOwned
	}
	if errors.Is(err, core.ErrLostOwnership) {
		return OutcomeNotOwned
	}

	// Everything else is a start the engine tried to commit and could not:
	// the database unreachable, a statement timeout, a context deadline. This
	// is the case the PostgreSQL outage tests produced 183 times, and the one
	// worth alerting on.
	return OutcomeRefused
}

// handleStartWorkflow opens a workflow, or reports the one already open.
func (s *Server) handleStartWorkflow(w http.ResponseWriter, r *http.Request) {
	var body StartWorkflowBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.WorkflowID == "" || body.WorkflowType == "" {
		s.observeStart(body.WorkflowType, OutcomeInvalid)
		writeBadRequest(w, "workflow_id and workflow_type are required")
		return
	}

	rt, ok := s.route(w, r, body.WorkflowID)
	if !ok {
		s.observeStart(body.WorkflowType, OutcomeNotOwned)
		return
	}
	if s.deps.Starter == nil {
		s.observeStart(body.WorkflowType, OutcomeUnavailable)
		writeNotImplemented(w, "workflow starter")
		return
	}

	res, err := s.deps.Starter.StartWorkflow(r.Context(), StartRequest{
		ShardID:        rt.ShardID,
		WorkflowID:     body.WorkflowID,
		WorkflowType:   body.WorkflowType,
		Input:          body.Input,
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey),
	})

	// A duplicate start is success. Rather than reporting a bare "already
	// open", look the existing run up and return it in full, so the client
	// gets the same body it would have got had it won the race and needs no
	// special case of its own.
	if errors.Is(err, core.ErrWorkflowAlreadyOpen) {
		s.observeStart(body.WorkflowType, OutcomeAlreadyOpen)
		if s.deps.Executions != nil {
			if exec, getErr := s.deps.Executions.Get(r.Context(), rt.ShardID, body.WorkflowID); getErr == nil {
				resp := executionResponse(exec)
				resp.AlreadyOpen = true
				writeJSON(w, http.StatusOK, resp)
				return
			}
		}
		writeError(w, err)
		return
	}
	if err != nil {
		s.observeStart(body.WorkflowType, startOutcome(err))
		s.logError(r, "start workflow failed", err)
		writeError(w, err)
		return
	}
	// A duplicate start reaches this point two different ways: as
	// ErrWorkflowAlreadyOpen, handled above, or as a SUCCESS carrying
	// res.AlreadyOpen — which is the path the engine actually takes, because
	// the executions row is opened in the same transaction as the start event
	// and the loser of that race is told about the winner rather than given
	// an error. Counting only the error form reported duplicates as
	// "accepted": a validation run that issued one create and one duplicate
	// produced accepted=2 and already_open=0.
	outcome := OutcomeAccepted
	if res.AlreadyOpen {
		outcome = OutcomeAlreadyOpen
	}
	s.observeStart(body.WorkflowType, outcome)

	status := res.Status
	if status == "" {
		status = core.StatusRunning
	}
	resp := WorkflowResponse{
		WorkflowID:   body.WorkflowID,
		RunID:        res.RunID,
		ShardID:      rt.ShardID,
		WorkflowType: body.WorkflowType,
		Status:       status,
		AlreadyOpen:  res.AlreadyOpen,
	}
	// 201 for a run we created, 200 for one that already existed. Both are
	// success; the code just says whether anything new came into being.
	if res.AlreadyOpen {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rt, ok := s.route(w, r, id)
	if !ok {
		return
	}
	if s.deps.Executions == nil {
		writeNotImplemented(w, "execution store")
		return
	}

	exec, err := s.deps.Executions.Get(r.Context(), rt.ShardID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, executionResponse(exec))
}

// handleGetHistory returns a run's raw history.
//
// The events are returned exactly as stored, not summarised. History is the
// engine's ground truth, and anything that reshapes it on the way out is
// another place the view and the truth can drift apart.
func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rt, ok := s.route(w, r, id)
	if !ok {
		return
	}
	if s.deps.Executions == nil || s.deps.Events == nil {
		writeNotImplemented(w, "event store")
		return
	}

	// The run id comes from the executions pointer rather than from the
	// client, so a caller cannot ask for a run that does not belong to this
	// business key.
	exec, err := s.deps.Executions.Get(r.Context(), rt.ShardID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	events, err := s.deps.Events.Load(r.Context(), rt.ShardID, id, exec.RunID)
	if err != nil {
		writeError(w, err)
		return
	}
	if events == nil {
		events = []core.Event{}
	}
	writeJSON(w, http.StatusOK, HistoryResponse{
		WorkflowID: id,
		RunID:      exec.RunID,
		ShardID:    rt.ShardID,
		Count:      len(events),
		Events:     events,
	})
}

// handleSignal delivers an external event into a running workflow.
//
// It answers 202, not 200. The signal is durably recorded, but the workflow
// has not necessarily reacted to it yet — it may be asleep on a timer for
// another week. Claiming 200 would suggest the effect has happened.
func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body SignalBody
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Name == "" {
		writeBadRequest(w, "name is required")
		return
	}

	rt, ok := s.route(w, r, id)
	if !ok {
		return
	}
	if s.deps.Signaller == nil {
		writeNotImplemented(w, "signaller")
		return
	}

	if err := s.deps.Signaller.Signal(r.Context(), SignalRequest{
		ShardID:        rt.ShardID,
		WorkflowID:     id,
		Name:           body.Name,
		Payload:        body.Payload,
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey),
	}); err != nil {
		s.logError(r, "signal failed", err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, AcceptedResponse{Accepted: true, WorkflowID: id, ShardID: rt.ShardID})
}

// handleCancel asks a workflow to stop. Also 202: in a saga, cancelling means
// running the compensations, which is more work, not less.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A cancel body is optional, so an empty one is not an error.
	var body CancelBody
	if !decodeOptionalBody(w, r, &body) {
		return
	}

	rt, ok := s.route(w, r, id)
	if !ok {
		return
	}
	if s.deps.Canceller == nil {
		writeNotImplemented(w, "canceller")
		return
	}

	if err := s.deps.Canceller.Cancel(r.Context(), CancelRequest{
		ShardID:        rt.ShardID,
		WorkflowID:     id,
		Reason:         body.Reason,
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey),
	}); err != nil {
		s.logError(r, "cancel failed", err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, AcceptedResponse{Accepted: true, WorkflowID: id, ShardID: rt.ShardID})
}

// handleListWorkflows lists executions across the shards this node owns.
//
// It does not fan out to other nodes. A node answers for its own shards only,
// which keeps the endpoint free of any cross-node call that could hang, and
// makes the union of all nodes' answers the cluster-wide view.
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	if s.deps.Executions == nil {
		writeNotImplemented(w, "execution store")
		return
	}
	status := core.ExecutionStatus(r.URL.Query().Get("status"))
	limit := clampLimit(intParam(r, "limit"))

	shards, ok := s.requestShards(w, r)
	if !ok {
		return
	}

	// Each shard is asked for a full page, not for whatever is left over. Taking
	// limit-len(out) from each in turn fills the whole page from the first shard
	// that has enough rows, so a run on a later shard stays invisible no matter
	// how recent it is — which is the bug this replaces.
	gathered := make([]core.Execution, 0, limit*2)
	for _, shard := range shards {
		execs, err := s.deps.Executions.List(r.Context(), shard, status, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		gathered = append(gathered, execs...)
	}

	orderExecutions(gathered)

	if len(gathered) > limit {
		gathered = gathered[:limit]
	}
	out := make([]WorkflowResponse, 0, len(gathered))
	for _, e := range gathered {
		out = append(out, executionResponse(e))
	}
	writeJSON(w, http.StatusOK, ListWorkflowsResponse{Count: len(out), Executions: out})
}

// ---- the contribution's observability surface -----------------------------

// handleDedupVerdicts returns journaled semantic dedup decisions.
//
// With workflow_id it returns that workflow's verdicts; without, it sweeps the
// shards this node owns. Either way it reads history and filters for the
// SemanticDedupEvaluated event, so what a reviewer sees here is exactly what
// replay will see — there is no second copy of the decision that could be
// right when the log is wrong.
func (s *Server) handleDedupVerdicts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Executions == nil || s.deps.Events == nil {
		writeNotImplemented(w, "event store")
		return
	}
	limit := clampLimit(intParam(r, "limit"))

	if id := r.URL.Query().Get("workflow_id"); id != "" {
		rt, ok := s.route(w, r, id)
		if !ok {
			return
		}
		exec, err := s.deps.Executions.Get(r.Context(), rt.ShardID, id)
		if err != nil {
			writeError(w, err)
			return
		}
		records, err := s.verdictsFor(r, exec, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, VerdictsResponse{Count: len(records), Verdicts: records})
		return
	}

	shards, ok := s.requestShards(w, r)
	if !ok {
		return
	}
	out := make([]VerdictRecord, 0, limit)
	for _, shard := range shards {
		if len(out) >= limit {
			break
		}
		execs, err := s.deps.Executions.List(r.Context(), shard, "", maxListLimit)
		if err != nil {
			writeError(w, err)
			return
		}
		for _, e := range execs {
			if len(out) >= limit {
				break
			}
			records, err := s.verdictsFor(r, e, limit-len(out))
			if err != nil {
				writeError(w, err)
				return
			}
			out = append(out, records...)
		}
	}
	writeJSON(w, http.StatusOK, VerdictsResponse{Count: len(out), Verdicts: out})
}

// verdictsFor pulls the dedup events out of one run's history.
func (s *Server) verdictsFor(r *http.Request, exec core.Execution, limit int) ([]VerdictRecord, error) {
	events, err := s.deps.Events.Load(r.Context(), exec.ShardID, exec.WorkflowID, exec.RunID)
	if err != nil {
		return nil, err
	}
	out := make([]VerdictRecord, 0)
	for _, ev := range events {
		if ev.Type != core.EvDedupEvaluated {
			continue
		}
		if len(out) >= limit {
			break
		}
		var payload core.DedupEvaluated
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			// One unreadable payload must not hide the rest of the history.
			// Skip it and keep going; the raw event is still available on the
			// history endpoint for whoever needs to look at it.
			continue
		}
		out = append(out, VerdictRecord{
			ShardID:        ev.ShardID,
			WorkflowID:     ev.WorkflowID,
			RunID:          ev.RunID,
			Seq:            ev.Seq,
			At:             ev.CreatedAt,
			DedupEvaluated: payload,
		})
	}
	return out, nil
}

// ---- admin ----------------------------------------------------------------

// handleFailStep arms a deliberate failure, for the fault-injection runs the
// evaluation depends on. It routes like any other workflow request, because a
// fault has to be armed on the node that will actually execute the step.
func (s *Server) handleFailStep(w http.ResponseWriter, r *http.Request) {
	var body FailStepRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if body.WorkflowID == "" {
		writeBadRequest(w, "workflow_id is required")
		return
	}

	rt, ok := s.route(w, r, body.WorkflowID)
	if !ok {
		return
	}
	if s.deps.Faults == nil {
		writeNotImplemented(w, "fault injector")
		return
	}

	body.ShardID = rt.ShardID
	if err := s.deps.Faults.FailStep(r.Context(), body); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, AcceptedResponse{
		Accepted: true, WorkflowID: body.WorkflowID, ShardID: rt.ShardID,
	})
}

// handleShards is the cluster view: who owns what, with which fencing token.
//
// The fencing token is exposed because it is the number that makes the safety
// argument checkable. Watching it increase by one on every handover is how you
// confirm from outside the process that ownership really did move, rather than
// two nodes both believing they hold the shard.
func (s *Server) handleShards(w http.ResponseWriter, r *http.Request) {
	if s.deps.Leases == nil {
		writeNotImplemented(w, "lease store")
		return
	}
	leases, err := s.deps.Leases.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	now := s.deps.Clock.Now()
	views := make([]ShardView, 0, len(leases))
	for _, l := range leases {
		views = append(views, ShardView{
			ShardID:   l.ShardID,
			Owner:     l.Owner,
			Token:     l.Token,
			ExpiresAt: l.ExpiresAt,
			Expired:   !l.ExpiresAt.After(now),
			Self:      l.Owner == s.deps.NodeID && l.ExpiresAt.After(now),
		})
	}
	writeJSON(w, http.StatusOK, ShardsResponse{Node: s.deps.NodeID, Count: len(views), Shards: views})
}

// handleHealthz reports that this process is up and serving.
//
// It deliberately does not check the database. This endpoint drives the
// restart decision, and a node that is running fine but cannot momentarily
// reach Postgres must not be killed for it — the lease will expire on its own
// and another node will take the shard over, which is the designed response.
// Restarting the process as well would only lengthen the outage.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	// OwnedNow, not OwnedShards: the latter is every shard in the cluster, on
	// purpose (see api.Deps), so reporting its length told every reader that
	// every node owned everything. A node with no leases at all answered the
	// same as a node carrying the whole workload.
	owned := len(s.deps.OwnedShards)
	if s.deps.OwnedNow != nil {
		owned = len(s.deps.OwnedNow())
	}
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:        "ok",
		Node:          s.deps.NodeID,
		Shards:        owned,
		ClusterShards: len(s.deps.OwnedShards),
	})
}

// ---- helpers --------------------------------------------------------------

// orderExecutions sorts rows gathered from several shards into the single order
// the dashboard shows them in. Each shard arrives already sorted, but the
// concatenation of sorted lists is not itself sorted, so the merged slice has
// to be put in order here before it is truncated to a page.
//
// The ordering must be total: two runs created in the same millisecond still
// need a stable relative position, or rows will swap places between polls and
// the list will appear to flicker.
func orderExecutions(execs []core.Execution) {
	slices.SortFunc(execs, func(a, b core.Execution) int {
		// Newest first, so a run the operator just started appears at the top.
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		// Two runs can share a timestamp, and SortFunc is not stable, so the
		// comparison continues into the primary key. Without this, equal-time
		// rows swap places on every poll and the table visibly jitters.
		if c := strings.Compare(a.WorkflowID, b.WorkflowID); c != 0 {
			return c
		}
		return int(a.ShardID) - int(b.ShardID)
	})
}

func executionResponse(e core.Execution) WorkflowResponse {
	return WorkflowResponse{
		WorkflowID:   e.WorkflowID,
		RunID:        e.RunID,
		ShardID:      e.ShardID,
		WorkflowType: e.WorkflowType,
		Status:       e.Status,
		LastSeq:      e.LastSeq,
	}
}

// requestShards resolves which shards a collection endpoint should read. An
// explicit ?shard= wins; otherwise it is the set this node owns.
func (s *Server) requestShards(w http.ResponseWriter, r *http.Request) ([]core.ShardID, bool) {
	if raw := r.URL.Query().Get("shard"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			writeBadRequest(w, "shard must be an integer")
			return nil, false
		}
		return []core.ShardID{core.ShardID(n)}, true
	}
	return s.deps.OwnedShards, true
}

// decodeBody reads a required JSON body, rejecting unknown fields.
//
// Unknown fields are an error rather than being ignored, because the usual
// cause is a typo in a field name, and silently accepting it means the request
// appears to succeed while the value the caller cared about was dropped.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeBadRequest(w, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// decodeOptionalBody is decodeBody for an endpoint where an empty body is
// legitimate, such as cancel.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeBadRequest(w, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func intParam(r *http.Request, name string) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return 0
	}
	return n
}

// clampLimit turns a caller-supplied limit into one we will actually serve.
// Zero and negative mean "unspecified", not "none".
func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultListLimit
	case n > maxListLimit:
		return maxListLimit
	default:
		return n
	}
}

func (s *Server) logError(r *http.Request, msg string, err error) {
	if s.deps.Log == nil {
		return
	}
	s.deps.Log.Error(r.Context(), msg, "path", r.URL.Path, "err", err.Error())
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
