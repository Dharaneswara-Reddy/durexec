// Command genericeval scores model-free duplicate comparators on the
// evaluation corpora: the flight corpus, the ten-domain development corpus,
// the ten-domain HELD-OUT corpus and the real rewordings of the live
// crash-and-replan run.
//
// It measures only comparators that need no embedding model, so it runs
// anywhere, deterministically, in a second:
//
//	exact           canonical text equality (the engine's exact key)
//	generic-v1      generic.go's value-shape fields, neutral gate, gate decides
//	                alone (pairs it cannot read are reported as FALLBACK, since
//	                deciding them needs the model)
//	generic-v2      generic2.go's pairwise comparison
//
// -corpora picks the corpora. The held-out corpus is excluded from the
// default on purpose: it is to be measured once, after the comparator under
// test is frozen.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/dharaneswar339/durexec/internal/core"
	"github.com/dharaneswar339/durexec/internal/evalcorpus"
	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

type pair struct {
	corpus, domain, activity, id, a, b string
	dup, hard                          bool
}

type row struct {
	pair
	arm, verdict, reason string
}

func main() {
	corpora := flag.String("corpora", "flight,multidomain,rephrase", "comma list: flight, multidomain, heldout, rephrase")
	rephraseCSV := flag.String("rephrase-csv", "../docs/results/rephrase-live-model.csv", "live rewording trials")
	out := flag.String("csv", "", "write one row per arm per pair here")
	show := flag.String("show", "", "print the rows of this arm whose verdict is wrong (FP or FN)")
	flag.Parse()

	var pairs []pair
	for _, c := range strings.Split(*corpora, ",") {
		ps, err := load(strings.TrimSpace(c), *rephraseCSV)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		pairs = append(pairs, ps...)
	}

	arms := []string{"exact", "generic-v1-gateonly", "generic-v2", "generic-v2+places"}
	var rows []row
	for _, p := range pairs {
		for _, arm := range arms {
			v, why := decide(arm, p)
			rows = append(rows, row{pair: p, arm: arm, verdict: v, reason: why})
		}
	}
	report(os.Stdout, rows, arms)
	if *show != "" {
		for _, r := range rows {
			if r.arm != *show {
				continue
			}
			wrong := (r.dup && r.verdict != string(core.VerdictDuplicate)) || (!r.dup && r.verdict == string(core.VerdictDuplicate))
			if wrong {
				fmt.Printf("%-5s %-12s %-9s | %s\n      A: %s\n      B: %s\n", label(r.dup), r.domain, r.verdict, r.reason, r.a, r.b)
			}
		}
	}
	if *out != "" {
		if err := writeCSV(*out, rows); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func label(dup bool) string {
	if dup {
		return "DUP"
	}
	return "DIST"
}

func load(name, rephraseCSV string) ([]pair, error) {
	conv := func(corpus string, ps []evalcorpus.Pair) []pair {
		out := make([]pair, 0, len(ps))
		for _, p := range ps {
			out = append(out, pair{corpus: corpus, domain: p.Domain, activity: p.Activity, id: p.ID,
				a: p.A, b: p.B, dup: p.Truth == evalcorpus.Duplicate, hard: p.Hard})
		}
		return out
	}
	switch name {
	case "flight":
		ps, err := evalcorpus.Corpus()
		for i := range ps {
			if ps[i].Domain == "" {
				ps[i].Domain = "flight"
			}
			if ps[i].Activity == "" {
				ps[i].Activity = "BookFlight"
			}
		}
		return conv("flight-213", ps), err
	case "multidomain":
		ps, err := evalcorpus.MultiDomain()
		return conv("multidomain-300", ps), err
	case "heldout":
		ps, err := evalcorpus.HeldOut()
		return conv("heldout-300", ps), err
	case "heldout2":
		ps, err := evalcorpus.HeldOut2()
		return conv("heldout2-300", ps), err
	case "rephrase":
		return loadRephrase(rephraseCSV)
	}
	return nil, fmt.Errorf("unknown corpus %q", name)
}

// loadRephrase keeps the trials cmd/fieldeval kept: the lost-journal arm,
// usable outcome, a genuine rewording (not byte-identical) that did not
// drift to a different action. Every kept pair is a duplicate.
func loadRephrase(path string) ([]pair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	var out []pair
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		g := func(k string) string { return rec[col[k]] }
		if g("arm") != "lost-journal" || g("outcome") != "usable" || g("byte_identical") == "true" || g("drifted") == "true" {
			continue
		}
		out = append(out, pair{corpus: "rephrase-live", domain: g("domain"), activity: g("commit_tool"),
			id: g("trial_key"), a: g("before_args"), b: g("after_args"), dup: true})
	}
	return out, nil
}

func canon(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func decide(arm string, p pair) (string, string) {
	switch arm {
	case "exact":
		if canon(p.a) == canon(p.b) {
			return string(core.VerdictDuplicate), "identical text"
		}
		return string(core.VerdictDistinct), "text differs"
	case "generic-v1-gateonly":
		fa, _ := semantic.GenericFields(p.activity, canon(p.a))
		fb, _ := semantic.GenericFields(p.activity, canon(p.b))
		if !fa.Recognised || !fb.Recognised {
			return "FALLBACK", "unreadable: needs the model"
		}
		if name, x, y, bad := semantic.FirstConflict(fa, fb); bad {
			return string(core.VerdictDistinct), fmt.Sprintf("%s %s vs %s", name, x, y)
		}
		if semantic.CountAgreeing(fa, fb) < 1 {
			return string(core.VerdictDistinct), "no agreeing field"
		}
		return string(core.VerdictDuplicate), fa.Signature()
	case "generic-v2":
		v := semantic.GenericV2Compare(p.activity, canon(p.a), canon(p.b))
		return string(v.Verdict), v.Reason
	case "generic-v2+places":
		v := semantic.GenericV2PlacesCompare(p.activity, canon(p.a), canon(p.b))
		return string(v.Verdict), v.Reason
	}
	return "", ""
}

type tally struct{ dup, caught, dupEsc, dist, wrong, distEsc, hardDist, hardWrong, fallback int }

func (t *tally) add(r row) {
	switch {
	case r.verdict == "FALLBACK":
		t.fallback++
	}
	if r.dup {
		t.dup++
		if r.verdict == string(core.VerdictDuplicate) {
			t.caught++
		}
		if r.verdict == string(core.VerdictEscalate) {
			t.dupEsc++
		}
		return
	}
	t.dist++
	if r.hard {
		t.hardDist++
	}
	if r.verdict == string(core.VerdictDuplicate) {
		t.wrong++
		if r.hard {
			t.hardWrong++
		}
	}
	if r.verdict == string(core.VerdictEscalate) {
		t.distEsc++
	}
}

func report(w io.Writer, rows []row, arms []string) {
	byCorpus := map[string]map[string]*tally{}
	byDomain := map[string]map[string]map[string]*tally{}
	var corpora []string
	for _, r := range rows {
		if byCorpus[r.corpus] == nil {
			byCorpus[r.corpus] = map[string]*tally{}
			byDomain[r.corpus] = map[string]map[string]*tally{}
			corpora = append(corpora, r.corpus)
		}
		if byCorpus[r.corpus][r.arm] == nil {
			byCorpus[r.corpus][r.arm] = &tally{}
		}
		byCorpus[r.corpus][r.arm].add(r)
		if byDomain[r.corpus][r.domain] == nil {
			byDomain[r.corpus][r.domain] = map[string]*tally{}
		}
		if byDomain[r.corpus][r.domain][r.arm] == nil {
			byDomain[r.corpus][r.domain][r.arm] = &tally{}
		}
		byDomain[r.corpus][r.domain][r.arm].add(r)
	}
	for _, c := range corpora {
		fmt.Fprintf(w, "\n== %s\n", c)
		fmt.Fprintf(w, "  %-22s %-16s %-16s %-14s %-10s %s\n", "arm", "caught", "wrongly blocked", "hard blocked", "escalated", "fallback")
		for _, a := range arms {
			t := byCorpus[c][a]
			fmt.Fprintf(w, "  %-22s %3d/%-3d %5.1f%%  %3d/%-3d %5.1f%%  %3d/%-3d       %3d+%-3d    %d\n", a,
				t.caught, t.dup, pct(t.caught, t.dup), t.wrong, t.dist, pct(t.wrong, t.dist),
				t.hardWrong, t.hardDist, t.dupEsc, t.distEsc, t.fallback)
		}
		var doms []string
		for d := range byDomain[c] {
			doms = append(doms, d)
		}
		sort.Strings(doms)
		if len(doms) > 1 {
			fmt.Fprintf(w, "  per domain (caught/dup, wrong/dist):\n")
			for _, a := range arms {
				fmt.Fprintf(w, "    %-20s", a)
				for _, d := range doms {
					t := byDomain[c][d][a]
					if t.dist > 0 {
						fmt.Fprintf(w, " %s %d/%d,%d/%d", short(d), t.caught, t.dup, t.wrong, t.dist)
					} else {
						fmt.Fprintf(w, " %s %d/%d", short(d), t.caught, t.dup)
					}
				}
				fmt.Fprintln(w)
			}
		}
	}
}

func short(d string) string {
	if len(d) > 6 {
		return d[:6]
	}
	return d
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func writeCSV(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"corpus", "domain", "activity", "pair_id", "truth", "hard", "arm", "verdict", "reason", "a", "b"})
	for _, r := range rows {
		_ = w.Write([]string{r.corpus, r.domain, r.activity, r.id, label(r.dup), fmt.Sprint(r.hard), r.arm, r.verdict, r.reason, r.a, r.b})
	}
	w.Flush()
	return w.Error()
}
