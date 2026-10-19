package llm

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestKeyringReadsEveryVariable pins which environment variables are picked
// up and in what order, because "key #2" in a message is only meaningful if
// the order is the same on every run.
func TestKeyringReadsEveryVariable(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "alpha")
	t.Setenv("GROQ_API_KEY_3", "gamma")
	t.Setenv("GROQ_API_KEY_2", "beta")
	t.Setenv("GROQ_MODEL", "not-a-key")

	kr := newKeyring()
	if kr.Len() != 3 {
		t.Fatalf("Len = %d, want 3", kr.Len())
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		idx, key, _, ok := kr.take(time.Now())
		if !ok {
			t.Fatalf("take %d: no key available", i)
		}
		if key != w {
			t.Errorf("take %d gave %q, want %q: GROQ_API_KEY must come first and the rest "+
				"must be sorted by variable name, or 'key #2' means a different key between runs",
				i, key, w)
		}
		if idx != i {
			t.Errorf("take %d reported index %d", i, idx)
		}
	}
}

// TestKeyringDropsDuplicates guards against counting one key twice and
// therefore pacing at twice the quota it really has.
func TestKeyringDropsDuplicates(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "same")
	t.Setenv("GROQ_API_KEY_2", "same")
	if n := newKeyring().Len(); n != 1 {
		t.Errorf("Len = %d, want 1: two variables holding one key is one key's quota", n)
	}
}

func TestKeyringIgnoresEmptyAndUnrelated(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("GROQ_API_KEY_2", "   ")
	t.Setenv("OPENAI_API_KEY", "nope")
	if n := newKeyring().Len(); n != 0 {
		t.Errorf("Len = %d, want 0", n)
	}
}

// TestKeyringRotatesRoundRobin is the behaviour the whole file exists for.
func TestKeyringRotatesRoundRobin(t *testing.T) {
	kr := &keyring{keys: []string{"a", "b", "c"}, coolUntil: make([]time.Time, 3)}
	var got []string
	for i := 0; i < 6; i++ {
		_, key, _, ok := kr.take(time.Now())
		if !ok {
			t.Fatalf("take %d: nothing available", i)
		}
		got = append(got, key)
	}
	if want := "a b c a b c"; strings.Join(got, " ") != want {
		t.Errorf("rotation was %q, want %q", strings.Join(got, " "), want)
	}
}

// TestKeyringSkipsACoolingKeyWithoutWaiting is the point of the design: a
// rate-limited key must cost the NEXT REQUEST, not a sleep.
func TestKeyringSkipsACoolingKeyWithoutWaiting(t *testing.T) {
	kr := &keyring{keys: []string{"a", "b"}, coolUntil: make([]time.Time, 2)}

	idx, key, _, ok := kr.take(time.Now())
	if !ok || key != "a" {
		t.Fatalf("first take gave %q, ok=%t", key, ok)
	}
	kr.penalise(idx, time.Minute)

	// "a" is cooling, so the next two takes must both be "b" and neither may
	// report a wait.
	for i := 0; i < 2; i++ {
		_, key, wait, ok := kr.take(time.Now())
		if !ok {
			t.Fatalf("take %d after penalising one of two keys: nothing available", i)
		}
		if key != "b" {
			t.Errorf("take %d gave %q, want the key that is not cooling", i, key)
		}
		if wait != 0 {
			t.Errorf("take %d reported a wait of %s while a key was free", i, wait)
		}
	}
}

// TestKeyringWaitsOnlyWhenEveryKeyIsCooling pins the other half: the caller
// sleeps once, and only until the FIRST key comes back.
func TestKeyringWaitsOnlyWhenEveryKeyIsCooling(t *testing.T) {
	kr := &keyring{keys: []string{"a", "b"}, coolUntil: make([]time.Time, 2)}
	kr.penalise(0, time.Minute)
	kr.penalise(1, 2*time.Second)

	_, _, wait, ok := kr.take(time.Now())
	if ok {
		t.Fatal("a key was handed out although both were cooling")
	}
	if wait <= 0 || wait > 2*time.Second {
		t.Errorf("wait = %s, want at most the 2s the sooner key was set aside for: waiting for "+
			"the LONGER one would idle through a usable quota", wait)
	}
}

// TestKeyringPenaltyNeverShortens stops a short penalty from cancelling a
// long one already in force.
func TestKeyringPenaltyNeverShortens(t *testing.T) {
	kr := &keyring{keys: []string{"a"}, coolUntil: make([]time.Time, 1)}
	kr.penalise(0, time.Minute)
	kr.penalise(0, time.Millisecond)
	if _, _, wait, ok := kr.take(time.Now()); ok || wait < 30*time.Second {
		t.Errorf("a one-millisecond penalty shortened a one-minute one (ok=%t, wait=%s)", ok, wait)
	}
}

// TestKeyringIsConcurrencySafe runs the race detector over the real access
// pattern: the extractor is called from whatever goroutine the engine's check
// is on, and a harness may have several in flight.
func TestKeyringIsConcurrencySafe(t *testing.T) {
	kr := &keyring{keys: []string{"a", "b", "c"}, coolUntil: make([]time.Time, 3)}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				idx, _, _, ok := kr.take(time.Now())
				if ok && j%7 == 0 {
					kr.penalise(idx, time.Millisecond)
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestLabelCannotLeakAKey is a one-line test for the rule that matters most.
func TestLabelCannotLeakAKey(t *testing.T) {
	secret := "gsk-this-must-never-appear"
	kr := newKeyringOf(secret)
	if got := kr.Label(0); strings.Contains(got, secret) || got != "key #1" {
		t.Errorf("Label(0) = %q; it must name a key by position only", got)
	}
}
