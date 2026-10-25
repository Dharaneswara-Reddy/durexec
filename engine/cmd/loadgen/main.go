// Command loadgen produces the three measured numbers this project's report
// needs, against a cluster that is already running.
//
// The design document makes three quantitative promises. None of them is
// proved by any test in the repository, because each one is a property of a
// live multi-node deployment rather than of a function:
//
//	NFR-5  throughput scales by at least 1.7x from one shard to two
//	NFR-2  a shard is unavailable for less than 6s at the median, 15s at p99,
//	       after the node owning it dies
//	NFR-3  zero duplicated side effects in 1000 runs
//
// This command measures all three. It never asserts them. Where a number could
// not be measured it says so and prints no number, because a target reprinted
// as though it were a result is worse than no evidence at all.
//
// # What it does not do
//
// It does not kill anything. Recovery mode watches for a node loss that
// somebody else causes — `make kill-owner`, `docker kill node-b`, pulling a
// cable — and times how long the affected shard stays unusable. Keeping the
// killing outside this tool is deliberate: a harness that both breaks the
// system and judges the break can accidentally measure its own timing rather
// than the engine's.
//
// # Modes
//
//	-mode throughput   drive load for a fixed time against a fixed shard set
//	-mode scaling      run throughput at several shard counts and divide
//	-mode arrival      OPEN LOOP: offer a fixed arrival rate and find saturation
//	-mode recovery     watch /v1/shards for handovers, time each outage
//	-mode duplicates   run a batch, then audit the event log with SQL
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

func main() {
	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}

	// A run interrupted by ctrl-C still prints whatever it managed to
	// measure. Throwing away twenty minutes of samples because somebody
	// stopped the run early would be a poor trade.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

type config struct {
	mode string

	bases     []string
	nodeURLs  map[string]string
	httpTO    time.Duration
	maxFollow int

	shardCount int32
	shards     int
	shardIDs   string
	spread     bool

	duration      time.Duration
	warmup        time.Duration
	grace         time.Duration
	concurrency   int
	poll          time.Duration
	shardPoll     time.Duration
	probeInterval time.Duration
	probeTimeout  time.Duration
	sweep         []int

	// Open-loop mode. rates is the arrival rates to sweep; serviceFloor is
	// the workload's own latency floor, which must be declared BEFORE a run
	// because the saturation gate is meaningless without it; arrivalWorkers
	// bounds the generator so its own saturation is reported rather than
	// hidden as memory growth.
	rates          []float64
	serviceFloor   time.Duration
	arrivalWorkers int

	workflowType string
	idPrefix     string
	prep         time.Duration
	amountCents  int64

	runs        int
	dsn         string
	auditPrefix string

	csvPath string
}

// stringList collects a repeatable flag, used for -node.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func parseFlags(args []string, errOut io.Writer) (config, error) {
	var (
		cfg   config
		bases string
		nodes stringList
		sweep string
		rates string
	)

	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(errOut)

	fs.StringVar(&cfg.mode, "mode", "throughput", "throughput | scaling | arrival | recovery | duplicates")
	fs.StringVar(&bases, "base", "http://localhost:8081,http://localhost:8082,http://localhost:8083",
		"comma-separated base URLs; load is spread over all of them")
	fs.Var(&nodes, "node", "explicit node-id=base-url mapping, repeatable; normally discovered from /healthz")
	fs.DurationVar(&cfg.httpTO, "timeout", 10*time.Second, "per-request HTTP timeout")
	fs.IntVar(&cfg.maxFollow, "max-follow", 6, "how many times one request may be re-sent after a 409 NotOwner")

	shardCount := fs.Int("shard-count", 16, "total shards in the cluster; MUST equal the cluster's SHARD_COUNT")
	fs.IntVar(&cfg.shards, "shards", 1, "how many shards to drive load at (1, 2, 4 ...)")
	fs.StringVar(&cfg.shardIDs, "shard-ids", "", "explicit shard ids, e.g. 0,5; overrides -shards")
	fs.BoolVar(&cfg.spread, "spread", true,
		"pick target shards owned by distinct nodes where possible, so -shards 2 really is two machines")

	fs.DurationVar(&cfg.duration, "duration", 60*time.Second, "length of the measurement window")
	fs.DurationVar(&cfg.warmup, "warmup", 5*time.Second, "time excluded from the window while pools and caches settle")
	fs.DurationVar(&cfg.grace, "grace", 30*time.Second, "how long to keep polling in-flight workflows after the window closes")
	fs.IntVar(&cfg.concurrency, "concurrency", 32, "concurrent in-flight workflow starts")
	fs.DurationVar(&cfg.poll, "poll", 100*time.Millisecond, "how often a started workflow is polled for completion")
	fs.DurationVar(&cfg.shardPoll, "shard-poll", 250*time.Millisecond, "how often /v1/shards is sampled in recovery mode")
	fs.DurationVar(&cfg.probeInterval, "probe-interval", time.Second, "gap between recovery probe workflows on one shard")
	fs.DurationVar(&cfg.probeTimeout, "probe-timeout", 60*time.Second, "how long one recovery probe may take before it is abandoned")
	fs.StringVar(&sweep, "sweep", "1,2,4", "shard counts to run in -mode scaling")
	fs.StringVar(&rates, "rates", "", "arrival rates in wf/s to sweep in -mode arrival, e.g. 5,10,20,40")
	fs.DurationVar(&cfg.serviceFloor, "service-floor", 0,
		"the workload's own latency floor, REQUIRED in -mode arrival. For DeliveryOrder "+
			"with -prep 50ms it is about 2.01s: 560ms of activity sleeps, the prep timer, "+
			"the timer scanner's 1s tick and up to one status poll. A point whose p50 sits "+
			"on this floor is an idle engine, not a saturated one")
	fs.IntVar(&cfg.arrivalWorkers, "arrival-workers", 0,
		"cap on concurrent in-flight starts in -mode arrival (0 derives it from -rates and -service-floor)")

	fs.StringVar(&cfg.workflowType, "workflow-type", "DeliveryOrder", "workflow type to start")
	fs.StringVar(&cfg.idPrefix, "id-prefix", "", "workflow id prefix; default is loadgen-<unix seconds>")
	fs.DurationVar(&cfg.prep, "prep", 50*time.Millisecond,
		"DeliveryOrder prep timer; the workflow's own default is 12 minutes, which no benchmark can wait for")
	fs.Int64Var(&cfg.amountCents, "amount-cents", 2450, "order amount; must be positive or ChargePayment fails by design")

	fs.IntVar(&cfg.runs, "runs", 1000, "batch size in -mode duplicates; NFR-3 is stated per 1000 runs")
	fs.StringVar(&cfg.dsn, "dsn", "postgres://durexec:durexec@localhost:5433/durexec?sslmode=disable",
		"PostgreSQL DSN for the -mode duplicates audit")
	fs.StringVar(&cfg.auditPrefix, "audit-prefix", "",
		"audit workflows an earlier run created, generating no new load; the prefix that run printed")

	fs.StringVar(&cfg.csvPath, "csv", "", "append results to this CSV file (header written once)")

	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	cfg.shardCount = int32(*shardCount)
	if cfg.shardCount <= 0 {
		return cfg, fmt.Errorf("-shard-count must be positive, got %d", cfg.shardCount)
	}
	for _, b := range strings.Split(bases, ",") {
		if b = strings.TrimSpace(strings.TrimRight(b, "/")); b != "" {
			cfg.bases = append(cfg.bases, b)
		}
	}
	if len(cfg.bases) == 0 {
		return cfg, errors.New("-base must name at least one URL")
	}

	cfg.nodeURLs = map[string]string{}
	for _, n := range nodes {
		id, url, ok := strings.Cut(n, "=")
		if !ok || id == "" || url == "" {
			return cfg, fmt.Errorf("-node %q must be node-id=base-url", n)
		}
		cfg.nodeURLs[id] = strings.TrimRight(url, "/")
	}

	var err error
	if cfg.sweep, err = parseInts(sweep); err != nil {
		return cfg, fmt.Errorf("-sweep: %w", err)
	}
	// Empty is allowed here and refused in runArrivalMode, so every other
	// mode keeps working with no new required flag.
	if rates != "" {
		if cfg.rates, err = parseFloats(rates); err != nil {
			return cfg, fmt.Errorf("-rates: %w", err)
		}
	}

	if cfg.idPrefix == "" {
		cfg.idPrefix = "loadgen-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	if cfg.concurrency <= 0 {
		return cfg, fmt.Errorf("-concurrency must be positive, got %d", cfg.concurrency)
	}
	if cfg.warmup >= cfg.duration {
		return cfg, fmt.Errorf("-warmup (%s) must be shorter than -duration (%s)", cfg.warmup, cfg.duration)
	}
	return cfg, nil
}

// parseFloats reads a comma-separated list of arrival rates. Rates are
// fractional on purpose: below about one workflow a second, an integer sweep
// cannot get near a slow workload's knee.
func parseFloats(s string) ([]float64, error) {
	var out []float64
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a number", f)
		}
		if n <= 0 {
			return nil, fmt.Errorf("%q is not a positive rate", f)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("no values")
	}
	return out, nil
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", f)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("no values")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Dispatch.
// ---------------------------------------------------------------------------

func run(ctx context.Context, cfg config, out io.Writer) error {
	client := NewClient(cfg.bases, cfg.nodeURLs, cfg.httpTO, cfg.maxFollow)

	// Every mode needs to know which node answers on which URL, because a 409
	// names a node id ("node-b") and a client can only follow it if it knows
	// where node-b listens.
	if err := client.Discover(ctx); err != nil {
		return fmt.Errorf("discovering cluster nodes: %w", err)
	}
	fmt.Fprintf(out, "cluster: %s\n", client.Describe())

	var rows []Row
	var err error

	switch cfg.mode {
	case "throughput":
		rows, err = runThroughputMode(ctx, client, cfg, out)
	case "scaling":
		rows, err = runScalingMode(ctx, client, cfg, out)
	case "recovery":
		rows, err = runRecoveryMode(ctx, client, cfg, out)
	case "duplicates":
		rows, err = runDuplicatesMode(ctx, client, cfg, out)
	case "arrival":
		rows, err = runArrivalMode(ctx, client, cfg, out)
	default:
		return fmt.Errorf("unknown -mode %q (throughput, scaling, arrival, recovery, duplicates)", cfg.mode)
	}
	if err != nil {
		return err
	}

	fmt.Fprintln(out)
	RenderTable(out, rows)
	fmt.Fprintln(out)
	fmt.Fprint(out, provenanceNote())

	if cfg.csvPath != "" {
		if err := WriteCSVRows(cfg.csvPath, rows); err != nil {
			return fmt.Errorf("writing %s: %w", cfg.csvPath, err)
		}
		fmt.Fprintf(out, "\nCSV appended: %s\n", cfg.csvPath)
	}
	return nil
}

// provenanceNote is printed under every table. The brief for this tool asks
// that a reader never has to guess which numbers came off a running cluster
// and which are restatements of the design document, so the distinction is
// spelled out rather than implied by a column heading.
func provenanceNote() string {
	return "how to read the 'kind' column\n" +
		"  measured      an observation taken from the running cluster in this run\n" +
		"  derived       arithmetic over this run's measurements (a ratio, a percentile)\n" +
		"  assumed       a value this tool was told, not something it checked\n" +
		"  not-measured  this run produced no evidence; the target column still shows\n" +
		"                what the design document asks for, and the value column is empty\n"
}

// ---------------------------------------------------------------------------
// Shard selection.
// ---------------------------------------------------------------------------

// selectTargets decides which shards the load will be aimed at.
//
// The default is not "shards 0..n-1". Two adjacent shard ids are frequently
// owned by the same node, and driving both of them would measure one machine
// twice while the CSV column said "2 shards". When -spread is on, the tool
// reads the live ownership table and picks shards belonging to distinct
// owners, so a scaling curve is a curve over machines rather than over
// integers.
func selectTargets(shardsWanted int, explicit string, shardCount int32, owners map[core.ShardID]string, spread bool) ([]core.ShardID, error) {
	if explicit != "" {
		ints, err := parseInts(explicit)
		if err != nil {
			return nil, err
		}
		out := make([]core.ShardID, 0, len(ints))
		for _, n := range ints {
			if n < 0 || int32(n) >= shardCount {
				return nil, fmt.Errorf("shard id %d is outside 0..%d", n, shardCount-1)
			}
			out = append(out, core.ShardID(n))
		}
		return out, nil
	}
	if shardsWanted <= 0 {
		return nil, fmt.Errorf("-shards must be positive, got %d", shardsWanted)
	}
	if int32(shardsWanted) > shardCount {
		return nil, fmt.Errorf("-shards %d exceeds -shard-count %d", shardsWanted, shardCount)
	}

	all := make([]core.ShardID, 0, shardCount)
	for i := int32(0); i < shardCount; i++ {
		all = append(all, core.ShardID(i))
	}
	if !spread || len(owners) == 0 {
		return all[:shardsWanted], nil
	}

	// First pass: one shard per distinct owner, in shard order so the choice
	// is reproducible between runs. Second pass: fill from whatever is left.
	seen := map[string]bool{}
	out := make([]core.ShardID, 0, shardsWanted)
	for _, s := range all {
		if len(out) == shardsWanted {
			break
		}
		o := owners[s]
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		out = append(out, s)
	}
	chosen := map[core.ShardID]bool{}
	for _, s := range out {
		chosen[s] = true
	}
	for _, s := range all {
		if len(out) == shardsWanted {
			break
		}
		if !chosen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// ownersOf reads the live ownership table for the shards given.
func ownersOf(ctx context.Context, c *Client) (map[core.ShardID]string, error) {
	resp, err := c.Shards(ctx)
	if err != nil {
		return nil, err
	}
	owners := make(map[core.ShardID]string, len(resp.Shards))
	for _, s := range resp.Shards {
		if !s.Expired {
			owners[s.ShardID] = s.Owner
		}
	}
	return owners, nil
}

func shardsString(ss []core.ShardID) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = strconv.FormatInt(int64(s), 10)
	}
	return strings.Join(parts, ",")
}

func distinctOwners(targets []core.ShardID, owners map[core.ShardID]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range targets {
		o := owners[s]
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		out = append(out, o)
	}
	return out
}
