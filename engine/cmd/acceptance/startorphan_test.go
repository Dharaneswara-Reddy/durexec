package main

import "testing"

// The start-path orphan: an execution the API accepted, then abandoned before
// its first event. C-11 must FAIL on one, and NFR-1 must refuse to stay
// "MANUAL" while accepted work is lost.
func TestStartOrphanFailsC11AndNFR1(t *testing.T) {
	cases := []struct {
		name    string
		orphans []Orphan
		c11     Outcome
		nfr1    Outcome
	}{
		{"none", nil, Pass, Manual},
		{"old and never journaled", []Orphan{{ShardID: 3, WorkflowID: "lost", Status: "RUNNING", LastSeq: 0, AgeSeconds: 120}}, Fail, Fail},
		{"still being started (young)", []Orphan{{ShardID: 3, WorkflowID: "new", Status: "RUNNING", LastSeq: 0, AgeSeconds: 2}}, Pass, Manual},
		{"waiting on a signal has events, so it is not an orphan", []Orphan{{ShardID: 3, WorkflowID: "waiter", Status: "RUNNING", LastSeq: 9, AgeSeconds: 900}}, Pass, Manual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := withEvents(cleanRun("wf-a"))
			ev.Orphans = tc.orphans
			rec := NewRecorder(AllRows())
			Evaluate(rec, ev)
			if got := rec.Get("C-11").Outcome; got != tc.c11 {
				t.Errorf("C-11 = %s, want %s (%s)", got, tc.c11, rec.Get("C-11").Observed)
			}
			if got := rec.Get("NFR-1").Outcome; got != tc.nfr1 {
				t.Errorf("NFR-1 = %s, want %s (%s)", got, tc.nfr1, rec.Get("NFR-1").Observed)
			}
			if tc.c11 == Fail && rec.ExitCode() == 0 {
				t.Errorf("a FAIL row must make the tool exit non-zero")
			}
		})
	}
}

func TestC11IsBlockedWithoutADatabase(t *testing.T) {
	rec := NewRecorder(AllRows())
	Evaluate(rec, newEvidence(Options{}))
	if got := rec.Get("C-11").Outcome; got != Blocked {
		t.Fatalf("C-11 with no database = %s, want BLOCKED", got)
	}
}
