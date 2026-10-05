package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// Kind records where a number came from. It is a required field on every row,
// because the one thing a reader of this report must never have to guess is
// whether a figure was observed or asserted.
const (
	KindMeasured    = "measured"
	KindDerived     = "derived"
	KindAssumed     = "assumed"
	KindNotMeasured = "not-measured"
)

// Verdict is the judgement against the design document's stated target.
const (
	VerdictMet        = "MET"
	VerdictNotMet     = "NOT MET"
	VerdictNoTarget   = "-"
	VerdictNoEvidence = "NO EVIDENCE"
)

// Row is one line of the report, in both renderings.
//
// Value and Target are separate columns and are never merged. A target with no
// measurement beside it prints as an empty value and a NO EVIDENCE verdict,
// which is the honest rendering of "we did not find out".
type Row struct {
	Measurement string
	NFR         string
	Metric      string
	Value       string
	Unit        string
	Target      string
	Verdict     string
	Kind        string
	Note        string
}

func csvHeader() []string {
	return []string{"measurement", "nfr", "metric", "value", "unit", "target", "verdict", "kind", "note"}
}

func (r Row) record() []string {
	return []string{r.Measurement, r.NFR, r.Metric, r.Value, r.Unit, r.Target, r.Verdict, r.Kind, r.Note}
}

// FormatCSV renders rows as CSV text, optionally with the header line.
func FormatCSV(rows []Row, withHeader bool) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if withHeader {
		_ = w.Write(csvHeader())
	}
	for _, r := range rows {
		_ = w.Write(r.record())
	}
	w.Flush()
	return buf.String()
}

// WriteCSVRows appends rows to path, writing the header only when the file is
// new or empty.
//
// Appending rather than truncating is what lets the scaling curve be built
// from three separate invocations — one per shard count — into a single file
// that can be pasted into the report whole.
func WriteCSVRows(path string, rows []Row) error {
	needHeader := true
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		needHeader = false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, FormatCSV(rows, needHeader))
	return err
}

// RenderTable prints the readable rendering.
func RenderTable(w io.Writer, rows []Row) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NFR\tMETRIC\tVALUE\tTARGET\tVERDICT\tKIND")
	fmt.Fprintln(tw, "---\t------\t-----\t------\t-------\t----")
	for _, r := range rows {
		value := r.Value
		if value != "" && r.Unit != "" {
			value += " " + r.Unit
		}
		if value == "" {
			value = "(not measured)"
		}
		target := r.Target
		if target == "" {
			target = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(r.NFR), r.Metric, value, target, dash(r.Verdict), r.Kind)
	}
	tw.Flush()

	// Notes are printed under the table rather than in a column: they are
	// sentences, and a sentence in a tab-aligned column destroys the alignment
	// that makes the numbers readable.
	printed := false
	for _, r := range rows {
		if r.Note == "" {
			continue
		}
		if !printed {
			fmt.Fprintln(w, "\nnotes")
			printed = true
		}
		fmt.Fprintf(w, "  %-34s %s\n", r.Metric, r.Note)
	}
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
