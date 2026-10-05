package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Everything in this file is a pure function of data already read. Nothing
// here touches the network, the clock or the database, which is what lets the
// whole analysis be tested against planted violations instead of hoping a
// broken cluster turns up.

// RunKey identifies one workflow run: the primary key of history, minus seq.
type RunKey struct {
	ShardID    int32
	WorkflowID string
	RunID      string
}

func (r RunKey) String() string {
	return fmt.Sprintf("shard %d / %s / run %s", r.ShardID, r.WorkflowID, shortRun(r.RunID))
}

func shortRun(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Run is one run's events, in the order the store returned them.
type Run struct {
	Key    RunKey
	Events []EventRow
}

// GroupByRun partitions a flat event list into runs, preserving both the order
// runs first appear and the order of events inside each run.
func GroupByRun(events []EventRow) []Run {
	index := make(map[RunKey]int)
	var runs []Run
	for _, ev := range events {
		k := RunKey{ShardID: ev.ShardID, WorkflowID: ev.WorkflowID, RunID: ev.RunID}
		i, ok := index[k]
		if !ok {
			i = len(runs)
			index[k] = i
			runs = append(runs, Run{Key: k})
		}
		runs[i].Events = append(runs[i].Events, ev)
	}
	return runs
}

// ---- FR-1: the log is append-only and ordered ------------------------------

// LogIntegrity is the result of the ordering scan.
type LogIntegrity struct {
	Runs   int
	Events int
	// Gaps is a seq that jumped, which would mean an event was never written
	// or was deleted. Either breaks "append-only and ordered".
	Gaps []string
	// Duplicates is the same (shard, workflow, run, seq) twice. The primary
	// key makes this impossible in PostgreSQL, so finding one means the rows
	// did not come from the schema the design document describes.
	Duplicates []string
	// BadStarts is a run whose first event is not seq 1.
	BadStarts []string
	// OutOfOrder is a seq lower than the one before it in load order.
	OutOfOrder []string
}

func (l LogIntegrity) OK() bool {
	return len(l.Gaps) == 0 && len(l.Duplicates) == 0 && len(l.BadStarts) == 0 && len(l.OutOfOrder) == 0
}

// Violations returns every problem found, capped for reporting.
func (l LogIntegrity) Violations(max int) []string {
	out := make([]string, 0, max)
	for _, group := range [][]string{l.Duplicates, l.Gaps, l.BadStarts, l.OutOfOrder} {
		for _, v := range group {
			if len(out) >= max {
				return out
			}
			out = append(out, v)
		}
	}
	return out
}

// CheckLogIntegrity verifies that every run's seq column is 1, 2, 3, ... with
// no gaps, no repeats and no backwards steps.
func CheckLogIntegrity(events []EventRow) LogIntegrity {
	var l LogIntegrity
	l.Events = len(events)
	for _, run := range GroupByRun(events) {
		l.Runs++
		seen := make(map[int64]bool, len(run.Events))
		var prev int64
		for i, ev := range run.Events {
			switch {
			case seen[ev.Seq]:
				l.Duplicates = append(l.Duplicates,
					fmt.Sprintf("%s: seq %d appears twice", run.Key, ev.Seq))
			case i == 0 && ev.Seq != 1:
				l.BadStarts = append(l.BadStarts,
					fmt.Sprintf("%s: history starts at seq %d, not 1", run.Key, ev.Seq))
			case i > 0 && ev.Seq < prev:
				l.OutOfOrder = append(l.OutOfOrder,
					fmt.Sprintf("%s: seq %d follows seq %d", run.Key, ev.Seq, prev))
			case i > 0 && ev.Seq != prev+1:
				l.Gaps = append(l.Gaps,
					fmt.Sprintf("%s: seq jumps %d -> %d", run.Key, prev, ev.Seq))
			}
			seen[ev.Seq] = true
			prev = ev.Seq
		}
	}
	return l
}

// ---- FR-2, FR-8, NFR-3: no duplicated side effect --------------------------

// kPayload is the one field every activity-shaped payload shares: the
// program-order index that joins a live execution to its replay.
type kPayload struct {
	K int `json:"k"`
}

// DupCompletion is one workflow step that completed more than once.
type DupCompletion struct {
	Run    RunKey
	K      int
	Count  int
	Seqs   []int64
	Types  []string
	Detail string
}

// FindDuplicateCompletions is the real duplicate-side-effect check.
//
// The obvious check — a duplicated (shard_id, workflow_id, run_id, seq) — can
// never fail, because that tuple is the events table's primary key, so a tool
// that reports it as passing has measured PostgreSQL's uniqueness constraint
// and nothing about this engine. The claim that actually needs evidence is one
// level up: a *step* must not complete twice. Two ActivityTaskCompleted events
// at the same program-order index k are two successful runs of the same
// activity, at different seqs, entirely legal as far as the schema is
// concerned, and exactly the duplicate side effect FR-2, FR-8 and NFR-3 all
// promise cannot happen.
//
// Compensation completions share the same k space and are counted with them,
// because a compensation that ran twice is a refund issued twice.
func FindDuplicateCompletions(events []EventRow) []DupCompletion {
	type occurrence struct {
		seqs  []int64
		types []string
	}
	var out []DupCompletion
	for _, run := range GroupByRun(events) {
		byK := make(map[int]*occurrence)
		order := make([]int, 0, 8)
		for _, ev := range run.Events {
			if ev.Type != evActivityCompleted && ev.Type != evCompCompleted {
				continue
			}
			var p kPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				// An undecodable completion payload is itself a finding: the
				// step cannot be shown to be unique, so it is reported rather
				// than skipped.
				out = append(out, DupCompletion{
					Run: run.Key, K: -1, Count: 1, Seqs: []int64{ev.Seq},
					Types:  []string{ev.Type},
					Detail: fmt.Sprintf("%s: completion at seq %d has an unreadable payload", run.Key, ev.Seq),
				})
				continue
			}
			o := byK[p.K]
			if o == nil {
				o = &occurrence{}
				byK[p.K] = o
				order = append(order, p.K)
			}
			o.seqs = append(o.seqs, ev.Seq)
			o.types = append(o.types, ev.Type)
		}
		sort.Ints(order)
		for _, k := range order {
			o := byK[k]
			if len(o.seqs) < 2 {
				continue
			}
			out = append(out, DupCompletion{
				Run: run.Key, K: k, Count: len(o.seqs), Seqs: o.seqs, Types: o.types,
				Detail: fmt.Sprintf("%s: step k=%d completed %d times (seqs %v, %v)",
					run.Key, k, len(o.seqs), o.seqs, o.types),
			})
		}
	}
	return out
}

// ---- FR-8: fencing tokens never decrease -----------------------------------

// TokenViolation is a shard whose fencing token went backwards.
type TokenViolation struct {
	ShardID int32
	Index   int
	From    int64
	To      int64
}

func (v TokenViolation) String() string {
	return fmt.Sprintf("shard %d: fencing token fell from %d to %d at sample %d",
		v.ShardID, v.From, v.To, v.Index)
}

// CheckTokenMonotonic reports any shard whose fencing token decreased across a
// series of observations.
//
// A fencing token is the engine's entire safety argument: it increases on
// every ownership change, and a write carrying a stale one is refused inside
// the transaction. If the number ever goes down, an old owner's token could
// become valid again and a fenced-out node could write, so this is the one
// invariant that cannot be allowed to bend. Series are per shard, in
// observation order.
func CheckTokenMonotonic(series map[int32][]int64) []TokenViolation {
	shards := make([]int32, 0, len(series))
	for s := range series {
		shards = append(shards, s)
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i] < shards[j] })

	var out []TokenViolation
	for _, shard := range shards {
		vals := series[shard]
		for i := 1; i < len(vals); i++ {
			if vals[i] < vals[i-1] {
				out = append(out, TokenViolation{
					ShardID: shard, Index: i, From: vals[i-1], To: vals[i],
				})
			}
		}
	}
	return out
}

// HandoverEvidence summarises how much ownership movement the cluster has
// actually seen, from the current fencing tokens.
//
// The token counts acquisitions: it starts at 0 and is bumped once per
// Acquire. So token-1 per shard is a lower bound on the number of handovers
// that shard has lived through, which is the closest this tool can get to "20
// induced failovers" without a record of every past lease.
type HandoverEvidence struct {
	Shards      int
	MaxToken    int64
	Handovers   int64
	OwnerCounts map[string]int
}

func SummariseLeases(leases []LeaseRow) HandoverEvidence {
	h := HandoverEvidence{OwnerCounts: map[string]int{}}
	for _, l := range leases {
		h.Shards++
		if l.Token > h.MaxToken {
			h.MaxToken = l.Token
		}
		if l.Token > 1 {
			h.Handovers += l.Token - 1
		}
		if l.Owner != "" {
			h.OwnerCounts[l.Owner]++
		}
	}
	return h
}

// ---- FR-3: retry with backoff, and a non-retryable class -------------------

// RetryReport describes what the log says about retries.
type RetryReport struct {
	// StepsWithFailures is the number of (run, k) steps that failed at least
	// once. StepsRetriedToSuccess is how many of those then succeeded.
	StepsWithFailures     int
	StepsRetriedToSuccess int
	// MostFailuresThenOneSuccess is the largest number of journaled failures
	// seen on a step that then succeeded exactly once — the number FR-3's
	// "fail 3x, succeed on 4th" is compared against.
	MostFailuresThenOneSuccess int
	// MaxAttempts is the highest attempt number seen on a failure event, so
	// the report can say "failed 3 times then succeeded" with a real number.
	MaxAttempts int
	// NonRetryable counts failures marked non-retryable. A non-retryable
	// failure that was retried anyway is a violation of the error class.
	NonRetryable        int
	NonRetryableRetried []string
	// MultipleSuccesses is the "log shows one success" half of the acceptance
	// condition: a step that failed and then succeeded twice.
	MultipleSuccesses []string
	Example           string
}

type activityFailedPayload struct {
	K            int    `json:"k"`
	Error        string `json:"error"`
	Attempt      int    `json:"attempt"`
	NonRetryable bool   `json:"non_retryable"`
}

// AnalyseRetries walks each run and, per program-order index k, counts the
// failures and successes recorded against it.
func AnalyseRetries(events []EventRow) RetryReport {
	var r RetryReport
	for _, run := range GroupByRun(events) {
		type step struct {
			failures     int
			successes    int
			maxAttempt   int
			nonRetryable bool
			// laterAttempt records that something was tried again after a
			// failure marked non-retryable.
			retriedAfterNonRetryable bool
		}
		steps := make(map[int]*step)
		order := make([]int, 0, 8)
		get := func(k int) *step {
			s := steps[k]
			if s == nil {
				s = &step{}
				steps[k] = s
				order = append(order, k)
			}
			return s
		}

		for _, ev := range run.Events {
			switch ev.Type {
			case evActivityFailed, evCompFailed:
				var p activityFailedPayload
				if json.Unmarshal(ev.Payload, &p) != nil {
					continue
				}
				s := get(p.K)
				s.failures++
				if p.Attempt > s.maxAttempt {
					s.maxAttempt = p.Attempt
				}
				if p.NonRetryable {
					s.nonRetryable = true
				} else if s.nonRetryable {
					s.retriedAfterNonRetryable = true
				}
			case evActivityCompleted, evCompCompleted:
				var p kPayload
				if json.Unmarshal(ev.Payload, &p) != nil {
					continue
				}
				s := get(p.K)
				s.successes++
				if s.nonRetryable {
					s.retriedAfterNonRetryable = true
				}
			}
		}

		sort.Ints(order)
		for _, k := range order {
			s := steps[k]
			if s.failures == 0 {
				continue
			}
			r.StepsWithFailures++
			if s.maxAttempt > r.MaxAttempts {
				r.MaxAttempts = s.maxAttempt
			}
			if s.nonRetryable {
				r.NonRetryable++
			}
			if s.retriedAfterNonRetryable {
				r.NonRetryableRetried = append(r.NonRetryableRetried,
					fmt.Sprintf("%s: step k=%d was retried after a non-retryable failure", run.Key, k))
			}
			if s.successes == 1 {
				r.StepsRetriedToSuccess++
				if s.failures > r.MostFailuresThenOneSuccess {
					r.MostFailuresThenOneSuccess = s.failures
					r.Example = fmt.Sprintf("%s k=%d: %d failures (max attempt %d) then exactly 1 success",
						run.Key, k, s.failures, s.maxAttempt)
				}
				if r.Example == "" {
					r.Example = fmt.Sprintf("%s k=%d: %d failures (max attempt %d) then exactly 1 success",
						run.Key, k, s.failures, s.maxAttempt)
				}
			}
			if s.successes > 1 {
				r.MultipleSuccesses = append(r.MultipleSuccesses,
					fmt.Sprintf("%s: step k=%d failed %d times and then succeeded %d times",
						run.Key, k, s.failures, s.successes))
			}
		}
	}
	return r
}

// ---- FR-10: compensation runs last-in-first-out ----------------------------

type activityScheduledPayload struct {
	K            int    `json:"k"`
	ActivityType string `json:"activity_type"`
	DedupKey     string `json:"dedup_key"`
	Mode         string `json:"mode"`
}

// LIFOReport describes the compensation ordering found in history.
type LIFOReport struct {
	// RunsWithCompensation is how many runs rolled anything back at all.
	RunsWithCompensation int
	// RunsChecked is how many of those had every compensation resolvable to
	// the forward step it undoes, so an order could actually be judged.
	RunsChecked int
	RunsLIFO    int
	Violations  []string
	// Unmapped names compensation activity types absent from the mapping.
	// They are reported, never assumed correct.
	Unmapped []string
	Example  string
}

// CheckCompensationLIFO reads compensation order straight out of history.
//
// The log records which compensations ran and in what order, but not which
// forward step each one undoes — that pairing lives in the workflow's source.
// compMap supplies it. With the pairing, LIFO has an exact meaning that can be
// tested: walking the compensations in the order they were scheduled, the
// program-order index k of the forward step each one undoes must strictly
// decrease. Anything else is unwinding out of order.
func CheckCompensationLIFO(events []EventRow, compMap map[string]string) LIFOReport {
	var r LIFOReport
	for _, run := range GroupByRun(events) {
		forwardK := make(map[string]int)  // forward activity type -> its k
		var compensations []string        // compensation activity types, in scheduled order
		var compKs []int                  // their own k, for the message
		unmapped := map[string]struct{}{} // types with no forward step named

		for _, ev := range run.Events {
			if ev.Type != evActivityScheduled && ev.Type != evCompScheduled {
				continue
			}
			var p activityScheduledPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			// A compensation is recognised by its activity type, not only by
			// the CompensationScheduled event type. The delivery saga runs
			// each undo step through ordinary ExecuteActivity, so on the
			// running engine a refund is journaled as ActivityTaskScheduled
			// and CompensationScheduled never appears at all. Keying on the
			// event type alone would find zero compensated runs forever and
			// report BLOCKED on a cluster that had rolled back plenty.
			_, isUndo := compMap[p.ActivityType]
			switch {
			case ev.Type == evActivityScheduled && !isUndo:
				// First scheduling wins: a retried step keeps its original k.
				if _, seen := forwardK[p.ActivityType]; !seen {
					forwardK[p.ActivityType] = p.K
				}
			default:
				if len(compensations) > 0 && compensations[len(compensations)-1] == p.ActivityType &&
					compKs[len(compKs)-1] == p.K {
					// The same compensation re-scheduled at the same k is a
					// retry of one rollback step, not a second rollback.
					continue
				}
				compensations = append(compensations, p.ActivityType)
				compKs = append(compKs, p.K)
			}
		}

		if len(compensations) == 0 {
			continue
		}
		r.RunsWithCompensation++

		// Resolve each compensation to the k of the step it undoes.
		resolved := make([]int, 0, len(compensations))
		ok := true
		for _, c := range compensations {
			forward, named := compMap[c]
			if !named {
				unmapped[c] = struct{}{}
				ok = false
				continue
			}
			k, ran := forwardK[forward]
			if !ran {
				// A compensation for a step that never ran forward is a
				// stronger finding than an ordering problem: it means the
				// rollback is undoing something that never happened.
				r.Violations = append(r.Violations, fmt.Sprintf(
					"%s: compensation %q undoes %q, which has no scheduling event in this run",
					run.Key, c, forward))
				ok = false
				continue
			}
			resolved = append(resolved, k)
		}
		for c := range unmapped {
			r.Unmapped = appendUnique(r.Unmapped, c)
		}
		if !ok || len(resolved) < 1 {
			continue
		}

		r.RunsChecked++
		lifo := true
		for i := 1; i < len(resolved); i++ {
			if resolved[i] >= resolved[i-1] {
				lifo = false
				r.Violations = append(r.Violations, fmt.Sprintf(
					"%s: compensation %q (undoes step k=%d) ran after %q (undoes step k=%d) — not reverse order",
					run.Key, compensations[i], resolved[i], compensations[i-1], resolved[i-1]))
			}
		}
		if lifo {
			r.RunsLIFO++
			if r.Example == "" {
				r.Example = fmt.Sprintf("%s: %v (undoing steps k=%v, strictly decreasing)",
					run.Key, compensations, resolved)
			}
		}
	}
	return r
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// ---- FR-13 and FR-14: the semantic verdict journal -------------------------

type dedupPayload struct {
	K          int     `json:"k"`
	Verdict    string  `json:"verdict"`
	MatchedK   int     `json:"matched_k"`
	Similarity float64 `json:"similarity"`
	TauHigh    float64 `json:"tau_high"`
	TauLow     float64 `json:"tau_low"`
	Model      string  `json:"model"`
	IntentText string  `json:"intent_text"`
}

// VerdictReport is what the log says about semantic deduplication.
type VerdictReport struct {
	Total int
	// Incomplete lists verdicts missing a field FR-14 requires, which is the
	// operative test of "explainable from the log alone": if the score, the
	// thresholds, the matched call or the model name is absent, nobody can
	// reconstruct why an action was skipped.
	Incomplete []string
	ByVerdict  map[string]int
	Models     map[string]int

	// Duplicates is how many DUPLICATE verdicts were journaled, and Blocked is
	// how many of those were followed by no scheduling of that step — the
	// action really was suppressed. Executed is the opposite and is a failure
	// of FR-13.
	Duplicates int
	Blocked    int
	Executed   []string
	// NotReusable counts DUPLICATE verdicts whose matched call had no
	// journaled result yet. The engine deliberately runs the action in that
	// case (internal/runtime/dedup.go: "refusing to skip is the safe
	// direction"), so such a run is by design and is not an FR-13 violation.
	NotReusable int
	Example     string
}

// verdictFields are the keys FR-14 needs present in every journaled verdict:
// the score, both thresholds, the matched call and the model. Presence is
// checked on the raw JSON rather than on a decoded struct because a missing
// numeric field decodes as 0, and 0 is a legitimate score and a legitimate
// program-order index — a struct cannot tell "absent" from "zero".
var verdictFields = []string{"verdict", "similarity", "tau_high", "tau_low", "matched_k", "model"}

// AnalyseVerdicts checks each journaled verdict for completeness, and each
// DUPLICATE verdict for whether the action it condemned actually got skipped.
func AnalyseVerdicts(events []EventRow) VerdictReport {
	r := VerdictReport{ByVerdict: map[string]int{}, Models: map[string]int{}}
	for _, run := range GroupByRun(events) {
		scheduledK := make(map[int]bool)
		completedAt := make(map[int]int64) // k -> seq of its first completion
		for _, ev := range run.Events {
			switch ev.Type {
			case evActivityScheduled, evCompScheduled:
				var p activityScheduledPayload
				if json.Unmarshal(ev.Payload, &p) == nil {
					scheduledK[p.K] = true
				}
			case evActivityCompleted, evCompCompleted:
				var p kPayload
				if json.Unmarshal(ev.Payload, &p) == nil {
					if _, seen := completedAt[p.K]; !seen {
						completedAt[p.K] = ev.Seq
					}
				}
			}
		}

		for _, ev := range run.Events {
			if ev.Type != evDedupEvaluated {
				continue
			}
			r.Total++
			var p dedupPayload
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				r.Incomplete = append(r.Incomplete,
					fmt.Sprintf("%s seq %d: verdict payload is unreadable", run.Key, ev.Seq))
				continue
			}
			r.ByVerdict[p.Verdict]++
			if p.Model != "" {
				r.Models[p.Model]++
			}

			var missing []string
			var raw map[string]json.RawMessage
			_ = json.Unmarshal(ev.Payload, &raw)
			for _, f := range verdictFields {
				if _, ok := raw[f]; !ok {
					missing = append(missing, f+" (absent)")
				}
			}
			if p.Verdict == "" {
				missing = append(missing, "verdict")
			}
			if p.Model == "" {
				missing = append(missing, "model")
			}
			if p.TauHigh <= 0 || p.TauLow <= 0 {
				missing = append(missing, "thresholds")
			}
			// A DISTINCT verdict legitimately has no match, so matched_k is
			// only required where a match was claimed.
			if p.Verdict == "DUPLICATE" && p.MatchedK < 0 {
				missing = append(missing, "matched_k")
			}
			if p.Verdict == "DUPLICATE" && p.Similarity < p.TauHigh {
				missing = append(missing, fmt.Sprintf("a score consistent with the verdict (%.4f < tau_high %.4f)",
					p.Similarity, p.TauHigh))
			}
			if len(missing) > 0 {
				r.Incomplete = append(r.Incomplete, fmt.Sprintf(
					"%s seq %d (k=%d, %s): missing %v", run.Key, ev.Seq, p.K, p.Verdict, missing))
			}

			if p.Verdict == "DUPLICATE" {
				r.Duplicates++
				matchedSeq, matchedDone := completedAt[p.MatchedK]
				switch {
				case scheduledK[p.K] && (!matchedDone || matchedSeq > ev.Seq):
					r.NotReusable++
				case scheduledK[p.K]:
					r.Executed = append(r.Executed, fmt.Sprintf(
						"%s: k=%d was judged a DUPLICATE of k=%d (score %.4f) but was scheduled anyway",
						run.Key, p.K, p.MatchedK, p.Similarity))
				default:
					r.Blocked++
					if r.Example == "" {
						r.Example = fmt.Sprintf(
							"%s: k=%d suppressed as a duplicate of k=%d, score %.4f vs tau_high %.4f, model %q",
							run.Key, p.K, p.MatchedK, p.Similarity, p.TauHigh, p.Model)
					}
				}
			}
		}
	}
	return r
}

// ---- FR-12: a stable deduplication key on every call -----------------------

// DedupKeyReport counts scheduled activities carrying a deduplication key.
type DedupKeyReport struct {
	Scheduled int
	WithKey   int
	Missing   []string
	// Reused is one key appearing at two different program-order indices in
	// the same run. It is reported rather than failed: a legitimate design
	// could derive the key from the business operation rather than the step.
	Reused  []string
	Example string
}

// AnalyseDedupKeys checks that every scheduled activity carries a key.
//
// "Identical across every crash and replay" cannot be observed by reading the
// log once, because the log holds one copy of each scheduling event: it is
// never rewritten, so the key on disk is by construction the key every replay
// sees. What this tool can check, and does, is the falsifiable half — that a
// key was forwarded at all, on every single external call.
func AnalyseDedupKeys(events []EventRow) DedupKeyReport {
	var r DedupKeyReport
	for _, run := range GroupByRun(events) {
		byKey := make(map[string]int)
		for _, ev := range run.Events {
			if ev.Type != evActivityScheduled && ev.Type != evCompScheduled {
				continue
			}
			var p activityScheduledPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			r.Scheduled++
			if p.DedupKey == "" {
				r.Missing = append(r.Missing, fmt.Sprintf(
					"%s seq %d: %s scheduled at k=%d with an empty dedup_key",
					run.Key, ev.Seq, p.ActivityType, p.K))
				continue
			}
			r.WithKey++
			if prev, seen := byKey[p.DedupKey]; seen && prev != p.K {
				r.Reused = append(r.Reused, fmt.Sprintf(
					"%s: dedup key %q used at k=%d and k=%d", run.Key, p.DedupKey, prev, p.K))
			}
			byKey[p.DedupKey] = p.K
			if r.Example == "" {
				r.Example = fmt.Sprintf("%s k=%d %s -> %q", run.Key, p.K, p.ActivityType, p.DedupKey)
			}
		}
	}
	return r
}

// ---- FR-4: a durable timer actually fired ----------------------------------

type timerStartedPayload struct {
	K        int   `json:"k"`
	Duration int64 `json:"duration_ns"`
}

// TimerObservation is one sleep read out of a run's history.
type TimerObservation struct {
	Run       RunKey
	K         int
	Requested int64 // nanoseconds, as journaled
	Elapsed   int64 // nanoseconds between TimerStarted and TimerFired
	Fired     bool
}

// AnalyseTimers pairs each TimerStarted with its TimerFired.
func AnalyseTimers(events []EventRow) []TimerObservation {
	var out []TimerObservation
	for _, run := range GroupByRun(events) {
		type started struct {
			at        int64
			requested int64
		}
		starts := make(map[int]started)
		order := make([]int, 0, 4)
		fired := make(map[int]int64)

		for _, ev := range run.Events {
			switch ev.Type {
			case evTimerStarted:
				var p timerStartedPayload
				if json.Unmarshal(ev.Payload, &p) != nil {
					continue
				}
				if _, seen := starts[p.K]; !seen {
					order = append(order, p.K)
				}
				starts[p.K] = started{at: ev.CreatedAt.UnixNano(), requested: p.Duration}
			case evTimerFired:
				var p kPayload
				if json.Unmarshal(ev.Payload, &p) != nil {
					continue
				}
				fired[p.K] = ev.CreatedAt.UnixNano()
			}
		}
		sort.Ints(order)
		for _, k := range order {
			s := starts[k]
			obs := TimerObservation{Run: run.Key, K: k, Requested: s.requested}
			if at, ok := fired[k]; ok {
				obs.Fired = true
				obs.Elapsed = at - s.at
			}
			out = append(out, obs)
		}
	}
	return out
}

// ---- helpers used by several checks ----------------------------------------

// RunLengths returns the event count of the longest run, for NFR-6, which
// states its target at a specific history length.
func RunLengths(events []EventRow) (longest int, runsOver200 int) {
	for _, run := range GroupByRun(events) {
		n := len(run.Events)
		if n > longest {
			longest = n
		}
		if n >= 200 {
			runsOver200++
		}
	}
	return longest, runsOver200
}

// CountSignals reports how many runs took at least one signal.
func CountSignals(events []EventRow) (runs, total int) {
	for _, run := range GroupByRun(events) {
		n := 0
		for _, ev := range run.Events {
			if ev.Type == evSignalReceived {
				n++
			}
		}
		if n > 0 {
			runs++
			total += n
		}
	}
	return runs, total
}

// TerminalCensus counts executions by status and by workflow type.
func TerminalCensus(execs []ExecRow) (byStatus, byType map[string]int) {
	byStatus = map[string]int{}
	byType = map[string]int{}
	for _, e := range execs {
		byStatus[e.Status]++
		byType[e.WorkflowType]++
	}
	return byStatus, byType
}

// sortedKeys renders a census deterministically for a report line.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func censusString(m map[string]int) string {
	keys := sortedKeys(m)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s=%d", k, m[k])
	}
	if s == "" {
		return "none"
	}
	return s
}
