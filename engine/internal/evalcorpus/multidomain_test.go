package evalcorpus

import "testing"

// TestMultiDomainIsBalanced is the property every overall figure computed
// from this corpus depends on. If one domain contributes more pairs than
// another, an "overall" number is really a weighted average nobody asked for.
func TestMultiDomainIsBalanced(t *testing.T) {
	pairs, err := MultiDomain()
	if err != nil {
		t.Fatalf("MultiDomain: %v", err)
	}
	want := len(DomainOrder) * 2 * PairsPerClassPerDomain
	if len(pairs) != want {
		t.Errorf("corpus has %d pairs, want %d", len(pairs), want)
	}

	type counts struct{ dup, dis, hard int }
	byDomain := map[string]*counts{}
	for _, p := range pairs {
		c := byDomain[p.Domain]
		if c == nil {
			c = &counts{}
			byDomain[p.Domain] = c
		}
		if bool(p.Truth) {
			c.dup++
		} else {
			c.dis++
			if p.Hard {
				c.hard++
			}
		}
		if p.Activity == "" {
			t.Errorf("pair %s has no activity type", p.ID)
		}
	}
	for _, d := range DomainOrder {
		c := byDomain[d]
		if c == nil {
			t.Fatalf("domain %s contributed no pairs", d)
		}
		if c.dup != PairsPerClassPerDomain || c.dis != PairsPerClassPerDomain {
			t.Errorf("%s: %d duplicates and %d distincts, want %d of each",
				d, c.dup, c.dis, PairsPerClassPerDomain)
		}
		if c.hard == 0 {
			t.Errorf("%s has no hard negatives, so its hard-negative false-positive "+
				"rate would be computed over nothing", d)
		}
	}
	if len(byDomain) != len(DomainOrder) {
		t.Errorf("%d domains in the corpus, %d in DomainOrder", len(byDomain), len(DomainOrder))
	}
}

// TestMultiDomainPrefixIsBalanced is what makes a killed run usable: the
// first N pairs must have roughly the shape of the whole corpus, not the
// first domain's shape.
func TestMultiDomainPrefixIsBalanced(t *testing.T) {
	pairs, err := MultiDomain()
	if err != nil {
		t.Fatalf("MultiDomain: %v", err)
	}
	// One full round of the interleave is one pair from every domain.
	round := len(DomainOrder)
	for _, prefix := range []int{round, 5 * round, 10 * round} {
		seen := map[string]int{}
		for _, p := range pairs[:prefix] {
			seen[p.Domain]++
		}
		if len(seen) != len(DomainOrder) {
			t.Errorf("the first %d pairs cover %d of %d domains", prefix, len(seen), len(DomainOrder))
		}
		for d, n := range seen {
			if n != prefix/round {
				t.Errorf("the first %d pairs contain %d from %s, want %d", prefix, n, d, prefix/round)
			}
		}
	}
}

// TestFlightCorpusStillBuilds guards the move into this package: the 213
// pairs the published result files were measured on must be unchanged.
func TestFlightCorpusStillBuilds(t *testing.T) {
	pairs, err := Corpus()
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if len(pairs) != 213 {
		t.Errorf("the flight corpus has %d pairs; every published semantic result was "+
			"measured on 213 and a changed corpus makes them incomparable", len(pairs))
	}
	s := Summarize(pairs)
	if s.Duplicates != 98 || s.Distincts != 115 || s.HardDistincts != 91 {
		t.Errorf("flight corpus is now %d duplicates / %d distincts (%d hard); "+
			"published runs used 98 / 115 (91 hard)", s.Duplicates, s.Distincts, s.HardDistincts)
	}
	for _, p := range pairs {
		if p.Domain != DomainFlight || p.Activity == "" {
			t.Fatalf("pair %s is not stamped with its domain and activity", p.ID)
		}
	}
}
