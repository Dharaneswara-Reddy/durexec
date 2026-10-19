package main

import "github.com/dharaneswar339/durexec/internal/evalcorpus"

// The corpus moved to internal/evalcorpus so that cmd/fieldeval can score the
// identical pairs. These aliases keep every reference in this command, and in
// its tests, spelled the way it always was: a moved corpus must not be able to
// change what this harness measures, and a rename sweep across a thousand
// lines of harness would be a change nobody could verify by reading the diff.
//
// They are aliases, not wrappers. `Pair` here IS evalcorpus.Pair, so a pair
// built by one harness is the same type the other scores, and the compiler
// enforces that rather than a convention.
type (
	Pair          = evalcorpus.Pair
	Label         = evalcorpus.Label
	CorpusSummary = evalcorpus.CorpusSummary
)

const (
	Duplicate = evalcorpus.Duplicate
	Distinct  = evalcorpus.Distinct

	// MinNearMissOverlap is asserted on by this command's tests, which is
	// the reason it is re-exported rather than left behind.
	MinNearMissOverlap = evalcorpus.MinNearMissOverlap
)

var (
	// Corpus is the flight corpus: the same 213 pairs the published result
	// files were measured on.
	Corpus = evalcorpus.Corpus

	Summarize = evalcorpus.Summarize
)
