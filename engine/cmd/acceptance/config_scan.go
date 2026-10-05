package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The configuration half of NFR-10 and the memory half of NFR-9.
//
// This tool may not run docker, so it cannot ask the container runtime what is
// actually running. What it can read is the compose file `make up` starts the
// stack from, and the go.mod the engine binary is built from. Both are read as
// plain text with deliberately small parsers: the question is only "does any
// coordination product appear anywhere in here", and a full YAML library would
// be a dependency added to answer a string search.

// ComposeService is one service block in the compose file.
type ComposeService struct {
	Name  string
	Image string
	// MemLimitMB is the service's own mem_limit, or the one it inherits from a
	// merged anchor (`<<: *engine-common`). Zero means no limit was found.
	MemLimitMB int
}

// ComposeScan is what the compose file declares.
type ComposeScan struct {
	Path     string
	Err      string
	Read     bool
	Services []ComposeService
	// Coordination lists every denylisted product name found anywhere in the
	// file's non-comment text, with its line number.
	Coordination []string
	// URLEnv is every environment key ending in _URL, which is how the engine
	// is told about anything it calls over the network.
	URLEnv   map[string]string
	LeaseTTL string
}

// ModScan is what go.mod says the engine binary links against.
type ModScan struct {
	Path         string
	Err          string
	Read         bool
	Modules      []string
	Coordination []string
}

func ScanCompose(path string) ComposeScan {
	b, err := os.ReadFile(path)
	if err != nil {
		return ComposeScan{Path: path, Err: err.Error()}
	}
	s := parseCompose(string(b))
	s.Path = path
	return s
}

func ScanGoMod(path string) ModScan {
	b, err := os.ReadFile(path)
	if err != nil {
		return ModScan{Path: path, Err: err.Error()}
	}
	s := parseGoMod(string(b))
	s.Path = path
	return s
}

// parseCompose walks the file line by line, tracking only three levels of
// indentation: top-level keys, service names under `services:`, and the keys
// directly inside each service or anchor block.
func parseCompose(text string) ComposeScan {
	out := ComposeScan{Read: true, URLEnv: map[string]string{}}

	type building struct {
		ComposeService
		ownMem   bool
		inherits []string
	}
	var (
		section   string
		curAnchor string
		cur       *building
		services  []*building
		anchorMem = map[string]int{}
	)

	for i, raw := range strings.Split(text, "\n") {
		line := stripYAMLComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, hit := range coordinationTokens(line) {
			out.Coordination = append(out.Coordination, fmt.Sprintf("line %d names %q", i+1, hit))
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))
		key, val := splitYAMLKey(strings.TrimSpace(line))

		switch {
		case indent == 0:
			section, curAnchor, cur = key, "", nil
			if strings.HasPrefix(val, "&") {
				curAnchor = strings.TrimPrefix(val, "&")
			}
		case section == "services" && indent == 2 && key != "" && val == "":
			cur = &building{ComposeService: ComposeService{Name: key}}
			services = append(services, cur)
		case section == "services" && cur != nil && indent == 4:
			switch key {
			case "image":
				cur.Image = val
			case "mem_limit":
				cur.MemLimitMB, cur.ownMem = parseMemMB(val), true
			case "<<":
				cur.inherits = append(cur.inherits, strings.TrimPrefix(val, "*"))
			}
		case curAnchor != "" && indent == 2 && key == "mem_limit":
			anchorMem[curAnchor] = parseMemMB(val)
		}

		if strings.HasSuffix(key, "_URL") && val != "" {
			out.URLEnv[key] = val
		}
		if key == "LEASE_TTL" && val != "" {
			out.LeaseTTL = val
		}
	}

	for _, s := range services {
		if !s.ownMem {
			for _, a := range s.inherits {
				if mb, ok := anchorMem[a]; ok {
					s.MemLimitMB = mb
				}
			}
		}
		out.Services = append(out.Services, s.ComposeService)
	}
	return out
}

// stripYAMLComment removes a trailing comment. A '#' only starts a comment at
// the start of a line or after whitespace, which is YAML's own rule and keeps a
// '#' inside a value intact.
func stripYAMLComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimRight(line[:i], " \t")
		}
	}
	return strings.TrimRight(line, " \t\r")
}

// splitYAMLKey splits "key: value". A list item ("- x") has no key.
func splitYAMLKey(s string) (key, val string) {
	if strings.HasPrefix(s, "- ") || s == "-" {
		return "", ""
	}
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", ""
	}
	key = strings.Trim(strings.TrimSpace(s[:i]), `"'`)
	val = strings.Trim(strings.TrimSpace(s[i+1:]), `"'`)
	return key, val
}

// parseMemMB reads a compose memory size ("300m", "1g", "512MB", bytes).
func parseMemMB(v string) int {
	v = strings.ToLower(strings.TrimSpace(v))
	mult := 1.0 / (1024 * 1024) // bare number = bytes
	for _, suf := range []struct {
		s string
		m float64
	}{{"gb", 1024}, {"g", 1024}, {"mb", 1}, {"m", 1}, {"kb", 1.0 / 1024}, {"k", 1.0 / 1024}, {"b", 1.0 / (1024 * 1024)}} {
		if strings.HasSuffix(v, suf.s) {
			v, mult = strings.TrimSuffix(v, suf.s), suf.m
			break
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return int(f*mult + 0.5)
}

func parseGoMod(text string) ModScan {
	out := ModScan{Read: true}
	inBlock := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "require ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		case !inBlock:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		out.Modules = append(out.Modules, fields[0])
		for _, hit := range coordinationTokens(fields[0]) {
			out.Coordination = append(out.Coordination, fmt.Sprintf("%s (%s)", fields[0], hit))
		}
	}
	sort.Strings(out.Modules)
	return out
}

// coordinationTokens returns every denylisted product named in s.
//
// The text is split on anything that is not a letter or digit, and whole
// tokens are compared. Substring matching would find "nats" inside
// "signatures"; token matching finds it in "nats-io/nats.go" and in a metric
// called nats_client_msgs, which is where it would really be.
func coordinationTokens(s string) []string {
	tokens := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var hits []string
	for _, t := range tokens {
		for _, bad := range coordinationServices {
			if t == bad {
				hits = appendUnique(hits, bad)
			}
		}
	}
	return hits
}

// classifyService sorts a compose service into what NFR-10 counts.
func classifyService(s ComposeService) string {
	name := strings.ToLower(s.Name + " " + s.Image)
	switch {
	case strings.Contains(name, "postgres") || strings.Contains(name, "pgvector"):
		return "postgresql"
	case strings.HasPrefix(strings.ToLower(s.Name), "node-"):
		return "engine"
	case strings.Contains(name, "prometheus") || strings.Contains(name, "grafana"):
		return "observability"
	case strings.Contains(name, "embedder"):
		return "embedder"
	case strings.Contains(name, "migrate"):
		return "one-shot"
	}
	return "unclassified"
}
