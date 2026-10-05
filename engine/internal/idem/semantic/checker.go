package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dharaneswar339/durexec/internal/core"
)

// NoMatch is the value written into core.DedupEvaluated.MatchedK when nothing
// was matched. Zero is not usable as "none" because zero is a legitimate
// program-order index, and a reader of the log must be able to tell "matched
// the first step" apart from "matched nothing".
const NoMatch = -1

// FallbackPrefix marks a verdict that was reached without asking the model.
//
// core.DedupEvaluated has no dedicated flag for this, and core is frozen, so
// the fact is recorded in the Model field: the model name is prefixed, giving
// for example "unavailable!all-MiniLM-L6-v2". The name is preserved rather
// than replaced so the log still says which model *would* have been consulted.
// IsFallback reads it back. This is reported as a gap in core.
const FallbackPrefix = "unavailable!"

// IsFallback reports whether a journaled verdict was produced by the
// degraded path rather than by an actual embedding comparison.
func IsFallback(ev core.DedupEvaluated) bool {
	return strings.HasPrefix(ev.Model, FallbackPrefix)
}

// Observer receives one measurement per check. It is declared here, as the
// narrowest interface the checker needs, rather than importing the metrics
// package, so this package stays testable with no Prometheus registry and no
// dependency on another agent's wiring. internal/obs.Metrics satisfies it.
type Observer interface {
	ObserveDedup(activityType string, verdict core.DedupVerdict, seconds float64)
}

// CheckRequest is one question: "is this activity, about to be scheduled at
// program-order index K, a repeat of something this run already did?".
type CheckRequest struct {
	ShardID      core.ShardID
	WorkflowID   string
	RunID        core.RunID
	K            int
	ActivityType string
	Input        json.RawMessage

	// Thresholds overrides the checker's defaults for this one call. A zero
	// value means "use the checker's". Whatever is actually used is copied
	// into the event, so a later threshold change cannot rewrite the past.
	Thresholds core.Thresholds

	// KBySeq translates a candidate's scheduling event seq into the
	// program-order index K that workflow code reasons about.
	//
	// core.Intent records ScheduledSeq while core.DedupEvaluated reports
	// MatchedK, and core provides no mapping between them, so the caller —
	// which is replaying history and therefore already knows both — supplies
	// it. When it is nil or has no entry, the seq is reported as-is, which is
	// still a usable pointer into history. Reported as a gap in core.
	KBySeq map[core.Seq]int
}

// Checker computes semantic dedup verdicts. One instance is shared by every
// workflow task on a node; it holds no per-workflow state.
type Checker struct {
	Embedder   core.Embedder
	Intents    core.IntentStore
	Thresholds core.Thresholds
	Clock      core.Clock

	// Observer is optional. A nil Observer means no metrics, not a panic,
	// because the checker must work in a unit test with no registry.
	Observer Observer

	// RenderIntent turns one activity's input into the sentence that gets
	// embedded. It is optional, and nil falls back to the generic
	// type(canonical args) form.
	//
	// The hook exists because the generic form is wrong for exactly the
	// activities this mechanism targets. A tool call arrives as an envelope —
	// {"tool":"BookFlight","args":"BLR to DEL Friday morning"} — and embedding
	// the whole envelope buries the one field that carries meaning under
	// constant JSON scaffolding that is byte-identical across every call of
	// that type. Two genuinely different bookings then look similar because
	// most of what was embedded was punctuation and key names. An activity
	// that knows its own shape can hand over just the sentence.
	RenderIntent func(activityType string, input json.RawMessage) (string, error)
}

// NewChecker builds a checker with the benchmarked default thresholds.
func NewChecker(emb core.Embedder, intents core.IntentStore) *Checker {
	return &Checker{
		Embedder:   emb,
		Intents:    intents,
		Thresholds: core.DefaultThresholds(),
		Clock:      core.SystemClock{},
	}
}

// Check produces the verdict that will be journaled.
//
// The returned core.DedupEvaluated is always fully populated — similarity,
// matched step, both thresholds, model and the exact intent text that was
// compared — even for a DISTINCT verdict and even for the degraded path. That
// completeness is the point: an auditor holding only the event log must be
// able to reconstruct why an action was or was not taken, without access to
// the model, the configuration, or the candidate rows as they stood that day.
//
// An error is returned only when the request itself is unusable (arguments
// that are not valid JSON) or when the candidate store is unreachable. A model
// that is down is not an error — see the fallback below.
func (c *Checker) Check(ctx context.Context, req CheckRequest) (core.DedupEvaluated, error) {
	start := c.now()

	th := req.Thresholds
	if th == (core.Thresholds{}) {
		th = c.Thresholds
	}
	if th == (core.Thresholds{}) {
		th = core.DefaultThresholds()
	}

	text, err := c.renderIntent(req.ActivityType, req.Input)
	if err != nil {
		return core.DedupEvaluated{}, err
	}

	model := ""
	if c.Embedder != nil {
		model = c.Embedder.Model()
	}

	// Base is the answer we give when nothing matches. Every early return
	// builds on it, so no exit path can leave a field unset.
	base := core.DedupEvaluated{
		K:          req.K,
		Verdict:    core.VerdictDistinct,
		MatchedK:   NoMatch,
		Similarity: 0,
		TauHigh:    th.TauHigh,
		TauLow:     th.TauLow,
		Model:      model,
		IntentText: text,
	}

	if c.Embedder == nil {
		// No model configured at all is the same situation as a model that is
		// down, and is handled the same way rather than failing the workflow.
		ev := base
		ev.Model = FallbackPrefix + model
		c.observe(req.ActivityType, ev.Verdict, start)
		return ev, nil
	}

	vec, err := c.Embedder.Embed(ctx, text)
	if errors.Is(err, core.ErrEmbeddingUnavailable) {
		// Degrade, never fail. A workflow that has been running for three weeks
		// must not die because a model server restarted. DISTINCT is the safe
		// direction: the activity runs again, and the activity's own exact
		// idempotency key still protects it at the worker. The alternative,
		// guessing DUPLICATE, would skip real work on no evidence.
		ev := base
		ev.Model = FallbackPrefix + model
		c.observe(req.ActivityType, ev.Verdict, start)
		return ev, nil
	}
	if err != nil {
		return core.DedupEvaluated{}, fmt.Errorf("semantic: embed intent: %w", err)
	}

	cands, err := c.Intents.Candidates(ctx, req.ShardID, req.WorkflowID, req.RunID, req.ActivityType)
	if err != nil {
		// The candidate store is the engine's own database. If it is
		// unreachable the surrounding transaction is doomed anyway, so this is
		// a real error rather than something to paper over.
		return core.DedupEvaluated{}, fmt.Errorf("semantic: load candidates: %w", err)
	}
	if len(cands) == 0 {
		// First invocation of this activity type in this run. There is nothing
		// it could be a repeat of.
		c.observe(req.ActivityType, base.Verdict, start)
		return base, nil
	}

	vecs := make([][]float32, len(cands))
	for i, cand := range cands {
		vecs[i] = cand.Embedding
	}
	idx, sim := BestMatch(vec, vecs)

	ev := base
	ev.Similarity = sim
	ev.Verdict = th.Classify(sim)
	if idx >= 0 {
		ev.MatchedK = req.matchedK(cands[idx].ScheduledSeq)
	}
	// A DISTINCT verdict points at nothing: naming a "match" the engine
	// explicitly rejected would mislead whoever reads the log later.
	if ev.Verdict == core.VerdictDistinct {
		ev.MatchedK = NoMatch
	}

	c.observe(req.ActivityType, ev.Verdict, start)
	return ev, nil
}

// matchedK maps a candidate's scheduling seq back to its program-order index.
func (r CheckRequest) matchedK(seq core.Seq) int {
	if k, ok := r.KBySeq[seq]; ok {
		return k
	}
	return int(seq)
}

func (c *Checker) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	return time.Now().UTC()
}

func (c *Checker) observe(activityType string, v core.DedupVerdict, start time.Time) {
	if c.Observer == nil {
		return
	}
	c.Observer.ObserveDedup(activityType, v, c.now().Sub(start).Seconds())
}

// renderIntent prefers the caller's per-activity renderer and falls back to the
// generic form. A renderer that returns an empty string is treated as having no
// opinion rather than as an instruction to embed nothing.
func (c *Checker) renderIntent(activityType string, input json.RawMessage) (string, error) {
	if c.RenderIntent != nil {
		text, err := c.RenderIntent(activityType, input)
		if err != nil {
			return "", err
		}
		if text != "" {
			return text, nil
		}
	}
	return IntentText(activityType, input)
}
