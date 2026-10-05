package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// A small reader for the Prometheus text exposition format.
//
// The engine already depends on the Prometheus client library, but the client
// library parses what it wrote; this tool has to read what a node is actually
// serving over HTTP, as an outside observer would. Parsing the text is a few
// dozen lines and keeps the acceptance tool honest about only using the public
// surface.

// Sample is one time series value with its labels.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Family is every sample sharing a metric name.
type Family struct {
	Name    string
	Help    string
	Type    string
	Samples []Sample
}

// MetricSet is a parsed /metrics page.
type MetricSet map[string]*Family

// ParseMetrics reads the exposition text. Malformed lines are skipped rather
// than failing the whole page: a metric this tool does not understand must not
// stop it reading the ones it does.
func ParseMetrics(text string) MetricSet {
	out := MetricSet{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			name := fields[2]
			f := out.ensure(name)
			switch fields[1] {
			case "HELP":
				f.Help = strings.Join(fields[3:], " ")
			case "TYPE":
				if len(fields) >= 4 {
					f.Type = fields[3]
				}
			}
			continue
		}

		name, labels, value, ok := parseSampleLine(line)
		if !ok {
			continue
		}
		f := out.ensure(name)
		f.Samples = append(f.Samples, Sample{Labels: labels, Value: value})
	}
	return out
}

func (m MetricSet) ensure(name string) *Family {
	f, ok := m[name]
	if !ok {
		f = &Family{Name: name}
		m[name] = f
	}
	return f
}

// Has reports whether a metric family was exported at all. "The engine exposes
// every metric the design names" is a presence question first.
func (m MetricSet) Has(name string) bool {
	_, ok := m[name]
	return ok
}

// Names returns every exported metric name, sorted.
func (m MetricSet) Names() []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Sum adds every sample in a family, which is what a counter across label sets
// means when the question is "did this ever happen".
func (m MetricSet) Sum(name string) (float64, bool) {
	f, ok := m[name]
	if !ok {
		return 0, false
	}
	var total float64
	for _, s := range f.Samples {
		total += s.Value
	}
	return total, true
}

func parseSampleLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	labels = map[string]string{}

	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", nil, 0, false
		}
		name = line[:i]
		labels = parseLabels(line[i+1 : j])
		line = strings.TrimSpace(line[j+1:])
	} else {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return "", nil, 0, false
		}
		name = fields[0]
		line = strings.Join(fields[1:], " ")
	}

	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil, 0, false
	}
	v, err := parseFloat(fields[0])
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

// parseLabels splits a label set. Values may contain commas and escaped
// quotes, so it walks the string rather than splitting on ",".
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			break
		}
		i++
		var val strings.Builder
		for i < len(s) {
			if s[i] == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			if s[i] == '"' {
				i++
				break
			}
			val.WriteByte(s[i])
			i++
		}
		out[key] = val.String()
		for i < len(s) && (s[i] == ',' || s[i] == ' ') {
			i++
		}
	}
	return out
}

func parseFloat(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// ---- histograms ------------------------------------------------------------

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	LE    float64
	Count float64
}

// Histogram is one histogram family, summed across label sets.
type Histogram struct {
	Name    string
	Count   float64
	Sum     float64
	Buckets []Bucket
}

// ReadHistogram assembles name_bucket, name_sum and name_count into one
// histogram. ok is false when the family was not exported at all, which is a
// different answer from "exported but never observed" and the checks treat it
// as such: a missing metric is a design claim unmet, an empty one is simply a
// measurement nobody has made yet.
func (m MetricSet) ReadHistogram(name string) (Histogram, bool) {
	bucketFam, hasBuckets := m[name+"_bucket"]
	countFam, hasCount := m[name+"_count"]
	if !hasBuckets && !hasCount {
		return Histogram{}, false
	}

	h := Histogram{Name: name}
	if hasCount {
		for _, s := range countFam.Samples {
			h.Count += s.Value
		}
	}
	if sumFam, ok := m[name+"_sum"]; ok {
		for _, s := range sumFam.Samples {
			h.Sum += s.Value
		}
	}
	if hasBuckets {
		byLE := map[float64]float64{}
		for _, s := range bucketFam.Samples {
			le, err := parseFloat(s.Labels["le"])
			if err != nil {
				continue
			}
			byLE[le] += s.Value
		}
		for le, c := range byLE {
			h.Buckets = append(h.Buckets, Bucket{LE: le, Count: c})
		}
		sort.Slice(h.Buckets, func(i, j int) bool { return h.Buckets[i].LE < h.Buckets[j].LE })
	}
	return h, true
}

// Quantile estimates a quantile the way Prometheus does: find the bucket the
// rank falls in and interpolate linearly inside it.
//
// The estimate is only as good as the bucket boundaries, so Quantile also
// reports the width of the bucket it landed in. A p99 of "12 s" taken from a
// bucket spanning 8 s to 16 s is a different kind of number from one taken
// from a bucket spanning 0.1 s, and a report that hides that is overstating
// its own precision.
func (h Histogram) Quantile(q float64) (value float64, bucketLow, bucketHigh float64, ok bool) {
	if h.Count <= 0 || len(h.Buckets) == 0 {
		return 0, 0, 0, false
	}
	rank := q * h.Count
	prevLE, prevCount := 0.0, 0.0
	for _, b := range h.Buckets {
		if b.Count >= rank {
			if math.IsInf(b.LE, 1) {
				// Everything above the last finite bucket. The only honest
				// statement is a lower bound.
				return prevLE, prevLE, math.Inf(1), true
			}
			if b.Count == prevCount {
				return b.LE, prevLE, b.LE, true
			}
			frac := (rank - prevCount) / (b.Count - prevCount)
			return prevLE + frac*(b.LE-prevLE), prevLE, b.LE, true
		}
		prevLE, prevCount = b.LE, b.Count
	}
	return prevLE, prevLE, prevLE, true
}

// Max reports the highest finite bucket boundary that still contains
// observations, and whether anything overflowed past the last finite bucket.
func (h Histogram) Max() (upperBound float64, overflowed bool) {
	var lastFinite float64
	var lastFiniteCount float64
	for _, b := range h.Buckets {
		if math.IsInf(b.LE, 1) {
			continue
		}
		lastFinite = b.LE
		lastFiniteCount = b.Count
	}
	return lastFinite, lastFiniteCount < h.Count
}

// Mean is the arithmetic mean of the observations, from _sum and _count.
func (h Histogram) Mean() float64 {
	if h.Count == 0 {
		return 0
	}
	return h.Sum / h.Count
}
