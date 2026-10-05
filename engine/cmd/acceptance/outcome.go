package main

import (
	"fmt"
	"sort"
)

// Outcome is one of exactly four verdicts. The set is deliberately small and
// deliberately not three: separating "a human has to do this" from "the check
// exists but could not run" is the difference between a gap in the method and
// a gap in this particular execution of it, and an examiner will ask which one
// a row is.
type Outcome string

const (
	// Pass means this tool checked the requirement's stated acceptance
	// condition against the running system and the condition held.
	Pass Outcome = "PASS"
	// Fail means this tool checked it and it did not hold.
	Fail Outcome = "FAIL"
	// Manual means the stated acceptance condition cannot be established from
	// outside the process by this tool — it needs a person, or a test that
	// runs inside the engine. The row names what that person must do.
	Manual Outcome = "MANUAL"
	// Blocked means the check is implemented and would have run, but
	// something it needs was not there. The row names the missing thing.
	Blocked Outcome = "BLOCKED"
)

// Result is one row of the acceptance matrix.
type Result struct {
	// Observed is what the tool actually measured, in plain numbers where it
	// measured anything. For a Blocked row it is empty.
	Observed string
	// Action is the human step for a Manual row, or the missing prerequisite
	// for a Blocked row. It is empty for Pass and Fail: nothing is owed.
	Action  string
	Outcome Outcome
	// Evaluated distinguishes a row a check deliberately left Blocked from a
	// row no check ever touched. Both print as BLOCKED, but only the second
	// is a hole in this tool rather than in the cluster it was pointed at.
	Evaluated bool
}

// Recorder collects one Result per requirement id.
//
// Every id in the spec starts Blocked and un-evaluated. A check has to say
// something about a row for it to become anything else, so a requirement this
// tool forgot about reports BLOCKED "no check ran" rather than quietly
// vanishing from the matrix or, worse, inheriting somebody else's PASS.
type Recorder struct {
	results map[string]Result
}

func NewRecorder(spec []Requirement) *Recorder {
	r := &Recorder{results: make(map[string]Result, len(spec))}
	for _, req := range spec {
		r.results[req.ID] = Result{
			Outcome: Blocked,
			Action:  "a check for this requirement in cmd/acceptance",
		}
	}
	return r
}

// Pass records that the stated acceptance condition was observed to hold.
// observed must say what was measured; a Pass with no measurement behind it is
// exactly the claim this tool exists to prevent.
func (r *Recorder) Pass(id, observed string, args ...any) {
	r.set(id, Result{Outcome: Pass, Observed: sprintf(observed, args...), Evaluated: true})
}

// Fail records that the condition was checked and did not hold.
func (r *Recorder) Fail(id, observed string, args ...any) {
	r.set(id, Result{Outcome: Fail, Observed: sprintf(observed, args...), Evaluated: true})
}

// Manual records that a person must establish this requirement, and what they
// must do. observed carries whatever this tool did manage to measure, which is
// usually a necessary condition of the requirement rather than the whole of it.
func (r *Recorder) Manual(id, humanStep, observed string, args ...any) {
	r.set(id, Result{
		Outcome:   Manual,
		Observed:  sprintf(observed, args...),
		Action:    humanStep,
		Evaluated: true,
	})
}

// Blocked records that the check could not run, naming the prerequisite.
func (r *Recorder) Blocked(id, prerequisite, reason string, args ...any) {
	r.set(id, Result{
		Outcome:   Blocked,
		Observed:  sprintf(reason, args...),
		Action:    prerequisite,
		Evaluated: true,
	})
}

// set refuses to downgrade a Fail.
//
// Checks overlap: the duplicate-completion scan touches FR-2, FR-8 and NFR-3,
// and the log-integrity scan touches FR-1 and FR-8. If one of them found a
// real violation, a later check that merely could not run must not overwrite
// that finding with a tidier-looking BLOCKED.
func (r *Recorder) set(id string, res Result) {
	if prev, ok := r.results[id]; ok && prev.Outcome == Fail && res.Outcome != Fail {
		return
	}
	r.results[id] = res
}

func (r *Recorder) Get(id string) Result { return r.results[id] }

// Tally counts each outcome.
func (r *Recorder) Tally() map[Outcome]int {
	out := map[Outcome]int{Pass: 0, Fail: 0, Manual: 0, Blocked: 0}
	for _, res := range r.results {
		out[res.Outcome]++
	}
	return out
}

// FailedIDs lists the ids that failed, in spec order, for the summary line.
func (r *Recorder) FailedIDs(spec []Requirement) []string {
	var out []string
	for _, req := range spec {
		if r.results[req.ID].Outcome == Fail {
			out = append(out, req.ID)
		}
	}
	return out
}

// AutomatedIDs lists the ids this run actually decided automatically — the
// answer to "which of these did you measure?".
func (r *Recorder) AutomatedIDs(spec []Requirement) []string {
	var out []string
	for _, req := range spec {
		switch r.results[req.ID].Outcome {
		case Pass, Fail:
			out = append(out, req.ID)
		}
	}
	return out
}

// ExitCode is non-zero only for a FAIL. A MANUAL or BLOCKED row is a statement
// about this tool's reach, not about the engine being wrong, and a build that
// went red because nobody had killed a node yet would train everyone to ignore
// the exit code.
func (r *Recorder) ExitCode() int {
	for _, res := range r.results {
		if res.Outcome == Fail {
			return 1
		}
	}
	return 0
}

// sortedIDs is only used by tests and diagnostics.
func (r *Recorder) sortedIDs() []string {
	out := make([]string, 0, len(r.results))
	for id := range r.results {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sprintf avoids turning a literal detail string containing a percent sign
// into a formatting accident when no arguments were supplied.
func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
