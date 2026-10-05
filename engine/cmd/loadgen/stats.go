package main

import (
	"fmt"
	"sort"
	"time"
)

// Summary is the shape of a latency distribution.
type Summary struct {
	N    int
	Min  time.Duration
	P50  time.Duration
	P95  time.Duration
	P99  time.Duration
	Max  time.Duration
	Mean time.Duration
}

// Percentile returns the p-th percentile of samples, 0 <= p <= 100.
//
// It uses the nearest-rank definition — sort, take the ceil(p/100 * N)-th
// value — and never interpolates between two samples. Interpolation would
// invent a latency nothing actually experienced, and for a figure that goes
// into a report next to a pass/fail target, every printed number should be a
// measurement that really happened.
//
// samples is not modified; a sorted copy is taken.
func Percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentileSorted(sorted, p)
}

func percentileSorted(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	switch {
	case p <= 0:
		return sorted[0]
	case p >= 100:
		return sorted[len(sorted)-1]
	}
	// ceil without importing math, avoiding a float rounding surprise at
	// exact boundaries such as p=50 on an even count.
	rank := int(p / 100 * float64(len(sorted)))
	if float64(rank) < p/100*float64(len(sorted)) {
		rank++
	}
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Summarise computes the whole distribution in one sort.
func Summarise(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, s := range sorted {
		total += s
	}
	return Summary{
		N:    len(sorted),
		Min:  sorted[0],
		P50:  percentileSorted(sorted, 50),
		P95:  percentileSorted(sorted, 95),
		P99:  percentileSorted(sorted, 99),
		Max:  sorted[len(sorted)-1],
		Mean: total / time.Duration(len(sorted)),
	}
}

// percentileConfidence is the plain-language caveat that belongs next to any
// tail figure computed from a small sample.
//
// A p99 is the 99th of a hundred equally likely outcomes. With fewer than a
// hundred samples the "p99" is simply the largest one, which is a maximum
// wearing a percentile's name. Saying so in the output costs one line and
// stops a reader from treating three failovers as a tail distribution.
func percentileConfidence(n int) string {
	switch {
	case n == 0:
		return "no samples"
	case n < 5:
		return fmt.Sprintf("%d samples: an anecdote, not a distribution", n)
	case n < 100:
		return fmt.Sprintf("%d samples: p99 here is effectively the worst case observed, not a true 99th percentile", n)
	default:
		return fmt.Sprintf("%d samples", n)
	}
}

// fmtDur renders a duration at a fixed, readable precision.
func fmtDur(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.3fs", d.Seconds())
	}
}

// durSeconds renders a duration as plain seconds for the CSV, where a
// spreadsheet has to be able to treat it as a number.
func durSeconds(d time.Duration) string {
	return fmt.Sprintf("%.4f", d.Seconds())
}
