package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ErrorResponse is the one shape every failure takes.
//
// Retry is part of the body rather than left to the status code because the
// status codes here are not self-explanatory: a 409 from this API means "ask a
// different node", which is retryable, while a 409 elsewhere usually means
// "your request conflicts, do not repeat it". Saying so explicitly stops every
// client from having to encode our table of status codes for itself.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
	Retry   bool   `json:"retry,omitempty"`

	// NotOwner
	ShardID      *core.ShardID `json:"shard_id,omitempty"`
	CurrentOwner string        `json:"current_owner,omitempty"`

	// NonDeterminism. These four fields are the whole reason a replay bug is
	// debuggable at all, so they are exposed rather than swallowed.
	Seq       *core.Seq `json:"seq,omitempty"`
	K         *int      `json:"k,omitempty"`
	Recorded  string    `json:"recorded,omitempty"`
	Requested string    `json:"requested,omitempty"`

	// Compensation
	Status   core.ExecutionStatus `json:"status,omitempty"`
	Activity string               `json:"activity,omitempty"`
	Attempts *int                 `json:"attempts,omitempty"`
}

// retryAfterSeconds is what we ask a client to wait when we have lost a shard.
// It is roughly one lease acquisition: retrying sooner just burns a request
// against a shard nobody owns yet.
const retryAfterSeconds = "2"

// writeError is the single place an error becomes an HTTP response.
//
// It is one function, not a decision scattered through the handlers, because
// the mapping IS the API contract. A client's whole retry strategy is derived
// from this table, and the fastest way to break that strategy is for two
// handlers to disagree about what a lost lease looks like.
//
// The table:
//
//	*core.NotOwnerError            409  routing problem, retryable elsewhere
//	core.ErrWorkflowAlreadyOpen    200  success — see below
//	core.ErrWorkflowNotFound       404
//	*core.NonDeterminismError      500  bug in workflow code, exposes the divergence
//	*core.CompensationFailedError  200  needs a human, not a retry
//	core.ErrLostOwnership          503  transient, with Retry-After
//	anything else                  500  generic, leaks nothing
func writeError(w http.ResponseWriter, err error) {
	// ---- 409 NotOwner ----
	// The request reached the wrong node. This is a routing problem, not a
	// workflow problem: nothing was attempted and nothing was changed, so the
	// client should simply ask the named owner instead.
	var notOwner *core.NotOwnerError
	if errors.As(err, &notOwner) {
		shard := notOwner.ShardID
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error:        "NotOwner",
			Message:      notOwner.Error(),
			ShardID:      &shard,
			CurrentOwner: notOwner.CurrentOwner,
			Retry:        true,
		})
		return
	}

	// ---- 200 AlreadyOpen ----
	// A duplicate start is a SUCCESS, not an error. This is the front door's
	// idempotency guarantee: the executions table is keyed on (shard,
	// workflow_id) with no run id, so at most one run can be open per business
	// key, and a second start request simply loses that race. The caller asked
	// for the workflow to be running, and it is running — returning 409 here
	// would push every client into writing retry logic that treats a conflict
	// as fine, which is a bug waiting to happen at the first network blip.
	//
	// Handlers that can look the run up return the full existing execution
	// instead of coming through here; this branch is the fallback for a caller
	// that cannot, so the status code stays consistent either way.
	if errors.Is(err, core.ErrWorkflowAlreadyOpen) {
		writeJSON(w, http.StatusOK, map[string]any{
			"already_open": true,
			"message":      "workflow is already running; the existing run was returned",
		})
		return
	}

	// ---- 404 NotFound ----
	if errors.Is(err, core.ErrWorkflowNotFound) {
		writeJSON(w, http.StatusNotFound, ErrorResponse{
			Error:   "WorkflowNotFound",
			Message: "no workflow with that id on this shard",
		})
		return
	}

	// ---- 500 NonDeterminism ----
	// Replay took a different path than the original run. The engine cannot
	// repair this: it has no way to know which version of the workflow code
	// told the truth. All four coordinates of the divergence are exposed —
	// where in history, which step, what was recorded, what the code asked for
	// this time — because that is what somebody needs to find the offending
	// line, and no amount of retrying will help.
	var nd *core.NonDeterminismError
	if errors.As(err, &nd) {
		seq, k := nd.Seq, nd.K
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{
			Error:     "NonDeterminism",
			Message:   nd.Error(),
			Seq:       &seq,
			K:         &k,
			Recorded:  nd.Recorded,
			Requested: nd.Requested,
		})
		return
	}

	// ---- 200 CompensationFailed ----
	// The rollback itself failed, so the workflow is in a known-bad state that
	// only a person can resolve. It is reported as 200 with an explicit
	// COMPENSATING_FAILED status rather than as a 5xx precisely so that no
	// client retries it: a 5xx invites a retry, and retrying a failed
	// compensation just fails it again while hiding the state that needs
	// attention. The request succeeded; the news it carries is bad.
	var comp *core.CompensationFailedError
	if errors.As(err, &comp) {
		attempts := comp.Attempts
		writeJSON(w, http.StatusOK, ErrorResponse{
			Error:    "CompensationFailed",
			Message:  comp.Error(),
			Status:   core.StatusCompensatingFailed,
			Activity: comp.Activity,
			Attempts: &attempts,
			Retry:    false,
		})
		return
	}

	// ---- 503 LostOwnership ----
	// The fencing token moved while we were working, so this node abandoned
	// the transaction. Nothing was committed. Another node is taking the shard
	// over right now, so the request is retryable after a short wait.
	if errors.Is(err, core.ErrLostOwnership) {
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{
			Error:   "LostOwnership",
			Message: "shard ownership changed while the request was in flight; retry",
			Retry:   true,
		})
		return
	}

	// ---- 500 default ----
	// Deliberately generic. An unrecognised error can be a driver error
	// carrying a connection string or a query fragment, and none of that
	// belongs in a response body. The detail goes to the log, keyed by the
	// request id, where an operator can find it.
	writeJSON(w, http.StatusInternalServerError, ErrorResponse{
		Error:   "Internal",
		Message: "internal error",
	})
}

// writeJSON writes a body with the status. It never returns an error: by the
// time encoding fails the status line is already on the wire, so there is
// nothing left to say to the client.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// writeBadRequest reports a malformed request. It is separate from writeError
// because a bad request is not an engine error at all — there is no core error
// type for "your JSON was broken", and inventing one would put client mistakes
// into the same taxonomy as shard failures.
func writeBadRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "BadRequest", Message: msg})
}

// writeNotImplemented is returned when the integrator has not wired a
// component in yet. Being explicit beats a nil-pointer panic that takes the
// whole node down during a demo.
func writeNotImplemented(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotImplemented, ErrorResponse{
		Error:   "NotImplemented",
		Message: what + " is not configured on this node",
	})
}
