// Package config loads the engine's runtime settings from the environment and
// refuses to start on a combination that would be unsafe.
//
// Validation here is not defensive box-ticking. Two of the rules encode real
// operational failure modes that are cheap to get wrong and expensive to
// discover in production: a lease renewal interval too close to the lease TTL,
// and fault injection reaching a real deployment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variable names, listed once so the loader and the documentation
// cannot drift apart.
const (
	EnvShardCount      = "SHARD_COUNT"
	EnvLeaseTTL        = "LEASE_TTL"
	EnvLeaseRenew      = "LEASE_RENEW"
	EnvTaskLock        = "TASK_LOCK"
	EnvWorkflowWorkers = "WORKFLOW_WORKERS"
	EnvActivityWorkers = "ACTIVITY_WORKERS"
	EnvSyncCommit      = "SYNC_COMMIT"
	EnvTauHigh         = "TAU_HIGH"
	EnvTauLow          = "TAU_LOW"
	EnvNodeID          = "NODE_ID"
	EnvListenAddr      = "LISTEN_ADDR"
	EnvDatabaseURL     = "DATABASE_URL"
	EnvEmbedderURL     = "EMBEDDER_URL"
	EnvFaultInjection  = "FAULT_INJECTION"
	EnvEscalationBlock = "ESCALATION_BLOCKS"
	EnvClusterNodes    = "CLUSTER_NODES"
)

// leaseRenewSafetyFactor is the number of renewal attempts that must fit
// inside one lease TTL. Three means a node can miss one renewal entirely — a
// GC pause, a brief network partition, a slow query — and still have a whole
// interval left to succeed before another node is allowed to take the shard.
const leaseRenewSafetyFactor = 3

// Config is the full set of knobs the engine reads at startup. Every field is
// set by Load; nothing is read from the environment after that, so the running
// configuration is a value that can be logged and tested.
type Config struct {
	// ShardCount is fixed for the life of a deployment. Changing it re-maps
	// every workflow to a different shard, so it is validated, never guessed.
	// ClusterNodes is every node id in the cluster, including this one. The
	// consistent hash ring is a pure function of this list, and it is what
	// decides which node prefers which shard. A node missing from the list is
	// never assigned work by the ring, so all nodes must be given the same
	// list. Defaults to just this node, which is correct for a single-node run.
	ClusterNodes []string

	ShardCount int

	// LeaseTTL is how long a shard lease survives without renewal. It is also
	// the worst-case time a shard is unowned after a node dies.
	LeaseTTL time.Duration

	// LeaseRenew is how often the owner refreshes its lease.
	LeaseRenew time.Duration

	// TaskLock is how long a polled task stays invisible to other workers.
	TaskLock time.Duration

	WorkflowWorkers int
	ActivityWorkers int

	// SyncCommit maps to PostgreSQL synchronous_commit. Turning it off trades
	// the durability guarantee this engine exists to provide for throughput,
	// so it defaults to on and should only be off in a benchmark.
	SyncCommit bool

	// TauHigh and TauLow bound the semantic dedup escalation band.
	TauHigh float64
	TauLow  float64

	NodeID      string
	ListenAddr  string
	DatabaseURL string
	EmbedderURL string

	// EscalationBlocks chooses what happens when the semantic duplicate check
	// lands in the uncertain band between TauLow and TauHigh.
	//
	// False, the default, means the activity runs. The verdict is still
	// journaled, so the band stays fully measurable, but the engine never
	// suppresses an action it was unsure about. True switches to the stricter
	// policy of design decision D5: the activity is not scheduled and the run
	// waits for a person to adjudicate before anything happens.
	//
	// It is a flag rather than a hard-coded choice because the evaluation has
	// to be able to run the same workload under both policies and compare
	// them. See internal/runtime/dedup.go for the full argument.
	EscalationBlocks bool

	// FaultInjection arms internal/faultinj. See faultExplicit.
	FaultInjection bool

	// faultExplicit records that FAULT_INJECTION was actually present in the
	// environment and parsed as true. It is unexported so no code path can set
	// FaultInjection to true by assignment and have it survive Validate; the
	// only way to enable fault injection is to say so in the environment.
	faultExplicit bool
}

// Default returns the configuration with every optional field at its default.
// The required fields — NodeID, DatabaseURL, EmbedderURL — are left empty and
// will fail Validate, which is the intended behaviour.
func Default() Config {
	return Config{
		ShardCount:      16,
		LeaseTTL:        10 * time.Second,
		LeaseRenew:      3 * time.Second,
		TaskLock:        30 * time.Second,
		WorkflowWorkers: 4,
		ActivityWorkers: 8,
		SyncCommit:      true,
		TauHigh:         0.90,
		TauLow:          0.85,
		ListenAddr:      ":8080",
	}
}

// Load reads the process environment, applies defaults, fills NodeID from the
// hostname when it was not supplied, and validates the result.
func Load() (Config, error) {
	cfg, err := LoadFrom(os.LookupEnv)
	if err != nil {
		return cfg, err
	}
	if cfg.NodeID == "" {
		// A container's hostname is its identity in every orchestrator we
		// target, and the shard lease needs a stable owner name.
		if host, hostErr := os.Hostname(); hostErr == nil {
			cfg.NodeID = host
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	if len(cfg.ClusterNodes) == 0 {
		// A node that is told nothing about peers is a cluster of one. That is
		// the right default: it owns every shard, which is exactly what a
		// single-node run should do.
		cfg.ClusterNodes = []string{cfg.NodeID}
	}
	return cfg, nil
}

// LoadFrom builds a Config from an arbitrary lookup function. It applies
// defaults and reports parse errors but does not validate, so tests can build
// a deliberately invalid Config and check the message Validate produces.
func LoadFrom(lookup func(string) (string, bool)) (Config, error) {
	cfg := Default()
	var errs []error

	get := func(key string) (string, bool) {
		v, ok := lookup(key)
		if !ok {
			return "", false
		}
		v = strings.TrimSpace(v)
		return v, v != ""
	}

	if v, ok := get(EnvShardCount); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not an integer: %w", EnvShardCount, v, err))
		} else {
			cfg.ShardCount = n
		}
	}

	durations := []struct {
		key string
		dst *time.Duration
	}{
		{EnvLeaseTTL, &cfg.LeaseTTL},
		{EnvLeaseRenew, &cfg.LeaseRenew},
		{EnvTaskLock, &cfg.TaskLock},
	}
	for _, d := range durations {
		if v, ok := get(d.key); ok {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not a duration such as %q or %q: %w", d.key, v, "10s", "1m30s", err))
				continue
			}
			*d.dst = parsed
		}
	}

	ints := []struct {
		key string
		dst *int
	}{
		{EnvWorkflowWorkers, &cfg.WorkflowWorkers},
		{EnvActivityWorkers, &cfg.ActivityWorkers},
	}
	for _, i := range ints {
		if v, ok := get(i.key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not an integer: %w", i.key, v, err))
				continue
			}
			*i.dst = n
		}
	}

	if v, ok := get(EnvSyncCommit); ok {
		b, err := parseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a boolean (use on/off, true/false, 1/0)", EnvSyncCommit, v))
		} else {
			cfg.SyncCommit = b
		}
	}

	floats := []struct {
		key string
		dst *float64
	}{
		{EnvTauHigh, &cfg.TauHigh},
		{EnvTauLow, &cfg.TauLow},
	}
	for _, f := range floats {
		if v, ok := get(f.key); ok {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not a number: %w", f.key, v, err))
				continue
			}
			*f.dst = n
		}
	}

	if v, ok := get(EnvNodeID); ok {
		cfg.NodeID = v
	}
	if v, ok := get(EnvClusterNodes); ok {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				cfg.ClusterNodes = append(cfg.ClusterNodes, n)
			}
		}
	}
	if v, ok := get(EnvListenAddr); ok {
		cfg.ListenAddr = v
	}
	if v, ok := get(EnvDatabaseURL); ok {
		cfg.DatabaseURL = v
	}
	if v, ok := get(EnvEmbedderURL); ok {
		cfg.EmbedderURL = v
	}

	if v, ok := get(EnvEscalationBlock); ok {
		b, err := parseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a boolean (use on/off, true/false, 1/0)", EnvEscalationBlock, v))
		} else {
			cfg.EscalationBlocks = b
		}
	}

	if v, ok := get(EnvFaultInjection); ok {
		b, err := parseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a boolean (use on/off, true/false, 1/0)", EnvFaultInjection, v))
		} else {
			cfg.FaultInjection = b
			cfg.faultExplicit = b
		}
	}

	return cfg, errors.Join(errs...)
}

// Validate reports every problem at once rather than the first one, so an
// operator fixing a deployment sees the whole list in a single restart.
func (c Config) Validate() error {
	var errs []error

	if c.ShardCount <= 0 {
		errs = append(errs, fmt.Errorf(
			"%s must be a positive number of shards, got %d: the shard count is the fixed partitioning of the whole keyspace and there is nothing to run with zero partitions",
			EnvShardCount, c.ShardCount))
	}

	if c.LeaseTTL <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive, got %s", EnvLeaseTTL, c.LeaseTTL))
	}
	if c.LeaseRenew <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive, got %s", EnvLeaseRenew, c.LeaseRenew))
	}
	if c.LeaseTTL > 0 && c.LeaseRenew > 0 && c.LeaseRenew*leaseRenewSafetyFactor > c.LeaseTTL {
		errs = append(errs, fmt.Errorf(
			"%s (%s) must be at most one third of %s (%s), and it is not: an owner that renews every %s inside a %s lease has no room to miss a single renewal, so one slow query or garbage-collection pause hands the shard to another node while work is still in flight; either lower %s to %s or raise %s to %s",
			EnvLeaseRenew, c.LeaseRenew, EnvLeaseTTL, c.LeaseTTL,
			c.LeaseRenew, c.LeaseTTL,
			EnvLeaseRenew, c.LeaseTTL/leaseRenewSafetyFactor,
			EnvLeaseTTL, c.LeaseRenew*leaseRenewSafetyFactor))
	}

	if c.TaskLock <= 0 {
		errs = append(errs, fmt.Errorf("%s must be positive, got %s", EnvTaskLock, c.TaskLock))
	}

	if c.WorkflowWorkers <= 0 {
		errs = append(errs, fmt.Errorf("%s must be at least 1, got %d", EnvWorkflowWorkers, c.WorkflowWorkers))
	}
	if c.ActivityWorkers <= 0 {
		errs = append(errs, fmt.Errorf("%s must be at least 1, got %d", EnvActivityWorkers, c.ActivityWorkers))
	}

	if c.TauHigh < 0 || c.TauHigh > 1 {
		errs = append(errs, fmt.Errorf("%s must be a cosine similarity between 0 and 1, got %v", EnvTauHigh, c.TauHigh))
	}
	if c.TauLow < 0 || c.TauLow > 1 {
		errs = append(errs, fmt.Errorf("%s must be a cosine similarity between 0 and 1, got %v", EnvTauLow, c.TauLow))
	}
	if c.TauLow > c.TauHigh {
		errs = append(errs, fmt.Errorf(
			"%s (%v) must not exceed %s (%v): the two thresholds bound the band where the engine is unsure and escalates to a human, and an inverted band would make every score either a duplicate or distinct with no uncertainty left",
			EnvTauLow, c.TauLow, EnvTauHigh, c.TauHigh))
	}

	if c.NodeID == "" {
		errs = append(errs, fmt.Errorf(
			"%s must be set: it is the owner name written into the shard lease, so two nodes sharing one identity would each believe they own the other's shards",
			EnvNodeID))
	}
	if c.ListenAddr == "" {
		errs = append(errs, fmt.Errorf("%s must be set, for example %q", EnvListenAddr, ":8080"))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, fmt.Errorf("%s must be set: the event log is the engine's only durable state", EnvDatabaseURL))
	}
	// EMBEDDER_URL is deliberately NOT required.
	//
	// It was, and the reasoning was sound: a misconfigured embedder should
	// surface on deploy rather than mid-workflow. But the engine ships with a
	// deterministic stand-in encoder (semantic.FakeEmbedder), so an unset URL
	// is not a misconfiguration at all — it is a complete, runnable node with
	// no model server attached, which is what lets a reviewer start the binary
	// on a laptop and watch a verdict get journaled. Leaving it out is
	// therefore a choice, and SemanticIsFake reports which choice was made so
	// startup can say so out loud.

	if c.FaultInjection && !c.faultExplicit {
		errs = append(errs, fmt.Errorf(
			"fault injection is on but %s was not set to true in the environment: the switch may only be thrown by explicit configuration, never by code, so that a deliberate failure can never be injected into a real deployment",
			EnvFaultInjection))
	}

	return errors.Join(errs...)
}

// SemanticIsFake reports that no embedding service was configured, so the
// engine will use its built-in deterministic stand-in encoder. Startup logs
// this, because a verdict produced by the fake encoder measures the plumbing
// and not the contribution.
func (c Config) SemanticIsFake() bool { return c.EmbedderURL == "" }

// Thresholds exposes the dedup band in the shape core expects.
func (c Config) Thresholds() (tauLow, tauHigh float64) { return c.TauLow, c.TauHigh }

// String renders the configuration for the startup log. DatabaseURL is
// redacted because it carries a password.
func (c Config) String() string {
	return fmt.Sprintf(
		"node=%s listen=%s shards=%d lease_ttl=%s lease_renew=%s task_lock=%s workers=%d/%d sync_commit=%t tau=%.2f/%.2f db=%s embedder=%s escalation_blocks=%t fault_injection=%t",
		c.NodeID, c.ListenAddr, c.ShardCount, c.LeaseTTL, c.LeaseRenew, c.TaskLock,
		c.WorkflowWorkers, c.ActivityWorkers, c.SyncCommit, c.TauLow, c.TauHigh,
		redactURL(c.DatabaseURL), embedderLabel(c.EmbedderURL), c.EscalationBlocks, c.FaultInjection)
}

// embedderLabel names the encoder in the startup line, so "(built-in fake)"
// appears in the log of any run that was not talking to a real model.
func embedderLabel(raw string) string {
	if raw == "" {
		return "(built-in fake)"
	}
	return raw
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("cannot parse %q as a boolean", v)
	}
}

// redactURL removes the password from a connection string without needing a
// URL parser to succeed on input that may be malformed.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "(set)"
	}
	creds, host, ok := strings.Cut(rest, "@")
	if !ok {
		return scheme + "://" + host
	}
	user, _, hasPassword := strings.Cut(creds, ":")
	if !hasPassword {
		return raw
	}
	return scheme + "://" + user + ":****@" + host
}
