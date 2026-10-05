package core

// IdempotencyMode is declared per activity. Exact is the default; Semantic is
// opt-in because its failure mode is asymmetric — see DedupVerdict.
type IdempotencyMode string

const (
	ModeExact    IdempotencyMode = "Exact"
	ModeSemantic IdempotencyMode = "Semantic"
)

// DedupVerdict is deliberately three-valued. Forcing a binary decision at the
// threshold is exactly where the model is least confident.
type DedupVerdict string

const (
	// VerdictDuplicate: skip the activity, reuse the recorded result.
	VerdictDuplicate DedupVerdict = "DUPLICATE"
	// VerdictEscalate: uncertain — ask a human rather than guess.
	VerdictEscalate DedupVerdict = "ESCALATE"
	// VerdictDistinct: genuinely new work, schedule it.
	VerdictDistinct DedupVerdict = "DISTINCT"
)

// Thresholds define the escalation band. TauLow <= TauHigh always.
type Thresholds struct {
	TauLow  float64
	TauHigh float64
}

// DefaultThresholds are the values benchmarked in the evaluation.
func DefaultThresholds() Thresholds { return Thresholds{TauLow: 0.85, TauHigh: 0.90} }

// Classify maps a similarity score onto the three bands.
func (t Thresholds) Classify(sim float64) DedupVerdict {
	switch {
	case sim >= t.TauHigh:
		return VerdictDuplicate
	case sim < t.TauLow:
		return VerdictDistinct
	default:
		return VerdictEscalate
	}
}

// Intent is one canonicalised activity invocation with its embedding.
type Intent struct {
	ShardID      ShardID
	WorkflowID   string
	RunID        RunID
	ScheduledSeq Seq
	ActivityType string
	IntentText   string
	Embedding    []float32
}
