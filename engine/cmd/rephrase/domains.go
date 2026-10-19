package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/dharaneswar339/durexec/app/agent"
	"github.com/dharaneswar339/durexec/app/agent/llm"
	"github.com/dharaneswar339/durexec/internal/core"
)

// ---------------------------------------------------------------------------
// THE DOMAINS
// ---------------------------------------------------------------------------
//
// The claim under test is not about flights. It is that an agent which crashes
// and re-plans re-issues the SAME intention in DIFFERENT words, so a duplicate
// check that compares bytes misses the repeat and the action happens twice.
// Flight booking is one instance of that. Measuring only flights would measure
// one domain and report a general result.
//
// So there are five, and they were chosen to differ in the SHAPE of the values
// that decide which action this is, not merely in their nouns. That shape is
// the thing the hybrid comparator's deciding-field gate depends on, and the
// whole point of varying it is to find out how far a hand-written field schema
// travels:
//
//	flight    an ordered pair of places, a weekday, a time of day, a count.
//	payment   ONE monetary amount and ONE recipient. No date, no place.
//	refund    an IDENTIFIER and a reason. The identifier is opaque — there is
//	          nothing to be semantically similar about, it either matches or it
//	          does not.
//	meeting   a person, a weekday, a clock time and a DURATION, which is a
//	          range rather than a point.
//	message   one addressee and a body that is almost entirely free text. This
//	          is the shape with no deciding fields to speak of, which is where
//	          a field gate has nothing to gate on and similarity has to decide.
//
// WHAT IS DELIBERATELY NOT DONE HERE. The engine's only deciding-field
// extractor, internal/idem/semantic/itinerary.go, is flight-shaped: it looks
// for airports, weekdays, dayparts and passenger counts. On the other four
// domains it will usually find no route and report the intent unrecognised, at
// which point the hybrid comparator falls back to the pure-semantic path. No
// extractor is written here for payments, refunds, meetings or messages,
// although writing five would make the hybrid numbers look much better. That
// would be measuring five scrapers fitted to five corpora, which is the
// criticism the existing hybrid result file already makes of itself, five
// times over. How far one hand-written schema generalises IS the finding, and
// the project's own conclusion — that an activity wanting semantic
// idempotency should DECLARE its deciding fields rather than have them scraped
// out of prose — is the thing this measures the need for.

// domain is one kind of action: the tools the planner may call, the prompt
// that describes them, and which tool is the one that must not happen twice.
type domain struct {
	// Name is short and appears in every CSV row and every per-domain table.
	Name string

	// Role is what the planner is told it is.
	Role string

	// ReadTool is safe to repeat: it looks something up and changes nothing.
	// Every domain has one because an agent with only a dangerous tool is not
	// an agent, and because the crash has to be able to land on the plan AFTER
	// a side effect rather than on the first plan of the run.
	ReadTool llm.ToolDoc

	// CommitTool is the one with a side effect in the world. Calling it twice
	// for one request is the harm.
	CommitTool llm.ToolDoc

	// KeepClause is the last line of the prompt: the values the model is told
	// to carry across unchanged. It names this domain's deciding fields, which
	// is the only per-domain instruction in the prompt.
	KeepClause string

	// ReadResult is what the read tool returns, as a JSON object. It is fixed
	// per domain rather than generated, so that the transcript the recovered
	// incarnation sees differs from the first one's only in the model's own
	// wording.
	ReadResult map[string]any
}

// Prompt is this domain's system prompt. It is llm.SystemPromptFor with this
// domain's two tools substituted and every other line identical, so that a
// rephrase rate measured on one domain is comparable with another.
func (d domain) Prompt() string {
	return llm.SystemPromptFor(d.Role, []llm.ToolDoc{d.ReadTool, d.CommitTool}, d.KeepClause)
}

var domains = map[string]domain{
	"flight": {
		Name:       "flight",
		Role:       llm.FlightRole,
		ReadTool:   llm.FlightTools[0],
		CommitTool: llm.FlightTools[1],
		KeepClause: "Always keep the route, date, time of day and passenger count\n" +
			"exactly as the goal states them.",
		ReadResult: map[string]any{
			"results": []map[string]string{
				{"flight": "6E-2113", "depart": "07:40", "arrive": "10:25", "fare": "INR 5,420"},
				{"flight": "AI-503", "depart": "08:15", "arrive": "11:05", "fare": "INR 6,180"},
			},
		},
	},

	"payment": {
		Name:       "payment",
		Role:       "payments agent for a small business",
		ReadTool:   llm.ToolDoc{Name: "LookupPayee", Doc: "find a saved payee. args: a short natural sentence naming who to look for."},
		CommitTool: llm.ToolDoc{Name: "SendPayment", Doc: "send money. args: a short natural sentence describing the payment."},
		KeepClause: "Always keep the amount, the currency and the recipient\n" +
			"exactly as the goal states them.",
		ReadResult: map[string]any{
			"payees": []map[string]string{
				{"name": "matched", "account": "****4417", "bank": "HDFC", "status": "verified"},
			},
		},
	},

	"refund": {
		Name:       "refund",
		Role:       "customer support agent who issues refunds",
		ReadTool:   llm.ToolDoc{Name: "LookupOrder", Doc: "look up an order. args: a short natural sentence naming the order."},
		CommitTool: llm.ToolDoc{Name: "IssueRefund", Doc: "refund an order. args: a short natural sentence describing the refund."},
		KeepClause: "Always keep the order number and the reason for the refund\n" +
			"exactly as the goal states them.",
		ReadResult: map[string]any{
			"order": map[string]string{
				"status": "DELIVERED", "paid": "INR 2,199", "refundable": "yes",
			},
		},
	},

	"meeting": {
		Name:     "meeting",
		Role:     "scheduling assistant",
		ReadTool: llm.ToolDoc{Name: "CheckCalendar", Doc: "check free time. args: a short natural sentence describing what to check."},
		CommitTool: llm.ToolDoc{Name: "ScheduleMeeting", Doc: "put a meeting on the calendar and invite people. args: a short natural " +
			"sentence describing the meeting."},
		KeepClause: "Always keep the person, the day, the start time and the length\n" +
			"exactly as the goal states them.",
		ReadResult: map[string]any{
			"busy": []string{"09:00-09:30", "13:00-14:00"},
			"note": "everything else that day is free",
		},
	},

	"message": {
		Name:       "message",
		Role:       "assistant who writes and sends email on someone's behalf",
		ReadTool:   llm.ToolDoc{Name: "FindContact", Doc: "find an address book entry. args: a short natural sentence naming who to look for."},
		CommitTool: llm.ToolDoc{Name: "SendEmail", Doc: "send an email. args: a short natural sentence saying who it goes to and what it says."},
		KeepClause: "Always keep the recipient and what the message has to say\n" +
			"exactly as the goal states them.",
		ReadResult: map[string]any{
			"contact": map[string]string{"match": "found", "address": "on file", "verified": "yes"},
		},
	},
}

// domainOrder fixes the order domains are reported in. A map's iteration order
// would reshuffle every table between runs and make two reports needlessly
// hard to compare.
var domainOrder = []string{"flight", "payment", "refund", "meeting", "message"}

// ---------------------------------------------------------------------------
// The tools, as activities.
// ---------------------------------------------------------------------------

// sideEffects records what a domain's commit tool actually did.
//
// It is the harness's own counter rather than Activities.Bookings(), because
// Bookings counts flight bookings and four of these five domains do not book
// flights. Counting the real calls — not inferring them from the verdicts — is
// what makes "two seats were taken" a measurement.
type sideEffects struct {
	n atomic.Int64
}

func (s *sideEffects) Count() int { return int(s.n.Load()) }

// toolsFor builds the two activities for one domain.
//
// The commit tool is registered core.ModeSemantic and the read tool
// core.ModeExact, which is the same split app/agent makes and for the same
// reason: repeating a lookup costs a call, repeating the commit costs money.
// The modes are not exercised by the in-memory simulator — it has no dedup
// path — but they are part of what a registration says about an activity, and
// declaring them wrongly here would make this file a bad example to copy.
func toolsFor(d domain, effects *sideEffects) []agent.Registration {
	if d.Name == "flight" {
		// The flight domain is the agent's own, already registered by
		// app/agent. Adding it again would collide, and Simulate refuses that.
		return nil
	}
	return []agent.Registration{
		{
			Type:    d.ReadTool.Name,
			Mode:    core.ModeExact,
			Handler: readHandler(d),
		},
		{
			Type:       d.CommitTool.Name,
			Mode:       core.ModeSemantic,
			Handler:    commitHandler(d, effects),
			IntentText: intentText,
		},
	}
}

func readHandler(d domain) func(context.Context, json.RawMessage) (json.RawMessage, error) {
	return func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in agent.ToolCall
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, &core.NonRetryableError{Cause: err}
		}
		out := map[string]any{"query": in.Args}
		for k, v := range d.ReadResult {
			out[k] = v
		}
		return json.Marshal(out)
	}
}

func commitHandler(d domain, effects *sideEffects) func(context.Context, json.RawMessage) (json.RawMessage, error) {
	return func(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
		var in agent.ToolCall
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, &core.NonRetryableError{Cause: err}
		}
		if in.Args == "" {
			return nil, &core.NonRetryableError{
				Cause: fmt.Errorf("%s: nothing to act on", d.CommitTool.Name),
			}
		}
		n := effects.n.Add(1)
		// The confirmation is deliberately unambiguous. A recovered planner
		// that is shown this transcript should be able to tell that the action
		// already happened — on the control arm, whether it does is the thing
		// being measured, and a vague result would measure the wording of the
		// stub instead of the model's judgement.
		return json.Marshal(map[string]string{
			"status":         "CONFIRMED",
			"reference":      fmt.Sprintf("%s-%06d", d.Name, n),
			"what_was_done":  in.Args,
			"already_done":   "yes",
			"irreversible":   "yes",
			"human_readable": fmt.Sprintf("%s completed: %s", d.CommitTool.Name, in.Args),
		})
	}
}

// intentText is the sentence the semantic check would compare for these tools:
// the model's own words, not the JSON envelope around them. It is the same
// rendering app/agent does for its own tools, and getting it wrong would
// dilute every similarity score with constant scaffolding.
func intentText(raw json.RawMessage) (string, error) {
	var c agent.ToolCall
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", err
	}
	text := agent.CanonicalIntent(c.Args)
	if text == "" {
		return "", fmt.Errorf("tool call for %q has no arguments to compare", c.Tool)
	}
	return text, nil
}
