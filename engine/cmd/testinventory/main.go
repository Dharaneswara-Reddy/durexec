// testinventory reads `go test -json` on stdin and prints how many tests
// EXECUTED (passed or failed), were SKIPPED (with the reason each gave),
// FAILED, or are BLOCKED, per package and in total.
//
// Definitions, used the same way by the acceptance matrix:
//
//	PASS     ran, every assertion held
//	FAIL     ran, an assertion did not hold
//	SKIPPED  did not run because an optional input is absent (no -short, no
//	         database locally). Must carry a reason. Not a pass.
//	BLOCKED  does not run because something it needs has not been built. Its
//	         skip reason starts "BLOCKED-ON-<owner>:". It is listed loudly and
//	         never counted as a pass, but it does not fail -strict, because
//	         failing would only tell you what the report already says.
//
// The point is to make a silent skip visible. A test that was skipped has not
// passed. With -strict, the exit status is 1 if anything failed OR anything was
// skipped that is not on the deliberate-optional list below, so a missing
// database cannot hide behind a green build.
//
//	go test -json -p 4 ./... | go run ./cmd/testinventory -strict
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// optional lists skips that are deliberate and acceptable, keyed by a substring
// of the skip reason. Every entry needs a reason a reader can judge.
var optional = []struct{ match, why string }{
	{"-short", "timing-sensitive benchmark, skipped only when -short is passed"},
	{"with GateDecides off this pair scored", "data-dependent: the case only applies if the pair scores a given way"},
	{"deploy/docker-compose.yml not present", "acceptance test needs the repository checkout layout"},
}

type event struct {
	Action  string
	Package string
	Test    string
	Output  string
}

type pkgStat struct {
	pass, fail, skip, blocked int
}

func main() {
	strict := flag.Bool("strict", false, "exit 1 on any failure or any skip not on the optional list")
	flag.Parse()

	stats := map[string]*pkgStat{}
	outputs := map[string][]string{}
	type skipped struct{ pkg, test, reason string }
	var skips []skipped
	var blocks []skipped
	var failed []string
	buildFail := false

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		st := stats[e.Package]
		if st == nil && e.Package != "" {
			st = &pkgStat{}
			stats[e.Package] = st
		}
		key := e.Package + "/" + e.Test
		switch e.Action {
		case "output":
			if e.Test != "" {
				outputs[key] = append(outputs[key], e.Output)
			}
		case "pass", "fail", "skip":
			if e.Test == "" {
				if e.Action == "fail" && st != nil && st.fail == 0 && st.pass == 0 && st.skip == 0 {
					buildFail = true // package failed without running a test
				}
				continue
			}
			switch e.Action {
			case "pass":
				st.pass++
			case "fail":
				st.fail++
				failed = append(failed, key)
			case "skip":
				r := reason(outputs[key])
				if strings.Contains(r, "BLOCKED-ON-") {
					st.blocked++
					blocks = append(blocks, skipped{e.Package, e.Test, r})
					continue
				}
				st.skip++
				skips = append(skips, skipped{e.Package, e.Test, r})
			}
		}
	}

	var pkgs []string
	for p := range stats {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	var tp, tf, ts, tb int
	fmt.Printf("%-60s %7s %7s %7s %7s\n", "package", "passed", "failed", "skipped", "blocked")
	for _, p := range pkgs {
		s := stats[p]
		tp, tf, ts, tb = tp+s.pass, tf+s.fail, ts+s.skip, tb+s.blocked
		if s.pass+s.fail+s.skip+s.blocked == 0 {
			continue
		}
		fmt.Printf("%-60s %7d %7d %7d %7d\n", strings.TrimPrefix(p, "github.com/dharaneswar339/durexec/"), s.pass, s.fail, s.skip, s.blocked)
	}
	fmt.Printf("%-60s %7d %7d %7d %7d\n", "TOTAL", tp, tf, ts, tb)

	bad := 0
	if len(skips) > 0 {
		fmt.Println("\nSKIPPED tests (a skipped test has not passed):")
		for _, s := range skips {
			why, ok := allowed(s.reason)
			tag := "UNEXPECTED"
			if !*strict {
				tag = "not run   "
			}
			if ok {
				tag = "optional  "
			} else {
				bad++
			}
			fmt.Printf("  [%s] %s %s\n      reason: %s\n", tag, strings.TrimPrefix(s.pkg, "github.com/dharaneswar339/durexec/"), s.test, s.reason)
			if ok {
				fmt.Printf("      allowed because: %s\n", why)
			}
		}
	}
	if len(blocks) > 0 {
		fmt.Println("\nBLOCKED tests (written, cannot run yet; NOT passing):")
		for _, b := range blocks {
			fmt.Printf("  %s %s\n      %s\n", strings.TrimPrefix(b.pkg, "github.com/dharaneswar339/durexec/"), b.test, b.reason)
		}
	}
	for _, f := range failed {
		fmt.Println("FAILED:", f)
	}
	if buildFail {
		fmt.Println("BLOCKED: at least one package failed to build or crashed before running a test")
	}
	if tf > 0 || buildFail || (*strict && bad > 0) {
		if *strict && bad > 0 {
			fmt.Printf("\nstrict: %d unexpected skip(s). Required tests must run.\n", bad)
		}
		os.Exit(1)
	}
}

func reason(lines []string) string {
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "=== ") || strings.HasPrefix(l, "--- ") {
			continue
		}
		return l
	}
	return "(no reason given)"
}

func allowed(reason string) (string, bool) {
	for _, o := range optional {
		if strings.Contains(reason, o.match) {
			return o.why, true
		}
	}
	return "", false
}
