package config

import (
	"strings"
	"testing"
)

// faultEnv is valid() plus the fault variables under test, so these tests
// fail on the crash-arming rules and never on an unrelated missing setting.
func faultEnv(extra map[string]string) func(string) (string, bool) {
	kv := valid()
	for k, v := range extra {
		kv[k] = v
	}
	return env(kv)
}

// TestFaultCrashAtRequiresExplicitInjection is the security regression for
// F9-2. FAULT_CRASH_AT must not become a second, quieter way to arm a
// deliberate failure: a process that was never told to inject faults must
// refuse to start rather than start with a boundary armed.
func TestFaultCrashAtRequiresExplicitInjection(t *testing.T) {
	cfg, err := LoadFrom(faultEnv(map[string]string{
		EnvFaultCrashAt: "BeforeStartCommit",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	verr := cfg.Validate()
	if verr == nil {
		t.Fatal("a crash boundary was accepted with fault injection off")
	}
	if !strings.Contains(verr.Error(), EnvFaultInjection) {
		t.Fatalf("the error does not explain the missing %s: %v", EnvFaultInjection, verr)
	}
}

// TestFaultCrashAtRejectsUnwiredPoint guards against the failure mode that
// matters most to a report: a run configured to crash at a seam nothing
// checks would complete cleanly, and the clean result would be written up as
// the boundary having been tested and survived.
func TestFaultCrashAtRejectsUnwiredPoint(t *testing.T) {
	for _, bad := range []string{"AfterLeaseRenew", "BeforeStartCommitt", "nonsense"} {
		cfg, err := LoadFrom(faultEnv(map[string]string{
			EnvFaultInjection: "true",
			EnvFaultCrashAt:   bad,
		}))
		if err != nil {
			t.Fatalf("load %q: %v", bad, err)
		}
		if cfg.Validate() == nil {
			t.Fatalf("%s=%q was accepted but nothing checks that point", EnvFaultCrashAt, bad)
		}
	}
}

// TestFaultCrashAtAcceptedWhenArmedProperly is the positive case: with the
// injector explicitly on and a wired point named, the engine starts.
func TestFaultCrashAtAcceptedWhenArmedProperly(t *testing.T) {
	cfg, err := LoadFrom(faultEnv(map[string]string{
		EnvFaultInjection: "true",
		EnvFaultCrashAt:   "BeforeStartCommit",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if verr := cfg.Validate(); verr != nil {
		t.Fatalf("a properly armed crash boundary was refused: %v", verr)
	}
	if cfg.FaultCrashAt != "BeforeStartCommit" {
		t.Fatalf("FaultCrashAt = %q", cfg.FaultCrashAt)
	}
}

// TestNoCrashArmedByDefault is the default-off assertion: an environment that
// says nothing about faults must produce a config with nothing armed.
func TestNoCrashArmedByDefault(t *testing.T) {
	cfg, err := LoadFrom(faultEnv(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.FaultCrashAt != "" {
		t.Fatalf("FaultCrashAt defaulted to %q", cfg.FaultCrashAt)
	}
	if cfg.FaultInjection {
		t.Fatal("FaultInjection defaulted to true")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a default config was refused: %v", err)
	}
}

// TestExtsimURLValidation guards the test-only external-effect path. A bad URL
// must be refused at start-up, because the external-effect activity would
// otherwise fail at call time with a transport error — and the retry policy
// reads a transport error as the AMBIGUOUS outcome, meaning "the effect may or
// may not have happened". That is the one verdict a configuration typo must
// never be able to manufacture.
func TestExtsimURLValidation(t *testing.T) {
	for _, bad := range []string{
		"not a url at all",
		"localhost:8090",  // no scheme: url.Parse accepts it, host ends up empty
		"ftp://host:8090", // wrong scheme
		"http://",         // no host
	} {
		cfg, err := LoadFrom(faultEnv(map[string]string{EnvExtsimURL: bad}))
		if err != nil {
			t.Fatalf("load %q: %v", bad, err)
		}
		if cfg.Validate() == nil {
			t.Fatalf("%s=%q was accepted", EnvExtsimURL, bad)
		}
	}

	for _, good := range []string{
		"http://extsim.durexec-lab.svc.cluster.local:8090",
		"https://example.test",
	} {
		cfg, err := LoadFrom(faultEnv(map[string]string{EnvExtsimURL: good}))
		if err != nil {
			t.Fatalf("load %q: %v", good, err)
		}
		if verr := cfg.Validate(); verr != nil {
			t.Fatalf("%s=%q was refused: %v", EnvExtsimURL, good, verr)
		}
	}
}

// TestNoExtsimByDefault is the default-off assertion: an environment that says
// nothing about a simulator must not configure one, so neither the
// ExternalPayment activity nor the probe workflow can exist.
func TestNoExtsimByDefault(t *testing.T) {
	cfg, err := LoadFrom(faultEnv(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ExtsimURL != "" {
		t.Fatalf("ExtsimURL defaulted to %q", cfg.ExtsimURL)
	}
}
