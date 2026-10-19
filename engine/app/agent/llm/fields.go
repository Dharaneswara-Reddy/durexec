package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/dharaneswar339/durexec/internal/idem/semantic"
)

// ---------------------------------------------------------------------------
// THE LLM FIELD EXTRACTOR
// ---------------------------------------------------------------------------
//
// This is the third answer to "where do the deciding fields come from?".
//
//	itinerary.go  a hand-written flight vocabulary. Ten airports, seven
//	              weekdays. Does not travel.
//	generic.go    token shapes. Travels everywhere, but only sees what has a
//	              shape, and cannot know that "the vendor" is a recipient.
//	this file     ask a model. Travels everywhere AND can read prose, at the
//	              price of a network call and of determinism.
//
// # Why asking a model for the fields is not circular
//
// It would be circular to ask a model "are these two requests the same?" and
// call the answer a duplicate check — the project's whole argument is that a
// similarity model answers a different question than the one being asked.
// This asks something much narrower and much more answerable: "which values
// in this one sentence decide what it does?". It is an extraction task over a
// single input, not a judgement over a pair. The comparison itself is still
// exact string equality over the extracted values, performed by the engine.
//
// # Determinism, and the one thing that makes this acceptable
//
// FieldExtractor's contract says it must be pure, and a network call to a
// sampled model is not pure. That is not a detail to wave through, so here is
// the exact reason it is survivable, and the exact place it would stop being:
//
// The engine journals the VERDICT, not the extraction. internal/idem's check
// runs once, on the live path, and writes core.DedupEvaluated — verdict,
// similarity, matched step, thresholds, model and intent text — into the
// event log. Replay does not re-run the comparator; it reads that event back
// and replays the decision it recorded (see internal/idem and the engine's
// replay path, and hybrid.go's note on why the comparator is a
// deployment-wide switch). So two nodes cannot reach two different verdicts
// for the same history: only one verdict was ever computed, and it is the one
// in the log forever.
//
// WHAT THAT DOES NOT COVER, stated plainly:
//
//   - The journaled EXPLANATION is a snapshot, not a derivation. With the
//     deterministic extractors a reader can re-derive the field signature
//     from the logged intent text and check the engine's reasoning. With this
//     one they cannot: a second call may extract different fields. The
//     explanation remains auditable only in the sense that it says what was
//     decided, not that it can be reproduced.
//   - A retry of the same activity BEFORE the verdict is journaled can
//     extract differently and reach a different verdict. That is a real
//     window, it is the same window the pure-semantic path has with a
//     non-deterministic embedding service, and it is bounded by the check
//     happening once per scheduling attempt.
//   - Temperature is 0 to reduce variance anyway. Temperature 0 is not a
//     guarantee of identical output across a provider's fleet, so this
//     reduces drift rather than removing it.
//
// # Failure is never the workflow's problem
//
// Every failure path here returns Recognised false and a nil error: no key,
// no network, a 429 that outlasted the backoff, unparseable JSON, an empty
// field set. The hybrid comparator reads Recognised false as "the gate has no
// information" and falls back to the pure-semantic comparison, stamping the
// reason into the verdict. A model outage therefore degrades the duplicate
// check; it does not fail the activity.

// FieldExtractor extracts deciding fields by asking a model.
//
// It is safe for concurrent use. Calls counts the requests actually sent, so
// a result file can report the cost of this arm as a measurement rather than
// an estimate.
type FieldExtractor struct {
	planner *GroqPlanner

	// Calls and Failures are counters, not statistics: Calls is every request
	// sent to the provider, Failures every one that ended in Recognised
	// false. Both are reported by the evaluation.
	Calls    atomic.Int64
	Failures atomic.Int64

	// Nanos accumulates the wall-clock time spent inside Extract, so the
	// added latency of this arm is measured rather than guessed.
	Nanos atomic.Int64
}

// FieldModel is the model asked for extractions.
//
// It is the same default the planner uses, for one reason: a result file that
// compared a planner on one model with an extractor on another would be
// comparing two systems and reporting one.
const FieldModel = DefaultModel

// NewFieldExtractor returns an extractor, or nil when GROQ_API_KEY is unset.
//
// Nil means "this arm cannot run", which the harness reports as a skipped arm
// rather than as a zero score. A zero score would read as a finding.
func NewFieldExtractor() *FieldExtractor {
	key := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	if key == "" {
		return nil
	}
	model := strings.TrimSpace(os.Getenv("GROQ_FIELD_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("GROQ_MODEL"))
	}
	if model == "" {
		model = FieldModel
	}
	return &FieldExtractor{
		planner: &GroqPlanner{
			key: key,
			// Every GROQ_API_KEY* in the environment, rotated. The extraction
			// arm is the only thing in this project that is quota-bound: the
			// provider's free tier limits TOKENS per minute, and one key
			// sustains about eleven extractions a minute against this prompt.
			// Rotation across independent accounts widens that without
			// changing anything the run measures — see keyring.go.
			keys:   newKeyring(),
			model:  model,
			client: &http.Client{Timeout: 45 * time.Second},
			// Five attempts with a two-second base, doubling: the free tier
			// answers 429 to perfectly valid calls, and do() also honours a
			// Retry-After header when the provider sends one.
			Attempts: 5,
			Backoff:  2 * time.Second,
			// Zero, not DefaultTemperature. The planner wants variety — that
			// is the behaviour the experiment measures. An extractor wants
			// the same answer twice.
			Temperature: 0,
		},
	}
}

// Model reports which model answered, for the result file's header.
func (e *FieldExtractor) Model() string { return e.planner.model }

// fieldPrompt is the system prompt. It does three things, and each one is
// load-bearing:
//
//  1. It defines a deciding value by its CONSEQUENCE — change it and a
//     different thing happens in the world — rather than by listing kinds of
//     value, which would make it a vocabulary in prose.
//  2. It specifies the NORMALISATION, because the whole approach dies if one
//     phrasing yields "500" and another "500 USD" or "five hundred". The
//     model is doing the normalising here; this file only folds case and
//     trims, so the instructions have to be exact.
//  3. It asks for the leftover text. Without a remainder every pair that
//     passes the gate has two empty remainders, which the comparator scores
//     1.0 WITHOUT CONSULTING THE EMBEDDER — and an arm that never consults
//     the model is measuring exact matching, which is precisely the caveat
//     the published hybrid result had to carry.
const fieldPrompt = `You extract the DECIDING VALUES from one request that an automated agent is about to carry out.

A value is DECIDING if changing it would make the agent do a DIFFERENT thing in the real world: a different amount would leave a different sum of money, a different date would book a different day, a different recipient would pay a different person, a different identifier would refund a different order.

A value is NOT deciding if it only changes how the request is worded: politeness, the verb chosen, a restatement, a preference or a note that does not change what is acted on.

Reply with ONE JSON object and nothing else:

{"fields": {"name": "value", ...}, "remainder": "..."}

Rules for "fields":
- Use short lower-case snake_case names that describe the ROLE of the value: amount, currency, recipient, date, time, quantity, order_id, origin, destination, duration, plan, address, subject.
- Use the SAME name for the same role every time. Never invent a name for something already covered by a name above.
- Normalise every value, because two wordings of one value must produce the same string:
  - numbers and quantities as digits only: "two" -> "2", "five hundred" -> "500"
  - money as digits only with no symbol and no separators: "$1,500.00" -> "1500"; put the currency in a separate "currency" field as a lower-case ISO code: "usd", "inr", "eur"
  - a calendar date as yyyy-mm-dd when the year is known, mm-dd when it is not; a named day as the lower-case weekday, with "+1w" appended when the request says next week: "next friday" -> "friday+1w"
  - a clock time as 24-hour hh:mm: "9am" -> "09:00", "7:40 pm" -> "19:40"
  - a part of the day as one of: am, afternoon, pm
  - everything else lower-cased, trimmed, with inner whitespace collapsed to one space
- Omit a field entirely when the request does not state it. NEVER guess, and never write "unknown", "none", "null" or an empty string.

Rules for "remainder": every word of the request that is NOT part of any value above, lower-cased, in the order it appeared, as one string. Omit the names of the fields themselves. It may be empty.

Answer with the JSON object only. No explanation, no markdown fence.`

// fieldReply is the shape asked for.
type fieldReply struct {
	Fields    map[string]string `json:"fields"`
	Remainder string            `json:"remainder"`
}

// RawExtraction is one extraction together with the reply it was parsed
// from.
//
// The raw reply is returned so a measurement harness can cache it on disk: a
// re-run then recomputes the identical Fields from the identical bytes
// instead of paying for the call again, and a parsing change applies to the
// stored replies rather than silently keeping results parsed by older code.
// The engine itself has no use for it and calls Extract.
type RawExtraction struct {
	Fields semantic.Fields
	Raw    string
}

// ExtractRaw is Extract, keeping the model's reply.
func (e *FieldExtractor) ExtractRaw(activityType, intentText string) (RawExtraction, error) {
	f, raw, err := e.extract(activityType, intentText)
	return RawExtraction{Fields: f, Raw: raw}, err
}

// Extract is the semantic.FieldExtractor. It never returns an error: see the
// header on why a model failure must degrade the duplicate check and not the
// workflow.
func (e *FieldExtractor) Extract(activityType, intentText string) (semantic.Fields, error) {
	f, _, err := e.extract(activityType, intentText)
	return f, err
}

func (e *FieldExtractor) extract(activityType, intentText string) (semantic.Fields, string, error) {
	if e == nil || e.planner == nil {
		return semantic.Fields{}, "", nil
	}
	start := time.Now()
	defer func() { e.Nanos.Add(int64(time.Since(start))) }()

	user := fmt.Sprintf("Activity: %s\nRequest: %s\n", activityType, intentText)
	body, err := json.Marshal(groqReq{
		Model: e.planner.model,
		Messages: []groqMsg{
			{Role: "system", Content: fieldPrompt},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   400,
	})
	if err != nil {
		e.Failures.Add(1)
		return semantic.Fields{}, "", nil
	}

	e.Calls.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	raw, err := e.planner.do(ctx, body)
	if err != nil {
		e.Failures.Add(1)
		return semantic.Fields{}, "", nil
	}

	var resp groqResp
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Error != nil || len(resp.Choices) == 0 {
		e.Failures.Add(1)
		return semantic.Fields{}, "", nil
	}

	content := resp.Choices[0].Message.Content
	f, ok := ParseFields(content)
	if !ok {
		e.Failures.Add(1)
		return semantic.Fields{}, content, nil
	}
	if !f.Recognised {
		e.Failures.Add(1)
	}
	return f, content, nil
}

// ParseFields reads the model's reply into a Fields.
//
// It is exported and takes a string so that a cached reply can be re-parsed
// with no model call: the evaluation caches the raw reply on disk, and a
// re-run must produce the identical Fields from it or the cache would be
// measuring something other than the run it stands in for.
func ParseFields(content string) (semantic.Fields, bool) {
	s := strings.TrimSpace(content)
	// Models wrap JSON in fences and preambles whatever the prompt says.
	// Taking the outermost object is the same tolerance ParsePlan applies,
	// for the same reason: a formatting habit must not be read as a refusal.
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var reply fieldReply
	if err := json.Unmarshal([]byte(s), &reply); err != nil {
		return semantic.Fields{}, false
	}

	deciding := make([]semantic.Field, 0, len(reply.Fields))
	for name, value := range reply.Fields {
		n, v := canonField(name), canonValue(value)
		if n == "" || v == "" || isNullish(v) {
			// A model told not to guess still writes "unknown" sometimes.
			// Keeping such a value would make two silences disagree.
			continue
		}
		deciding = append(deciding, semantic.Field{Name: n, Value: v})
	}
	return semantic.NewFields(deciding, canonValue(reply.Remainder)), true
}

// canonField folds a field name to the one spelling the gate compares.
func canonField(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// canonValue folds a value: lower case, trimmed, inner whitespace collapsed,
// surrounding quotes and trailing sentence punctuation removed.
//
// It deliberately does NOT reinterpret the value — it will not turn "two"
// into "2". The prompt asks the model to normalise, and doing it again here
// with the generic extractor's rules would make this arm a blend of two
// extractors while reporting as one.
func canonValue(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, `"'`)
	s = strings.TrimRight(s, ".,;:!?")
	return strings.Join(strings.Fields(s), " ")
}

// isNullish catches the ways a model says "I don't know" while having been
// told to omit the field instead.
func isNullish(v string) bool {
	switch v {
	case "unknown", "none", "null", "nil", "n/a", "na", "not stated",
		"not specified", "unspecified", "-", "--", "?":
		return true
	}
	return false
}

// Compile-time proof that this satisfies the engine's extractor contract.
var _ semantic.FieldExtractor = (*FieldExtractor)(nil).Extract
