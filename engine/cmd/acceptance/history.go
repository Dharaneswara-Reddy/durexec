package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// Pure helpers over one workflow's history, used by the checks that drive a
// workflow themselves. Like analyze.go, nothing here touches the network.

// activityTypesByK maps each program-order index to the activity scheduled at
// it, so a completion (which carries only k) can be named.
func activityTypesByK(events []EventRow) map[int]string {
	out := map[int]string{}
	for _, ev := range events {
		if ev.Type != evActivityScheduled && ev.Type != evCompScheduled {
			continue
		}
		var p activityScheduledPayload
		if json.Unmarshal(ev.Payload, &p) == nil {
			if _, seen := out[p.K]; !seen {
				out[p.K] = p.ActivityType
			}
		}
	}
	return out
}

// SignalTiming says where in a history a signal landed relative to the point
// at which the workflow could first have started waiting for it.
type SignalTiming struct {
	// SignalSeq is the seq of the first SignalReceived event, 0 if none.
	SignalSeq int64
	// LastPreWaitSeq is the seq of the last completion of an activity that
	// runs BEFORE the wait. The workflow cannot have reached the wait until
	// that completion was journaled, so a signal with a lower seq provably
	// arrived while the workflow was not yet waiting.
	LastPreWaitSeq int64
	// BeforeWait is SignalSeq < LastPreWaitSeq.
	BeforeWait bool
	// Received is whether any SignalReceived event exists.
	Received bool
}

// SignalBeforeWait analyses one run. afterWait names the activity types the
// workflow only schedules after the wait (for the travel agent, Finalize);
// every other completed activity is pre-wait work.
//
// The argument rests on the workflow's program order, which is the one thing
// a history cannot show directly: the wait itself journals nothing. So the
// claim is phrased in terms of events that are journaled. If the signal's seq
// is lower than the completion of some step the workflow had to finish before
// it could wait, then the signal was stored before the wait began, and the
// workflow picking it up later is exactly "buffered, not lost".
func SignalBeforeWait(events []EventRow, afterWait map[string]bool) SignalTiming {
	var t SignalTiming
	types := activityTypesByK(events)
	for _, ev := range events {
		switch ev.Type {
		case evSignalReceived:
			if !t.Received {
				t.Received = true
				t.SignalSeq = ev.Seq
			}
		case evActivityCompleted:
			var p kPayload
			if json.Unmarshal(ev.Payload, &p) != nil {
				continue
			}
			if afterWait[types[p.K]] {
				continue
			}
			if ev.Seq > t.LastPreWaitSeq {
				t.LastPreWaitSeq = ev.Seq
			}
		}
	}
	t.BeforeWait = t.Received && t.LastPreWaitSeq > 0 && t.SignalSeq < t.LastPreWaitSeq
	return t
}

// CompletedResultField reads one boolean field out of the workflow's result,
// as journaled in WorkflowExecutionCompleted. found is false when there is no
// completion event or the field is absent.
func CompletedResultField(events []EventRow, field string) (value, found bool) {
	for _, ev := range events {
		if ev.Type != evWorkflowCompleted {
			continue
		}
		var p struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil {
			return false, false
		}
		raw, ok := p.Result[field]
		if !ok {
			return false, false
		}
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

// CountType counts events of one type.
func CountType(events []EventRow, typ string) int {
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// CompareHistories checks that the history the query API returned is the
// stored log, event for event. That is what "no separate read model" means
// when observed from outside: there is nothing between the table and the
// response that could differ.
//
// Payloads are compared as decoded JSON, not bytes. PostgreSQL's JSONB
// normalises key order and whitespace on the way in, so two byte strings can
// differ while carrying the same document, and reporting that as a mismatch
// would be a finding about JSONB rather than about the engine.
func CompareHistories(api, db []EventRow) (bool, string) {
	if len(api) != len(db) {
		return false, fmt.Sprintf("API returned %d events, the table holds %d", len(api), len(db))
	}
	for i := range api {
		a, d := api[i], db[i]
		switch {
		case a.Seq != d.Seq:
			return false, fmt.Sprintf("event %d: API seq %d, table seq %d", i, a.Seq, d.Seq)
		case a.Type != d.Type:
			return false, fmt.Sprintf("seq %d: API type %s, table type %s", a.Seq, a.Type, d.Type)
		case a.RunID != d.RunID:
			return false, fmt.Sprintf("seq %d: API run %s, table run %s", a.Seq, a.RunID, d.RunID)
		case !a.CreatedAt.Equal(d.CreatedAt):
			return false, fmt.Sprintf("seq %d: API created_at %s, table %s", a.Seq, a.CreatedAt, d.CreatedAt)
		case !sameJSON(a.Payload, d.Payload):
			return false, fmt.Sprintf("seq %d (%s): payloads differ", a.Seq, a.Type)
		}
	}
	return true, ""
}

func sameJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
