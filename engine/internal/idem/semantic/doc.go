// Package semantic decides whether an activity the workflow is about to
// schedule *means the same thing* as one it already ran in this run, even when
// the arguments are not byte-identical.
//
// # Why this is not the same problem as exact idempotency
//
// Exact idempotency asks "are these arguments the same bytes?". That is a
// closed question with one right answer. The semantic check asks "do these two
// requests mean the same thing?", and the answer comes from a model, a
// similarity score and a threshold. It is a judgement, not a fact.
//
// # Why the check must run on the workflow path, never inside the worker
//
// The engine's whole correctness argument is replay: to recover a workflow we
// re-run its code against its recorded history, and the code must make exactly
// the same choices it made the first time. That only holds if every input to
// those choices is either pure code or something already written in the log.
//
// A semantic verdict is neither. It depends on three things that can all move
// underneath us: which embedding model is loaded, what floating-point
// threshold is configured, and which prior invocations happen to be in the
// candidate set at that moment. Put the check inside the activity worker and
// there are only two possible designs, and both are broken:
//
//   - Recompute the verdict during replay. Then a model upgrade, a threshold
//     tweak, or one extra row in the candidate set can flip DUPLICATE to
//     DISTINCT, replay takes a different branch than the original run did, and
//     history no longer describes the execution. That is divergence.
//   - Skip the check during replay. Then the decision that caused an activity
//     to be skipped exists nowhere. Nobody can later answer "why was this
//     payment not charged?". That is irreproducibility.
//
// So the verdict is computed exactly once, on the workflow path, at the moment
// the activity is first scheduled, and is immediately journaled as a
// core.DedupEvaluated event inside the same atomic commit as the decision it
// justified. Replay then reads the verdict out of history and performs no
// embedding call at all. Recovery therefore never depends on the model server
// being reachable, and a skipped action stays explainable from the log alone —
// the event carries the similarity, the matched step, both thresholds, the
// model name and the exact intent text that was compared.
//
// # Why the verdict has three values and not two
//
// Right at the threshold the model is least confident, and that is precisely
// where a binary yes/no forces the engine to guess. core.DedupVerdict
// therefore has a middle band, ESCALATE, which hands the decision to a human
// instead of silently skipping or silently duplicating an irreversible action.
package semantic
