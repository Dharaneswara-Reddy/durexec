package main

import (
	"strings"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// TestPercentile pins the arithmetic on inputs whose answer can be worked out
// by hand, because a percentile that is quietly one rank out would shift every
// latency and recovery figure in the report by exactly enough to be wrong and
// not enough to be noticed.
func TestPercentile(t *testing.T) {
	// 1..100ms, already the easiest case to check: the p-th percentile of
	// 1..100 is p.
	hundred := make([]time.Duration, 0, 100)
	for i := 1; i <= 100; i++ {
		hundred = append(hundred, ms(i))
	}

	tests := []struct {
		name    string
		samples []time.Duration
		p       float64
		want    time.Duration
	}{
		{"empty is zero", nil, 50, 0},
		{"single sample is every percentile", []time.Duration{ms(7)}, 50, ms(7)},
		{"single sample at p99", []time.Duration{ms(7)}, 99, ms(7)},

		{"1..100 p50", hundred, 50, ms(50)},
		{"1..100 p95", hundred, 95, ms(95)},
		{"1..100 p99", hundred, 99, ms(99)},
		{"1..100 p100 is the max", hundred, 100, ms(100)},
		{"1..100 p0 is the min", hundred, 0, ms(1)},

		// Nearest rank on 10 samples: ceil(0.5*10)=5 -> the 5th value.
		{"ten samples p50 is the fifth", []time.Duration{ms(1), ms(2), ms(3), ms(4), ms(5), ms(6), ms(7), ms(8), ms(9), ms(10)}, 50, ms(5)},
		// ceil(0.9*10)=9 -> the 9th value.
		{"ten samples p90 is the ninth", []time.Duration{ms(1), ms(2), ms(3), ms(4), ms(5), ms(6), ms(7), ms(8), ms(9), ms(10)}, 90, ms(9)},
		// ceil(0.99*10)=10 -> with ten samples, p99 IS the maximum.
		{"ten samples p99 is the maximum", []time.Duration{ms(1), ms(2), ms(3), ms(4), ms(5), ms(6), ms(7), ms(8), ms(9), ms(10)}, 99, ms(10)},

		// ceil(0.5*3) = 2 -> the middle value of three.
		{"three samples p50 is the middle", []time.Duration{ms(1), ms(2), ms(3)}, 50, ms(2)},
		{"unsorted input is sorted first", []time.Duration{ms(9), ms(1), ms(5)}, 50, ms(5)},
		{"duplicates are kept", []time.Duration{ms(4), ms(4), ms(4), ms(4)}, 99, ms(4)},

		// Percentiles are never interpolated, so no value is invented.
		{"p75 of four is the third", []time.Duration{ms(10), ms(20), ms(30), ms(40)}, 75, ms(30)},
		{"p76 of four is the fourth", []time.Duration{ms(10), ms(20), ms(30), ms(40)}, 76, ms(40)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Percentile(tc.samples, tc.p); got != tc.want {
				t.Fatalf("Percentile(p%g) = %s, want %s", tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentileDoesNotMutateInput(t *testing.T) {
	in := []time.Duration{ms(9), ms(1), ms(5)}
	_ = Percentile(in, 50)
	if in[0] != ms(9) || in[1] != ms(1) || in[2] != ms(5) {
		t.Fatalf("input was reordered: %v", in)
	}
}

func TestSummarise(t *testing.T) {
	tests := []struct {
		name    string
		samples []time.Duration
		want    Summary
	}{
		{name: "empty", samples: nil, want: Summary{}},
		{
			name:    "one sample",
			samples: []time.Duration{ms(8)},
			want:    Summary{N: 1, Min: ms(8), P50: ms(8), P95: ms(8), P99: ms(8), Max: ms(8), Mean: ms(8)},
		},
		{
			name:    "four samples",
			samples: []time.Duration{ms(40), ms(10), ms(30), ms(20)},
			want:    Summary{N: 4, Min: ms(10), P50: ms(20), P95: ms(40), P99: ms(40), Max: ms(40), Mean: ms(25)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summarise(tc.samples); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPercentileConfidence checks the caveat text, which is the only thing
// standing between "p99 = 4.1s" over three failovers and a reader treating it
// as a tail distribution.
func TestPercentileConfidence(t *testing.T) {
	tests := []struct {
		n        int
		contains string
	}{
		{0, "no samples"},
		{1, "anecdote"},
		{4, "anecdote"},
		{5, "worst case observed"},
		{99, "worst case observed"},
		{100, "100 samples"},
	}
	for _, tc := range tests {
		got := percentileConfidence(tc.n)
		if !strings.Contains(got, tc.contains) {
			t.Errorf("percentileConfidence(%d) = %q, want it to mention %q", tc.n, got, tc.contains)
		}
	}
}

func TestFmtDur(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0"},
		{500 * time.Microsecond, "500us"},
		{12500 * time.Microsecond, "12.5ms"},
		{1500 * time.Millisecond, "1.500s"},
	}
	for _, tc := range tests {
		if got := fmtDur(tc.in); got != tc.want {
			t.Errorf("fmtDur(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDurSeconds(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.0000"},
		{1500 * time.Millisecond, "1.5000"},
		{6 * time.Second, "6.0000"},
		{time.Microsecond * 1234, "0.0012"},
	}
	for _, tc := range tests {
		if got := durSeconds(tc.in); got != tc.want {
			t.Errorf("durSeconds(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
