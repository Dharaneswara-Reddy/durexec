package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CSVHeader is the column order of the CSV file. It is exported as a variable
// so the test pins it: a consumer of the CSV breaks if it drifts.
var CSVHeader = []string{"id", "kind", "outcome", "requirement", "acceptance", "observed", "action", "evaluated"}

// WriteCSV writes one row per requirement and check, in report order.
// encoding/csv does the quoting, so a detail string containing a comma, a
// quote or a newline survives the round trip intact.
func WriteCSV(w io.Writer, rows []Requirement, rec *Recorder) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(CSVHeader); err != nil {
		return err
	}
	for _, req := range rows {
		res := rec.Get(req.ID)
		if err := cw.Write([]string{
			req.ID, req.Kind, string(res.Outcome), req.Text, req.Acceptance,
			res.Observed, res.Action, strconv.FormatBool(res.Evaluated),
		}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

var groupTitles = []struct{ kind, title string }{
	{"FR", "FUNCTIONAL REQUIREMENTS"},
	{"NFR", "NON-FUNCTIONAL REQUIREMENTS"},
	{"CHECK", "SUPPORTING CHECKS (the measurements behind the rows above)"},
}

const detailWidth = 96

// PrintTable writes the aligned matrix, grouped FR, NFR, then checks. Each row
// is one aligned line — id, outcome, short name — with what was measured and
// what is owed wrapped underneath it.
func PrintTable(w io.Writer, rows []Requirement, rec *Recorder) {
	for _, g := range groupTitles {
		var group []Requirement
		for _, r := range rows {
			if r.Kind == g.kind {
				group = append(group, r)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", g.title)
		fmt.Fprintf(w, "%-6s  %-7s  %s\n", "ID", "OUTCOME", "REQUIREMENT")
		fmt.Fprintf(w, "%s  %s  %s\n", strings.Repeat("-", 6), strings.Repeat("-", 7), strings.Repeat("-", 44))
		for _, r := range group {
			res := rec.Get(r.ID)
			fmt.Fprintf(w, "%-6s  %-7s  %s\n", r.ID, res.Outcome, r.Short)
			if res.Observed != "" {
				writeWrapped(w, "measured: ", res.Observed)
			}
			switch res.Outcome {
			case Manual:
				writeWrapped(w, "to do:    ", res.Action)
			case Blocked:
				if !res.Evaluated {
					writeWrapped(w, "needs:    ", "no check ran for this row — "+res.Action)
				} else {
					writeWrapped(w, "needs:    ", res.Action)
				}
			}
		}
	}
}

// writeWrapped prints a labelled paragraph indented under its row.
func writeWrapped(w io.Writer, label, text string) {
	const indent = "                 " // 6 + 2 + 7 + 2 spaces: under REQUIREMENT
	lines := wrap(text, detailWidth-len(indent)-len(label))
	for i, l := range lines {
		if i == 0 {
			fmt.Fprintf(w, "%s%s%s\n", indent, label, l)
		} else {
			fmt.Fprintf(w, "%s%s%s\n", indent, strings.Repeat(" ", len(label)), l)
		}
	}
}

func wrap(text string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	var line strings.Builder
	for _, word := range strings.Fields(text) {
		if line.Len() > 0 && line.Len()+1+len(word) > width {
			out = append(out, line.String())
			line.Reset()
		}
		if line.Len() > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(word)
	}
	if line.Len() > 0 {
		out = append(out, line.String())
	}
	return out
}

// tallyOf counts outcomes over a subset of rows.
func tallyOf(rows []Requirement, rec *Recorder) map[Outcome]int {
	out := map[Outcome]int{Pass: 0, Fail: 0, Manual: 0, Blocked: 0}
	for _, r := range rows {
		out[rec.Get(r.ID).Outcome]++
	}
	return out
}

func idsWith(rows []Requirement, rec *Recorder, want ...Outcome) []string {
	var out []string
	for _, r := range rows {
		o := rec.Get(r.ID).Outcome
		for _, w := range want {
			if o == w {
				out = append(out, r.ID)
			}
		}
	}
	return out
}

// PrintSummary answers the examiner's question directly: which claims were
// measured, and which were only asserted or left for a person.
func PrintSummary(w io.Writer, rows []Requirement, rec *Recorder) {
	var reqs, checks []Requirement
	for _, r := range rows {
		if r.Kind == "CHECK" {
			checks = append(checks, r)
		} else {
			reqs = append(reqs, r)
		}
	}
	line := func(name string, set []Requirement) {
		t := tallyOf(set, rec)
		fmt.Fprintf(w, "%-19s %2d rows   PASS %2d   FAIL %2d   MANUAL %2d   BLOCKED %2d\n",
			name, len(set), t[Pass], t[Fail], t[Manual], t[Blocked])
	}
	fmt.Fprintln(w, "\nSUMMARY")
	line("FR + NFR", reqs)
	line("Supporting checks", checks)
	list := func(label string, ids []string) {
		if len(ids) == 0 {
			ids = []string{"none"}
		}
		fmt.Fprintf(w, "  %-34s %s\n", label, strings.Join(ids, ", "))
	}
	list("Verified automatically (PASS/FAIL):", idsWith(reqs, rec, Pass, Fail))
	list("Needs a person (MANUAL):", idsWith(reqs, rec, Manual))
	list("Could not run here (BLOCKED):", idsWith(reqs, rec, Blocked))
	if failed := idsWith(rows, rec, Fail); len(failed) > 0 {
		list("FAILED:", failed)
	}
}
