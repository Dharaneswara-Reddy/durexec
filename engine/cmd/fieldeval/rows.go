package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// THE PER-ROW CSV IS THE RUN
// ---------------------------------------------------------------------------
//
// Every figure in the report is recomputed from this file, and the file is
// written one row at a time and flushed immediately. Three properties follow,
// and all three are requirements of this project rather than conveniences:
//
//	A RUN KILLED PART WAY LEAVES USABLE ROWS. The corpora are interleaved by
//	domain and by label, so a prefix is a representative sample rather than
//	the first domain only.
//
//	A RUN RESTARTED SKIPS WHAT IT HAS. The resume key is
//	comparator|corpus|pair, read back from the file at startup. Re-running the
//	identical command continues; it does not duplicate and does not start over.
//
//	THE REPORT CANNOT DISAGREE WITH THE DATA. Summaries are never accumulated
//	in memory while measuring. They are computed by reading this file back,
//	which is also what -report-only does, so a partial run yields a valid
//	partial report with no separate code path to be wrong.

// row is one comparator's answer about one pair: every column a reader needs
// to check a figure by hand, and nothing derived.
//
// Nothing here is a verdict at a threshold. Verdicts are derived at scoring
// time from Similarity, Gated and the arm's thresholds, because that is what
// lets one pass over the corpus produce a whole threshold sweep and both
// escalation policies. CheckerVerdict is the engine's own answer at the
// shipped thresholds, kept so that the derivation can be CHECKED against the
// engine rather than assumed equal to it.
type row struct {
	Corpus   string
	Domain   string
	Activity string
	PairID   string
	Category string
	Hard     bool
	Truth    bool

	Comparator    string
	Extractor     string
	GateMode      string
	MinAgreeing   int
	CasePreserved bool

	// GateDecides records that this arm let the gate's answer stand and
	// allowed a dissimilar remainder only to escalate. It is a column
	// because the verdict derivation needs it, and a verdict that could not
	// be re-derived from the row would make the whole sweep unverifiable.
	GateDecides bool

	// Render is the intent rendering this row was measured under: "sentence"
	// (what the engine does for the agent's tool calls) or "envelope" (the
	// checker's generic fallback, and what the published semantic-experiment
	// files used). It is a column so that rows measured under two renderings
	// can never be averaged together by accident.
	Render string

	// ExactHit is the byte-comparison baseline's answer. It is separate from
	// Similarity because the exact baseline measures no similarity at all,
	// and a 1.0 in that column would read as one.
	ExactHit bool

	RecognisedA bool
	RecognisedB bool
	Gated       bool
	GateField   string

	AgreeingFields int

	// Similarity is what the checker reported: the cosine over the whole
	// intent on the pure path, over the free text on the hybrid path, and
	// zero when the gate answered without measuring anything.
	Similarity float64

	// SimilarityFree is the free-text cosine taken from Compare, which is
	// meaningful even on a pair the gate rejected — so the similarity-only
	// ablation can score the same pairs without the gate's veto.
	SimilarityFree float64

	ModelUsed bool
	Fallback  bool

	CheckerVerdict string

	SignatureA string
	SignatureB string
	RemainderA string
	RemainderB string

	LatencyUS float64
	Detail    string
	Note      string
}

// rowHeader is the CSV header. The order is fixed forever: a reader with an
// older file must be able to append to it.
var rowHeader = []string{
	"corpus", "domain", "activity", "pair_id", "category", "hard", "truth",
	"comparator", "extractor", "gate_mode", "min_agreeing", "case_preserved", "gate_decides", "render",
	"exact_hit", "recognised_a", "recognised_b", "gated", "gate_field",
	"agreeing_fields", "similarity", "similarity_free_text", "model_used",
	"fallback", "checker_verdict", "signature_a", "signature_b",
	"remainder_a", "remainder_b", "latency_us", "detail", "note",
}

func (r row) fields() []string {
	return []string{
		r.Corpus, r.Domain, r.Activity, r.PairID, r.Category,
		boolStr(r.Hard), truthStr(r.Truth),
		r.Comparator, r.Extractor, r.GateMode, strconv.Itoa(r.MinAgreeing),
		boolStr(r.CasePreserved), boolStr(r.GateDecides), r.Render,
		boolStr(r.ExactHit), boolStr(r.RecognisedA), boolStr(r.RecognisedB),
		boolStr(r.Gated), r.GateField,
		strconv.Itoa(r.AgreeingFields),
		strconv.FormatFloat(r.Similarity, 'f', 6, 64),
		strconv.FormatFloat(r.SimilarityFree, 'f', 6, 64),
		boolStr(r.ModelUsed), boolStr(r.Fallback), r.CheckerVerdict,
		r.SignatureA, r.SignatureB, r.RemainderA, r.RemainderB,
		strconv.FormatFloat(r.LatencyUS, 'f', 1, 64),
		clip(r.Detail, 400), r.Note,
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// truthStr writes the label as a word rather than a boolean. "DUPLICATE" in a
// spreadsheet cannot be mistaken for anything else; "true" can.
func truthStr(b bool) string {
	if b {
		return "DUPLICATE"
	}
	return "DISTINCT"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// rowKey is the resume key.
func rowKey(comparator, corpus, pairID string) string {
	return comparator + "|" + corpus + "|" + pairID
}

func (r row) key() string { return rowKey(r.Comparator, r.Corpus, r.PairID) }

// ---------------------------------------------------------------------------
// Writing.
// ---------------------------------------------------------------------------

// rowWriter appends to the CSV and flushes after every row.
//
// Flushing per row costs a syscall per row and buys the only thing that
// matters here: that the run's value does not depend on the run finishing.
type rowWriter struct {
	f   *os.File
	w   *csv.Writer
	n   int
	bad bool
}

func openRowWriter(path string) (*rowWriter, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// Does it already have a header? An existing file is appended to, which
	// is what makes a restart continue rather than overwrite. Nothing here
	// ever truncates a result file.
	needHeader := true
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		needHeader = false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)
	if needHeader {
		if err := w.Write(rowHeader); err != nil {
			f.Close()
			return nil, err
		}
		w.Flush()
		if err := w.Error(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &rowWriter{f: f, w: w}, nil
}

func (rw *rowWriter) Write(r row) error {
	if err := rw.w.Write(r.fields()); err != nil {
		return err
	}
	rw.w.Flush()
	if err := rw.w.Error(); err != nil {
		return err
	}
	// fsync is deliberately NOT called. A flush puts the row in the operating
	// system's hands, which survives this process being killed — the failure
	// this file is defending against. Surviving a power cut is not worth a
	// disk sync per row.
	rw.n++
	return nil
}

func (rw *rowWriter) Close() error {
	if rw.f == nil {
		return nil
	}
	rw.w.Flush()
	err := rw.f.Close()
	rw.f = nil
	return err
}

// alreadyMeasured reads the resume keys out of an existing CSV.
//
// A row whose note begins with ERROR is NOT counted as measured: an error row
// records that the attempt happened and failed, and a restart should try it
// again rather than inherit the failure forever.
func alreadyMeasured(path string) (map[string]bool, error) {
	done := map[string]bool{}
	rows, err := readRows(path)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Note, "ERROR") {
			continue
		}
		done[r.key()] = true
	}
	return done, nil
}

// readRows reads the whole CSV back. A missing file is not an error: it is a
// run that has not started.
func readRows(path string) ([]row, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"corpus", "pair_id", "comparator", "truth", "similarity"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("%s has no %q column; it was not written by this harness", path, need)
		}
	}

	var out []row
	line := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s line %d: %w", path, line, err)
		}
		line++
		get := func(name string) string {
			i, ok := col[name]
			if !ok || i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		num := func(name string) float64 {
			v, _ := strconv.ParseFloat(get(name), 64)
			return v
		}
		out = append(out, row{
			Corpus:         get("corpus"),
			Domain:         get("domain"),
			Activity:       get("activity"),
			PairID:         get("pair_id"),
			Category:       get("category"),
			Hard:           get("hard") == "true",
			Truth:          get("truth") == "DUPLICATE",
			Comparator:     get("comparator"),
			Extractor:      get("extractor"),
			GateMode:       get("gate_mode"),
			MinAgreeing:    int(num("min_agreeing")),
			CasePreserved:  get("case_preserved") == "true",
			GateDecides:    get("gate_decides") == "true",
			Render:         get("render"),
			ExactHit:       get("exact_hit") == "true",
			RecognisedA:    get("recognised_a") == "true",
			RecognisedB:    get("recognised_b") == "true",
			Gated:          get("gated") == "true",
			GateField:      get("gate_field"),
			AgreeingFields: int(num("agreeing_fields")),
			Similarity:     num("similarity"),
			SimilarityFree: num("similarity_free_text"),
			ModelUsed:      get("model_used") == "true",
			Fallback:       get("fallback") == "true",
			CheckerVerdict: get("checker_verdict"),
			SignatureA:     get("signature_a"),
			SignatureB:     get("signature_b"),
			RemainderA:     get("remainder_a"),
			RemainderB:     get("remainder_b"),
			LatencyUS:      num("latency_us"),
			Detail:         get("detail"),
			Note:           get("note"),
		})
	}
	return out, nil
}
