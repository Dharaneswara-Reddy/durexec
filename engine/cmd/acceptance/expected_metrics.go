package main

import (
	"reflect"
	"regexp"
	"sort"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dharaneswar339/durexec/internal/obs"
)

// ExpectedMetricNames returns every metric name the engine's own
// observability package defines.
//
// The list is taken from internal/obs rather than typed out here, and that is
// the whole point. A hand-written list in an acceptance tool checks that the
// cluster exports the names the tool's author remembered, which is a test of
// the author. Taking them from obs.Metrics means the check is "the running
// node exports everything the engine says it has" — and if somebody adds a
// collector to obs and forgets to register it, or registers it and the node
// never serves it, this check notices without anyone editing this file.
//
// The names are recovered through Describe rather than through Gather because
// a freshly built registry has no samples in it: a CounterVec with no label
// values yet produces no metric family at all, so Gather would report an empty
// engine. Describe reports what a collector *can* produce, which is the right
// question for "does the node expose this metric".
func ExpectedMetricNames() []string {
	m := obs.New(prometheus.NewRegistry())

	seen := map[string]bool{}
	v := reflect.ValueOf(m).Elem()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if !field.CanInterface() {
			continue
		}
		collector, ok := field.Interface().(prometheus.Collector)
		if !ok || collector == nil {
			continue
		}
		for _, name := range describeNames(collector) {
			seen[name] = true
		}
	}

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// fqNamePattern pulls the metric name out of a prometheus.Desc. Desc keeps its
// fqName unexported and offers no accessor, so its String form is the only
// public way to read it.
var fqNamePattern = regexp.MustCompile(`fqName:\s*"([^"]+)"`)

func describeNames(c prometheus.Collector) []string {
	ch := make(chan *prometheus.Desc, 16)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	var out []string
	for desc := range ch {
		if match := fqNamePattern.FindStringSubmatch(desc.String()); len(match) == 2 {
			out = append(out, match[1])
		}
	}
	return out
}

// MissingMetrics returns the expected names a node did not serve.
//
// A histogram is exported as three families — _bucket, _sum and _count — and
// never under its bare name, so its bare name is matched against the suffixed
// forms as well.
func MissingMetrics(expected []string, served MetricSet) []string {
	var missing []string
	for _, name := range expected {
		if served.Has(name) ||
			served.Has(name+"_count") ||
			served.Has(name+"_bucket") ||
			served.Has(name+"_total") {
			continue
		}
		missing = append(missing, name)
	}
	return missing
}
