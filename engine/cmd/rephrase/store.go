package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// THE CSV IS THE RUN'S MEMORY, NOT A REPORT OF IT
// ---------------------------------------------------------------------------
//
// Every trial is written to disk and flushed the moment it finishes, before
// the next one starts. Three things follow from that, and all three are the
// point:
//
//  1. A RUN KILLED AT TRIAL 37 LEAVES 37 USABLE ROWS. Each trial costs real
//     model calls against a quota, and this harness can run for twenty
//     minutes. Buffering until the end would mean that any interruption —
//     a terminal closing, a laptop sleeping, a session ending — threw away
//     everything that had been paid for.
//
//  2. THE RUN IS RESUMABLE. On start, whatever is already in the file is read
//     back and those trials are not run again. The key is the arm and the
//     trial number, both of which are deterministic, so resuming lands on
//     exactly the trials that are missing.
//
//  3. THE REPORT IS COMPUTED FROM THE FILE, never from memory. A partial file
//     therefore produces a valid partial report, and -report-only regenerates
//     the whole report from the CSV with no model and no encoder involved. A
//     reader who disagrees with a classification can recompute every figure.
//
// The schema is fixed in one place, used by both the writer and the reader, so
// a column added to one cannot silently shift the other.

var csvColumns = []string{
	"trial_key", "index", "arm", "domain", "goal_id", "goal_text", "commit_tool",
	"outcome", "error",
	"before_tool", "before_args", "after_tool", "after_args",
	"after_tool_call_count", "side_effects",
	"byte_identical", "canonical_equal", "exact_key_equal",
	"cosine_similarity", "semantic_verdict",
	"hybrid_verdict", "hybrid_gated", "hybrid_fell_back",
	"hybrid_differing_field", "hybrid_mine_value", "hybrid_theirs_value",
	"hybrid_similarity", "hybrid_model_used",
	"drifted",
	"before_fields", "after_fields",
	"before_contradictions", "after_contradictions",
	"before_missing", "after_missing",
}

// trialStore appends rows to a CSV and flushes each one.
type trialStore struct {
	path string
	f    *os.File
	w    *csv.Writer
}

// openStore opens the CSV for appending, writing the header if the file is new
// or empty.
//
// It appends rather than truncating because truncating is how a resumed run
// would destroy the rows it was meant to keep.
func openStore(path string) (*trialStore, error) {
	existing, err := os.Stat(path)
	fresh := errors.Is(err, os.ErrNotExist) || (err == nil && existing.Size() == 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s := &trialStore{path: path, f: f, w: csv.NewWriter(f)}
	if fresh {
		if err := s.w.Write(csvColumns); err != nil {
			f.Close()
			return nil, err
		}
		s.w.Flush()
		if err := s.w.Error(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *trialStore) Close() error { s.w.Flush(); return s.f.Close() }

// Append writes one trial and flushes, then fsyncs.
//
// The fsync is not superstition: the whole reason this file exists is to
// survive the process not getting to run its deferred Close, and a row sitting
// in the kernel's page cache when the machine stops is a row that was never
// written.
func (s *trialStore) Append(t trial) error {
	if err := s.w.Write(t.row()); err != nil {
		return err
	}
	s.w.Flush()
	if err := s.w.Error(); err != nil {
		return err
	}
	return s.f.Sync()
}

func (t trial) row() []string {
	return []string{
		t.Key, strconv.Itoa(t.Index), t.Arm, t.Domain, t.Goal.ID, t.Goal.Text, t.CommitTool,
		string(t.Outcome), t.Err,
		t.BeforeTool, t.Before, t.AfterTool, t.After,
		strconv.Itoa(t.AfterCallCount), strconv.Itoa(t.SideEffects),
		b(t.ByteIdentical), b(t.CanonicalEqual), b(t.ExactKeyEqual),
		fmt.Sprintf("%.6f", t.Similarity), string(t.SemVerdict),
		string(t.HybridVerdict), b(t.HybridGated), b(t.HybridFellBack),
		t.HybridField, t.HybridMineValue, t.HybridTheirsValue,
		fmt.Sprintf("%.6f", t.HybridSimilarity), b(t.HybridModelUsed),
		b(t.Drifted),
		t.BeforeFields, t.AfterFields,
		t.BeforeContradictions, t.AfterContradictions,
		t.BeforeMissing, t.AfterMissing,
	}
}

// readStore reads back every trial already recorded.
//
// A row this version cannot parse is an error rather than a skip. Silently
// dropping rows would make a resumed run quietly rerun trials that had already
// been paid for, and would make the report's denominators wrong in a way
// nothing would announce.
func readStore(path string) ([]trial, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, name := range header {
		idx[name] = i
	}
	for _, want := range csvColumns {
		if _, ok := idx[want]; !ok {
			return nil, fmt.Errorf("%s is missing the %q column; it was written by a different "+
				"version of this harness and cannot be resumed — move it aside", path, want)
		}
	}

	var out []trial
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		get := func(name string) string {
			i := idx[name]
			if i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		t := trial{
			Key:                  get("trial_key"),
			Index:                atoi(get("index")),
			Arm:                  get("arm"),
			Domain:               get("domain"),
			CommitTool:           get("commit_tool"),
			Outcome:              outcome(get("outcome")),
			Err:                  get("error"),
			BeforeTool:           get("before_tool"),
			Before:               get("before_args"),
			AfterTool:            get("after_tool"),
			After:                get("after_args"),
			AfterCallCount:       atoi(get("after_tool_call_count")),
			SideEffects:          atoi(get("side_effects")),
			ByteIdentical:        get("byte_identical") == "true",
			CanonicalEqual:       get("canonical_equal") == "true",
			ExactKeyEqual:        get("exact_key_equal") == "true",
			Similarity:           atof(get("cosine_similarity")),
			SemVerdict:           core.DedupVerdict(get("semantic_verdict")),
			HybridVerdict:        core.DedupVerdict(get("hybrid_verdict")),
			HybridGated:          get("hybrid_gated") == "true",
			HybridFellBack:       get("hybrid_fell_back") == "true",
			HybridField:          get("hybrid_differing_field"),
			HybridMineValue:      get("hybrid_mine_value"),
			HybridTheirsValue:    get("hybrid_theirs_value"),
			HybridSimilarity:     atof(get("hybrid_similarity")),
			HybridModelUsed:      get("hybrid_model_used") == "true",
			Drifted:              get("drifted") == "true",
			BeforeFields:         get("before_fields"),
			AfterFields:          get("after_fields"),
			BeforeContradictions: get("before_contradictions"),
			AfterContradictions:  get("after_contradictions"),
			BeforeMissing:        get("before_missing"),
			AfterMissing:         get("after_missing"),
		}
		// The goal is referred to by id in the file and resolved back to the
		// corpus here, so that a report regenerated from an old CSV uses the
		// goal text the run actually sent rather than today's corpus.
		t.Goal = goalSpec{ID: get("goal_id"), Domain: t.Domain, Text: get("goal_text")}
		out = append(out, t)
	}
	return out, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atof(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// b renders a boolean as a word rather than as 0/1, so a spreadsheet cannot
// quietly turn a verdict into a number and average it.
func b(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// notesFor is the companion file written next to the CSV: what the columns
// mean and the exact command that resumes the run. It exists because a CSV
// found on disk six months from now has to be self-explanatory, and because
// the resume command is the one thing a person picking up an interrupted run
// needs and cannot guess.
func notesFor(csvPath string, cmd string) string {
	return fmt.Sprintf(`Companion notes for %s
================================================================================

WHAT THIS FILE IS
  One row per crash-and-replan trial, written and flushed as each trial
  finished. It is the raw data behind docs/results/rephrase-live-model.txt, and
  every figure in that file is a count over these rows.

HOW TO RESUME AN INTERRUPTED RUN
  Rows already present are skipped, keyed by trial_key, so re-running the exact
  same command continues where it stopped:

    %s

HOW TO REGENERATE THE REPORT WITHOUT CALLING THE MODEL
  Add -report-only. It reads this CSV and recomputes every table from it. No
  API key and no embedding service are needed.

    go run ./cmd/rephrase -report-only -csv=%s -txt=<where to write the report>

THE COLUMNS
  trial_key              arm and trial number; the resume key
  arm                    lost-journal (the experiment) or journal-intact (the control)
  domain                 flight | payment | refund | meeting | message
  commit_tool            the tool whose repeat is the harm in this domain
  outcome                usable | never-booked | no-rebooking | failed
  before_args            the EXACT sentence the model used before the crash
  after_args             the EXACT sentence the model used after recovery
  after_tool_call_count  how many tool calls the recovered incarnation executed
  side_effects           how many times the commit tool really ran, both incarnations
  byte_identical         whether the two sentences are the same bytes
  canonical_equal        whether trimming and lowercasing would match them
  exact_key_equal        whether the engine's exact idempotency keys match
  cosine_similarity      real cosine from all-MiniLM-L6-v2 over the two sentences
  semantic_verdict       the shipped comparator's verdict at tau 0.85/0.90
  hybrid_verdict         the opt-in comparator's verdict at the same thresholds
  hybrid_gated           a deciding field differed, so no threshold could match them
  hybrid_fell_back       the field extractor could not read the intent at all, so
                         the verdict is the pure-semantic one
  hybrid_model_used      whether answering actually required an embedding
  drifted                whether either sentence contradicts the goal, i.e. the
                         model planned a DIFFERENT action rather than rewording one
  before_fields          every declared field of the goal, as read out of the
  after_fields           sentence: name=value(agrees|missing|contradicts)

WHAT NOT TO DO WITH IT
  Do not average the verdict columns. Do not compute a precision figure: this
  corpus has no designed negative class, and the drifted rows are whatever the
  model happened to get wrong rather than a set of near-misses built to test
  for false positives.
`, csvPath, cmd, csvPath)
}
