package semantic

import "math"

// Cosine is the similarity between two embedding vectors: 1 means they point
// in exactly the same direction, 0 means they are unrelated, and negative
// means they point apart.
//
// It compares direction only, not length. That is what we want here — the
// question is "do these two intents mean the same thing?", not "how long did
// the model make the vector?".
//
// A zero-magnitude vector returns 0, not NaN. The textbook formula divides by
// the product of the two lengths, and a zero vector makes that a division by
// zero, which in floating point yields NaN. NaN is poison in this code path:
// every comparison against a NaN is false, so a NaN score would slip past the
// threshold test and land in whichever branch happens to be last. Returning 0
// makes an empty or degenerate vector read as "not similar", which routes it
// to VerdictDistinct — the safe direction, since a distinct verdict merely
// runs the activity again rather than skipping it.
//
// Vectors of different lengths are also 0. Mismatched dimensions mean the two
// vectors came from different models, so no comparison between them is
// meaningful.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// Rounding can push an identical pair a hair past 1.0. Clamping keeps the
	// score inside the range the thresholds were calibrated on.
	if sim > 1 {
		return 1
	}
	if sim < -1 {
		return -1
	}
	return sim
}

// BestMatch scans candidates and returns the index of the most similar one
// together with its score, or (-1, 0) when there is nothing to compare with.
//
// This is a brute-force linear scan, deliberately. The candidate set here is
// every prior completed invocation of the *same activity type* in the *same
// run* — tens of rows, not millions. It is bounded by how long one workflow's
// history is, not by how much data the system holds in total.
//
// That bound is the architectural advantage this design has over a semantic
// cache. A semantic cache searches a single global corpus that grows without
// limit, so it is forced to adopt an approximate nearest-neighbour index,
// which brings index build cost, recall that is under 100% by construction,
// and a background rebuild that can silently change answers over time. Here,
// scanning every candidate is cheap and exhaustive, so the match is exactly
// reproducible from the same rows. No ANN index is warranted, and adding one
// would trade away the very determinism the replay engine depends on.
func BestMatch(query []float32, candidates [][]float32) (int, float64) {
	best, bestSim := -1, 0.0
	for i, c := range candidates {
		sim := Cosine(query, c)
		if best == -1 || sim > bestSim {
			best, bestSim = i, sim
		}
	}
	if best == -1 {
		return -1, 0
	}
	return best, bestSim
}
