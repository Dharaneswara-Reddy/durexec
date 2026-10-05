package config

import (
	"strings"
	"testing"
	"time"
)

// env builds a lookup function over a fixed map, so no test touches the real
// process environment and tests can run in parallel.
func env(kv map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

// valid is the smallest environment that passes Validate.
func valid() map[string]string {
	return map[string]string{
		EnvNodeID:      "node-a",
		EnvDatabaseURL: "postgres://durexec:secret@db:5432/durexec",
		EnvEmbedderURL: "http://embedder:8081",
	}
}

func TestLoadFromDefaults(t *testing.T) {
	cfg, err := LoadFrom(env(valid()))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}

	want := Default()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{EnvShardCount, cfg.ShardCount, want.ShardCount},
		{EnvLeaseTTL, cfg.LeaseTTL, want.LeaseTTL},
		{EnvLeaseRenew, cfg.LeaseRenew, want.LeaseRenew},
		{EnvTaskLock, cfg.TaskLock, want.TaskLock},
		{EnvWorkflowWorkers, cfg.WorkflowWorkers, want.WorkflowWorkers},
		{EnvActivityWorkers, cfg.ActivityWorkers, want.ActivityWorkers},
		{EnvSyncCommit, cfg.SyncCommit, true},
		{EnvTauHigh, cfg.TauHigh, 0.90},
		{EnvTauLow, cfg.TauLow, 0.85},
		{EnvListenAddr, cfg.ListenAddr, ":8080"},
		{EnvFaultInjection, cfg.FaultInjection, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadFromOverrides(t *testing.T) {
	kv := valid()
	for k, v := range map[string]string{
		EnvShardCount:      "64",
		EnvLeaseTTL:        "30s",
		EnvLeaseRenew:      "5s",
		EnvTaskLock:        "1m",
		EnvWorkflowWorkers: "16",
		EnvActivityWorkers: "32",
		EnvSyncCommit:      "off",
		EnvTauHigh:         "0.95",
		EnvTauLow:          "0.7",
		EnvListenAddr:      "0.0.0.0:9090",
		EnvFaultInjection:  "true",
	} {
		kv[k] = v
	}

	cfg, err := LoadFrom(env(kv))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.ShardCount != 64 || cfg.LeaseTTL != 30*time.Second || cfg.LeaseRenew != 5*time.Second {
		t.Errorf("shard/lease overrides not applied: %+v", cfg)
	}
	if cfg.TaskLock != time.Minute || cfg.WorkflowWorkers != 16 || cfg.ActivityWorkers != 32 {
		t.Errorf("task/worker overrides not applied: %+v", cfg)
	}
	if cfg.SyncCommit {
		t.Error("SYNC_COMMIT=off should disable synchronous commit")
	}
	if cfg.TauHigh != 0.95 || cfg.TauLow != 0.7 {
		t.Errorf("threshold overrides not applied: %v/%v", cfg.TauLow, cfg.TauHigh)
	}
	if cfg.ListenAddr != "0.0.0.0:9090" {
		t.Errorf("LISTEN_ADDR = %q", cfg.ListenAddr)
	}
	if !cfg.FaultInjection {
		t.Error("FAULT_INJECTION=true should enable injection")
	}
}

func TestLoadFromBooleanSpellings(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"on", true}, {"ON", true}, {"true", true}, {"1", true}, {"yes", true},
		{"off", false}, {"OFF", false}, {"false", false}, {"0", false}, {"no", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			kv := valid()
			kv[EnvSyncCommit] = tc.in
			cfg, err := LoadFrom(env(kv))
			if err != nil {
				t.Fatalf("LoadFrom(%q): %v", tc.in, err)
			}
			if cfg.SyncCommit != tc.want {
				t.Fatalf("SYNC_COMMIT=%q gave %v, want %v", tc.in, cfg.SyncCommit, tc.want)
			}
		})
	}
}

func TestLoadFromParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantSub string
	}{
		{"shard count not a number", EnvShardCount, "sixteen", "SHARD_COUNT"},
		{"lease ttl not a duration", EnvLeaseTTL, "10 seconds", "LEASE_TTL"},
		{"workers not a number", EnvWorkflowWorkers, "many", "WORKFLOW_WORKERS"},
		{"tau not a number", EnvTauHigh, "high", "TAU_HIGH"},
		{"sync commit not a boolean", EnvSyncCommit, "maybe", "SYNC_COMMIT"},
		{"fault injection not a boolean", EnvFaultInjection, "perhaps", "FAULT_INJECTION"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kv := valid()
			kv[tc.key] = tc.value
			_, err := LoadFrom(env(kv))
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not name %s", err, tc.wantSub)
			}
		})
	}
}

// Blank and unset must behave the same, so an empty value in a compose file
// falls back to the default instead of silently becoming an empty string.
func TestBlankValueFallsBackToDefault(t *testing.T) {
	kv := valid()
	kv[EnvListenAddr] = "   "
	kv[EnvShardCount] = ""
	cfg, err := LoadFrom(env(kv))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.ListenAddr != ":8080" || cfg.ShardCount != 16 {
		t.Fatalf("blank values did not fall back to defaults: %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantSub string
	}{
		{
			name:   "defaults are valid",
			mutate: func(map[string]string) {},
		},
		{
			name:    "shard count zero",
			mutate:  func(kv map[string]string) { kv[EnvShardCount] = "0" },
			wantSub: "positive number of shards",
		},
		{
			name:    "shard count negative",
			mutate:  func(kv map[string]string) { kv[EnvShardCount] = "-4" },
			wantSub: "positive number of shards",
		},
		{
			name:    "renew equal to ttl",
			mutate:  func(kv map[string]string) { kv[EnvLeaseRenew] = "10s" },
			wantSub: "at most one third",
		},
		{
			name:    "renew just over a third of ttl",
			mutate:  func(kv map[string]string) { kv[EnvLeaseRenew] = "4s" },
			wantSub: "at most one third",
		},
		{
			name:   "renew exactly a third of ttl is allowed",
			mutate: func(kv map[string]string) { kv[EnvLeaseTTL] = "9s"; kv[EnvLeaseRenew] = "3s" },
		},
		{
			name:    "lease ttl zero",
			mutate:  func(kv map[string]string) { kv[EnvLeaseTTL] = "0s" },
			wantSub: "LEASE_TTL must be positive",
		},
		{
			name:    "lease renew zero",
			mutate:  func(kv map[string]string) { kv[EnvLeaseRenew] = "0s" },
			wantSub: "LEASE_RENEW must be positive",
		},
		{
			name:    "task lock zero",
			mutate:  func(kv map[string]string) { kv[EnvTaskLock] = "0s" },
			wantSub: "TASK_LOCK must be positive",
		},
		{
			name:    "no workflow workers",
			mutate:  func(kv map[string]string) { kv[EnvWorkflowWorkers] = "0" },
			wantSub: "WORKFLOW_WORKERS must be at least 1",
		},
		{
			name:    "no activity workers",
			mutate:  func(kv map[string]string) { kv[EnvActivityWorkers] = "0" },
			wantSub: "ACTIVITY_WORKERS must be at least 1",
		},
		{
			name:    "tau low above tau high",
			mutate:  func(kv map[string]string) { kv[EnvTauLow] = "0.95" },
			wantSub: "must not exceed",
		},
		{
			name:   "tau low equal to tau high is allowed",
			mutate: func(kv map[string]string) { kv[EnvTauLow] = "0.90" },
		},
		{
			name:    "tau high out of range",
			mutate:  func(kv map[string]string) { kv[EnvTauHigh] = "1.5" },
			wantSub: "between 0 and 1",
		},
		{
			name:    "tau low negative",
			mutate:  func(kv map[string]string) { kv[EnvTauLow] = "-0.1" },
			wantSub: "between 0 and 1",
		},
		{
			name:    "missing node id",
			mutate:  func(kv map[string]string) { delete(kv, EnvNodeID) },
			wantSub: "NODE_ID must be set",
		},
		{
			name:    "missing database url",
			mutate:  func(kv map[string]string) { delete(kv, EnvDatabaseURL) },
			wantSub: "DATABASE_URL must be set",
		},
		{
			// An unset EMBEDDER_URL is valid, not a misconfiguration: the
			// engine falls back to its built-in deterministic stand-in
			// encoder, which is what lets a node start with no model server.
			name:    "missing embedder url is allowed",
			mutate:  func(kv map[string]string) { delete(kv, EnvEmbedderURL) },
			wantSub: "",
		},
		{
			name:    "empty listen address",
			mutate:  func(kv map[string]string) { kv[EnvListenAddr] = "" },
			wantSub: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kv := valid()
			tc.mutate(kv)
			cfg, loadErr := LoadFrom(env(kv))
			if loadErr != nil {
				t.Fatalf("LoadFrom: %v", loadErr)
			}
			err := cfg.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// Fault injection may only be turned on by the environment. Setting the field
// directly must be rejected, otherwise a stray line of code could arm the
// injector in a real deployment.
func TestFaultInjectionCannotBeEnabledInCode(t *testing.T) {
	cfg, err := LoadFrom(env(valid()))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	cfg.FaultInjection = true

	verr := cfg.Validate()
	if verr == nil {
		t.Fatal("assigning FaultInjection=true was accepted")
	}
	if !strings.Contains(verr.Error(), "may only be thrown by explicit configuration") {
		t.Fatalf("unexpected error: %v", verr)
	}
}

func TestFaultInjectionExplicitlyOffIsValid(t *testing.T) {
	kv := valid()
	kv[EnvFaultInjection] = "false"
	cfg, err := LoadFrom(env(kv))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.FaultInjection {
		t.Fatal("FAULT_INJECTION=false enabled injection")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	kv := map[string]string{EnvShardCount: "0", EnvTauLow: "0.99"}
	cfg, err := LoadFrom(env(kv))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	verr := cfg.Validate()
	if verr == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"SHARD_COUNT", "TAU_LOW", "NODE_ID", "DATABASE_URL"} {
		if !strings.Contains(verr.Error(), want) {
			t.Errorf("combined error does not mention %s:\n%v", want, verr)
		}
	}
}

func TestStringRedactsThePassword(t *testing.T) {
	cfg, err := LoadFrom(env(valid()))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	s := cfg.String()
	if strings.Contains(s, "secret") {
		t.Fatalf("password leaked into the log line: %s", s)
	}
	if !strings.Contains(s, "durexec:****@db:5432") {
		t.Fatalf("redaction mangled the connection string: %s", s)
	}
}

// The escalation policy is a knob because the evaluation has to run the same
// workload under both policies. Defaulting it to "off" is the safety claim:
// an uncertain verdict never silently suppresses an action unless an operator
// asked for that.
func TestEscalationBlocksDefaultsToOff(t *testing.T) {
	cfg, err := LoadFrom(env(valid()))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.EscalationBlocks {
		t.Fatal("ESCALATION_BLOCKS must default to false")
	}

	kv := valid()
	kv[EnvEscalationBlock] = "on"
	cfg, err = LoadFrom(env(kv))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !cfg.EscalationBlocks {
		t.Fatalf("ESCALATION_BLOCKS=on did not take effect")
	}

	kv[EnvEscalationBlock] = "banana"
	if _, err := LoadFrom(env(kv)); err == nil {
		t.Fatal("expected a parse error for a non-boolean ESCALATION_BLOCKS")
	}
}

// A node with no embedder configured is a complete node, and it says so.
func TestSemanticIsFakeWhenNoEmbedderConfigured(t *testing.T) {
	kv := valid()
	delete(kv, EnvEmbedderURL)
	cfg, err := LoadFrom(env(kv))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !cfg.SemanticIsFake() {
		t.Fatal("expected SemanticIsFake with no EMBEDDER_URL")
	}
	if !strings.Contains(cfg.String(), "(built-in fake)") {
		t.Fatalf("startup line does not name the fake encoder: %s", cfg.String())
	}
}
