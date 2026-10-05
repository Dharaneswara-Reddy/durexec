// Command acceptance checks a running cluster against the design document's
// requirement tables, FR-1..FR-16 and NFR-1..NFR-10, and reports exactly one
// of four outcomes for each:
//
//	PASS     checked here, against the running system, and the condition held
//	FAIL     checked here, and it did not
//	MANUAL   cannot be established from outside the process; the row says
//	         what a person must do (and which unit test covers it, if any)
//	BLOCKED  the check exists, but something it needs was unavailable; the row
//	         names it
//
// The rule it is built around: a row is PASS only when this tool measured the
// stated acceptance condition itself. Coverage by a unit test elsewhere is a
// MANUAL row with a pointer to that test, never a PASS.
//
// Usage (from the engine directory, with the stack up):
//
//	go run ./cmd/acceptance
//	go run ./cmd/acceptance -csv acceptance.csv -timeout 5m
//
// The exit status is 1 if and only if some row is FAIL. MANUAL and BLOCKED rows
// describe this tool's reach, not a defect in the engine, and do not fail the
// run. A command-line usage error exits 2, because then nothing was checked.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, OpenStore))
}

// run is main with its dependencies passed in, so the whole command can be
// exercised by a test with a fake store and httptest nodes.
func run(args []string, stdout, stderr io.Writer, open func(context.Context, string) (Store, error)) int {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cluster    = fs.String("cluster", "http://localhost:8081,http://localhost:8082,http://localhost:8083", "comma-separated engine node base URLs")
		dsn        = fs.String("dsn", "postgres://durexec:durexec@localhost:5433/durexec?sslmode=disable", "engine database DSN (read-only use)")
		standbyDSN = fs.String("standby-dsn", "postgres://durexec:durexec@localhost:5434/durexec?sslmode=disable", "replication follower DSN for FR-9 evidence; empty to skip")
		csvPath    = fs.String("csv", "acceptance-results.csv", "CSV output path; empty to skip")
		timeout    = fs.Duration("timeout", 4*time.Minute, "overall time limit for the whole run")
		reqTimeout = fs.Duration("request-timeout", 5*time.Second, "per-HTTP-request time limit")
		budget     = fs.Duration("workflow-budget", 90*time.Second, "how long a driven workflow may take to reach a terminal state")
		sleep      = fs.Duration("timer-sleep", 2*time.Second, "durable sleep requested in the driven DeliveryOrder")
		drive      = fs.Bool("drive", true, "start the tool's own DeliveryOrder and TravelAgent workflows")
		inject     = fs.Bool("inject", false, "also run a DeliveryOrder with DispatchDriver armed to fail, to produce a compensation (process-wide on the owning node: do not use on a shared cluster)")
		limit      = fs.Int("event-limit", 500000, "maximum events rows read for the log analyses")
		compose    = fs.String("compose", "deploy/docker-compose.yml", "compose file the stack is started from (NFR-9, NFR-10)")
		gomod      = fs.String("gomod", "go.mod", "engine go.mod (NFR-10)")
		compMap    = fs.String("compensation-map", "", "Undo=Forward pairs overriding the built-in map, e.g. RefundPayment=ChargePayment,...")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cm, err := parseCompMap(*compMap)
	if err != nil {
		fmt.Fprintln(stderr, "acceptance:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	opts := Options{
		BaseURLs:       splitList(*cluster),
		DSN:            *dsn,
		StandbyDSN:     *standbyDSN,
		ComposePath:    *compose,
		GoModPath:      *gomod,
		Drive:          *drive,
		Inject:         *inject,
		WorkflowBudget: *budget,
		PollInterval:   500 * time.Millisecond,
		TimerSleep:     *sleep,
		EventLimit:     *limit,
		CompMap:        cm,
		RunTag:         runTag(),
	}

	c := NewCluster(opts.BaseURLs, &http.Client{Timeout: *reqTimeout})
	ev := Gather(ctx, opts, c, open, stderr)

	rows := AllRows()
	rec := NewRecorder(rows)
	Evaluate(rec, ev)

	fmt.Fprintf(stdout, "Acceptance matrix  run %s  cluster %s\n", opts.RunTag, strings.Join(opts.BaseURLs, ","))
	PrintTable(stdout, rows, rec)
	PrintSummary(stdout, rows, rec)

	if *csvPath != "" {
		if err := writeCSVFile(*csvPath, rows, rec); err != nil {
			// Reported, but it does not change the exit status: the status
			// answers "did a requirement fail", and a full disk does not.
			fmt.Fprintf(stderr, "acceptance: could not write %s: %v\n", *csvPath, err)
		} else {
			fmt.Fprintf(stdout, "\nCSV written to %s\n", *csvPath)
		}
	}
	return rec.ExitCode()
}

func writeCSVFile(path string, rows []Requirement, rec *Recorder) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := WriteCSV(f, rows, rec); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseCompMap reads "Undo=Forward,Undo=Forward". Empty means the built-in map.
func parseCompMap(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range splitList(s) {
		undo, fwd, ok := strings.Cut(pair, "=")
		undo, fwd = strings.TrimSpace(undo), strings.TrimSpace(fwd)
		if !ok || undo == "" || fwd == "" {
			return nil, fmt.Errorf("-compensation-map: %q is not Undo=Forward", pair)
		}
		out[undo] = fwd
	}
	return out, nil
}

// runTag makes the driven workflow ids unique per run and recognisable in the
// database: acceptance-delivery-20260921T101500-3fa2.
func runTag() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}
