// Package llm holds the agent's real, model-backed planner.
//
// It is a package rather than a file inside cmd/engined because two programs
// need the same planner and must not hold two copies of it. The engine uses it
// to plan live workflows; cmd/rephrase uses it to measure what the model
// actually does when a crash makes it plan the same step twice. If the two
// drove different clients, different prompts or different decoding settings,
// the measurement would describe a planner the engine does not run — which is
// precisely the kind of gap the project's results files exist to close.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/app/agent"
)

// GroqPlanner is a real language model standing where demoPlanner used to.
//
// The project's claim is about what happens when an AI agent crashes and
// re-plans: it re-issues the same intention in words it chooses itself, and an
// exact-text duplicate check misses it. A scripted planner cannot evidence that
// claim, because the rephrasing would be ours rather than the model's. This
// calls a model over the network and takes whatever wording comes back.
//
// Groq's API is OpenAI-shaped, so this is a small hand-rolled client rather
// than a dependency: one POST, one JSON body, no streaming and no tool-calling.
type GroqPlanner struct {
	key    string
	model  string
	client *http.Client

	// keys holds every configured API key and rotates between them, so a
	// rate-limited key costs the next request rather than a sleep. Nil means
	// "use the single key field above", which is what a caller that built
	// this struct by hand gets. See keyring.go for why more than one key is
	// used and why it changes nothing about a measurement.
	keys *keyring

	// Attempts bounds how many times one planning call is sent. See do() for
	// why a rate-limited call is retried rather than failed.
	Attempts int

	// Backoff is the first retry delay; each further wait doubles it. It is a
	// field so a measurement harness can pace itself against a known quota
	// without editing this file.
	Backoff time.Duration

	// Temperature is the decoding temperature. It is a field for the same
	// reason: the shipped value is the one the engine runs, and a harness that
	// wants to report what a different value does has to be able to set it
	// without changing what engined does.
	Temperature float64

	// Prompt is the system prompt. Empty means SystemPrompt, the flight agent
	// the engine runs. A harness measuring the claim across several unrelated
	// actions sets it per domain; see SystemPromptFor.
	Prompt string
}

// DefaultModel is deliberately not the largest model on offer. The reasoning
// models return their answer in a separate field this client does not read, so
// a plain instruct model is both simpler and more predictable here.
const DefaultModel = "qwen/qwen3.8-27b"

// DefaultTemperature is non-zero, because the point of this planner is that
// the model phrases the same intent differently on a retry. At temperature 0
// it would repeat itself and the experiment would measure nothing.
const DefaultTemperature = 0.8

// Endpoint is Groq's OpenAI-compatible chat completions URL.
const Endpoint = "https://api.groq.com/openai/v1/chat/completions"

// NewGroqPlanner returns a planner backed by Groq, or nil when no key is set.
// Nil means "fall back to the scripted planner", so the engine still starts and
// demonstrates on a machine with no API key and no network.
func NewGroqPlanner() *GroqPlanner {
	key := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	if key == "" {
		return nil
	}
	model := strings.TrimSpace(os.Getenv("GROQ_MODEL"))
	if model == "" {
		model = DefaultModel
	}
	return &GroqPlanner{
		key:   key,
		keys:  newKeyring(),
		model: model,
		// A workflow task holds a shard's attention while it runs, so a hung
		// model must not hold it indefinitely. On timeout the activity fails
		// and the engine retries it, which is the behaviour we want.
		client:      &http.Client{Timeout: 45 * time.Second},
		Attempts:    5,
		Backoff:     2 * time.Second,
		Temperature: DefaultTemperature,
	}
}

// Model reports which model this planner talks to, so a result file can record
// it rather than assume it.
func (p *GroqPlanner) Model() string { return p.model }

var _ agent.Planner = (*GroqPlanner)(nil)

// ToolDoc is how one tool is described to the model: its name, and the one
// line that tells the model what it does and what to put in args.
type ToolDoc struct {
	Name string
	Doc  string
}

// FlightRole and FlightTools are the engine's own agent: a travel booking
// assistant with a read-only search and a side-effecting booking.
var (
	FlightRole = "travel booking agent"

	FlightTools = []ToolDoc{
		{"SearchFlights", "find flights. args: a short natural sentence describing the search."},
		{"BookFlight", "book a flight. args: a short natural sentence describing the booking."},
	}
)

// SystemPrompt is the prompt the engine runs. It is the flight agent, which is
// what app/agent's workflow is.
var SystemPrompt = SystemPromptFor(FlightRole, FlightTools,
	"Always keep the route, date, time of day and passenger count\nexactly as the goal states them.")

// SystemPromptFor builds the planner prompt for one role and tool set.
//
// There is a template rather than a literal because the project's claim is not
// about flights. An agent that crashes and re-plans re-issues ITS intention in
// new words whatever that intention was, so a measurement of the claim has to
// put the same planner behind several unrelated actions — a payment, a refund,
// a message — and the only honest way to do that is to vary the tool list and
// change nothing else. Every other line below is identical for every domain,
// so a rephrase rate can be compared across them.
//
// The instruction to vary the wording is not decoration. A model asked twice
// for the same step will often return the same sentence, and an identical
// sentence is caught by the exact-match check, which would make the semantic
// check look unnecessary for the wrong reason. Asking for natural variation
// reproduces what a re-planning agent actually does.
//
// This is exported, and so is the prompt it builds, because any measurement of
// this planner's output has to be able to print the prompt that produced it.
// An instruction to vary phrasing is a thing a reader of a rephrase-rate
// figure needs to know about, and a prompt kept private would hide it.
func SystemPromptFor(role string, tools []ToolDoc, keepClause string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a %s. You work in steps.\n\nTools you may call:\n", role)

	width := 0
	for _, t := range tools {
		if len(t.Name) > width {
			width = len(t.Name)
		}
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		fmt.Fprintf(&b, "  %-*s - %s\n", width, t.Name, t.Doc)
		names = append(names, t.Name)
	}

	fmt.Fprintf(&b, `
Reply with ONLY a JSON object, no markdown fence, no commentary:
{"thought":"...","tool":"%s","args":"...","done":false}
When the goal is met, reply instead:
{"thought":"...","done":true,"answer":"..."}

Write args as a person would say it out loud, not as structured data.
Vary your phrasing naturally between calls; do not repeat an earlier sentence
word for word. %s`, strings.Join(names, "|"), keepClause)
	return b.String()
}

type groqReq struct {
	Model       string    `json:"model"`
	Messages    []groqMsg `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
}

type groqMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqResp struct {
	Choices []struct {
		Message groqMsg `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ErrRateLimited is returned when the provider refused the call for quota
// reasons and every permitted attempt was used up. It is distinguished from
// other failures so a caller measuring the model can tell "we ran out of
// quota" apart from "the model answered badly" — the first invalidates a
// trial, the second is data.
var ErrRateLimited = errors.New("groq: rate limited and out of attempts")

// Plan asks the model what to do next, given the goal and what has happened.
func (p *GroqPlanner) Plan(ctx context.Context, in agent.PlanInput) (agent.Plan, error) {
	temp := p.Temperature
	if temp == 0 {
		temp = DefaultTemperature
	}
	prompt := p.Prompt
	if prompt == "" {
		prompt = SystemPrompt
	}
	body, err := json.Marshal(groqReq{
		Model: p.model,
		Messages: []groqMsg{
			{Role: "system", Content: prompt},
			{Role: "user", Content: p.userPrompt(in)},
		},
		Temperature: temp,
		MaxTokens:   300,
	})
	if err != nil {
		return agent.Plan{}, fmt.Errorf("groq: encode request: %w", err)
	}

	raw, err := p.do(ctx, body)
	if err != nil {
		return agent.Plan{}, err
	}

	var out groqResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return agent.Plan{}, fmt.Errorf("groq: decode response: %w", err)
	}
	if out.Error != nil {
		return agent.Plan{}, fmt.Errorf("groq: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return agent.Plan{}, fmt.Errorf("groq: no choices in response")
	}

	plan, err := ParsePlan(out.Choices[0].Message.Content)
	if err != nil {
		return agent.Plan{}, err
	}
	plan.Step = in.Step
	return plan, nil
}

// do sends one planning request, retrying while the provider says "not yet".
//
// A free-tier quota answers 429 to perfectly valid calls, and a 5xx from a
// hosted inference service is usually a moment of capacity rather than a
// permanent refusal. Giving up on either would fail a workflow for a reason
// that has nothing to do with the workflow, and would silently drop trials
// from a measurement run — which is worse than slow, because the trials that
// drop out are not a random sample of the trials attempted.
//
// Retry-After is honoured when the provider sends it, because a server that
// has told us when to come back knows better than our own exponential guess.
func (p *GroqPlanner) do(ctx context.Context, body []byte) ([]byte, error) {
	attempts := p.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	backoff := p.Backoff
	if backoff <= 0 {
		backoff = 2 * time.Second
	}

	ring := p.keys
	if ring == nil || ring.Len() == 0 {
		ring = newKeyringOf(p.key)
	}
	// With N keys a round of attempts is N requests rather than one, because a
	// key the provider has just refused is set aside and the next is tried
	// IMMEDIATELY. Only when every key is cooling does anything sleep. The
	// budget is therefore attempts x keys, and a 429 on one key costs one
	// request instead of a wait.
	budget := attempts * ring.Len()

	var lastStatus int
	slept := 0
	for attempt := 0; attempt < budget; attempt++ {
		idx, key, wait, ok := ring.take(time.Now())
		if !ok {
			// Every key is rate-limited. This is the only place that waits,
			// and it waits exactly until the first key is free rather than
			// guessing, with the exponential backoff as a floor so a provider
			// that keeps refusing is not hammered.
			floor := time.Duration(float64(backoff) * math.Pow(2, float64(slept)))
			if wait < floor {
				wait = floor
			}
			slept++
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("groq: build request: %w", err)
		}
		// The key goes into this header and nowhere else. It is never
		// formatted into a message, an error or a log line; keyring.Label
		// names a key by position when one has to be identified.
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")

		resp, err := p.client.Do(req)
		if err != nil {
			// A transport error is retried for the same reason a 5xx is: the
			// call never reached a model, so nothing about it was decided.
			lastStatus = 0
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		status := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("groq: read response: %w", readErr)
		}

		switch {
		case status == http.StatusTooManyRequests:
			// This key is out of quota for now. Set it aside for as long as
			// the provider asked and take the next key on the next iteration
			// WITHOUT sleeping: another account's quota is unaffected by this
			// one's.
			lastStatus = status
			d, haveRetryAfter := parseRetryAfter(retryAfter)
			if !haveRetryAfter {
				d = 0 // penalise applies its own short default
			}
			ring.penalise(idx, d)
			continue

		case status >= 500:
			// Not the key's fault: a moment of capacity at the provider.
			// Retrying is reasonable, but the key is set aside briefly so one
			// sick endpoint cannot absorb every attempt.
			lastStatus = status
			ring.penalise(idx, time.Second)
			continue
		case status != http.StatusOK:
			// A 4xx that is not a rate limit is our fault — a rejected key, a
			// retired model, a malformed body — and retrying it would only
			// repeat the mistake more expensively. The key is named by
			// POSITION; the provider's body is echoed because it says what
			// was wrong, and it never contains the key that was sent.
			return nil, fmt.Errorf("groq: status %d using %s: %s",
				status, ring.Label(idx), truncate(string(raw), 200))
		}
		return raw, nil
	}

	if lastStatus == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w after %d attempts across %d key(s)",
			ErrRateLimited, budget, ring.Len())
	}
	return nil, fmt.Errorf("groq: gave up after %d attempts across %d key(s) (last status %d)",
		budget, ring.Len(), lastStatus)
}

// parseRetryAfter reads the header in its delay-seconds form. The HTTP-date
// form is ignored rather than mishandled: a wrong wait computed from a clock
// skew would be worse than falling back to our own backoff.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs < 0 {
		return 0, false
	}
	// A provider asking us to wait longer than a workflow task's patience is
	// not something to obey silently; cap it and let the timeout decide.
	if secs > 120 {
		secs = 120
	}
	return time.Duration(secs * float64(time.Second)), true
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// userPrompt restates the goal and replays what has happened so far.
//
// The transcript is sent every time rather than kept in the planner, because
// the planner is called fresh on every workflow task and may be called on a
// different node after a failover. Its only memory is what the engine's log
// hands it, which is the same discipline the workflow code itself follows.
func (p *GroqPlanner) userPrompt(in agent.PlanInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n\n", in.Goal)
	if len(in.Transcript) == 0 {
		b.WriteString("Nothing has happened yet. Decide the first step.\n")
		return b.String()
	}
	b.WriteString("What has happened so far:\n")
	for _, t := range in.Transcript {
		fmt.Fprintf(&b, "- called %s with %q, result: %s\n",
			t.Call.Tool, t.Call.Args, truncate(string(t.Observation), 200))
	}
	b.WriteString("\nDecide the next step.\n")
	return b.String()
}

// ParsePlan reads the model's JSON, tolerating the wrappers models add.
//
// Instructing a model to emit bare JSON is a request, not a guarantee: a
// markdown fence or a sentence of preamble is common. Rather than fail a
// workflow over formatting, this finds the outermost JSON object and decodes
// that; anything genuinely unparseable is still an error.
func ParsePlan(content string) (agent.Plan, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var plan agent.Plan
	if err := json.Unmarshal([]byte(s), &plan); err != nil {
		return agent.Plan{}, fmt.Errorf("groq: model did not return usable JSON: %w (got %q)", err, truncate(content, 160))
	}
	if !plan.Done && plan.Tool == "" {
		return agent.Plan{}, fmt.Errorf("groq: model returned neither a tool nor done (got %q)", truncate(content, 160))
	}
	return plan, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
