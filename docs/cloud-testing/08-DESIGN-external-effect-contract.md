# Design — cross-run idempotency and external-effect compensation

**Status: PROPOSAL. None of this is implemented.** It is written down because Phase 7
produced two findings that the current code cannot answer, and inventing an answer in the
engine would misrepresent what is built. Each section states the question, the proposal, and
what a decision would cost.

## 1. The two keys are different things, and conflating them is the bug

| | Workflow-start idempotency key | External-operation idempotency key |
|---|---|---|
| Answers | "is this the same *request to run a workflow*?" | "is this the same *side effect*?" |
| Today | the `workflow_id` itself — `executions` is keyed `(shard_id, workflow_id)` with no run id, so a second start loses the race and is told about the winner | `sha256(scheme ‖ workflow_id ‖ run_id ‖ k)`, journaled in the scheduled event |
| Scope | one business key, forever | one **run**, one program-order index |
| Proven | Yes — Phase 7 live check: `accepted=1, already_open=2` | Yes — Phase 7 A/F/G: one effect per key |

The gap: the operation key contains `run_id`. Within a run it is exactly right. Across runs it
is **deliberately** different, and that is correct for a genuinely new business request and
wrong for a re-run of the same one.

## 2. Should a key be reused? One answer per case

| Case | Reuse the operation key? | Why |
|---|---|---|
| Same-run retry after a transient error | **Yes** — already does | The retry *is* the same operation; this is what makes an ambiguous timeout safe |
| Replay after a crash | **Yes** — already does | The key is read from history, never recomputed |
| Same-run sequence race (two workers, one `k`) | **Yes** — already does | One loses on the events primary key; the effect is one |
| **Explicit restart / retry-failed-workflow as a new run** | **Yes — NOT implemented** | The operator's intent is "finish the job I already asked for", so the payment that already landed must not land again |
| **Continue-as-new** | **Yes for already-completed steps, no for new ones — NOT implemented** | A continued run is the same logical job; its *new* steps are new operations |
| A genuinely new business request | **No** — already correct | Two orders must be two payments |
| Same key, different payload | **Must be refused** — NOT implemented | See §4 |

**What a safe design needs:** an operation key derived from a caller-supplied *business
operation identity* rather than from `run_id`. The smallest form:

```
operation_key = sha256(scheme ‖ tenant ‖ workflow_type ‖ business_key ‖ operation_name ‖ k)
```

where `business_key` defaults to `workflow_id` and `operation_name` is the activity type.
Dropping `run_id` is what makes it stable across related runs; including `workflow_type` and
`tenant` is what stops two tenants or two workflow types colliding on a shared business key.

**Compatibility.** This changes the key format, so it must be **additive**: a new
`dedup_scheme` version (`dx2:`) alongside the existing `dx1:`, chosen per activity
registration, with `dx1:` remaining the default. A run started under `dx1:` keeps `dx1:`
forever, because the key lives in its history. Silently switching schemes would give a
re-delivered in-flight activity a *different* key from the one its first attempt used —
turning a safe retry into a duplicate effect. **That is the single most dangerous change in
this document and the reason it is not being made casually.**

## 3. Who supplies the identity

The caller must, because only the caller knows whether two requests are the same business
intent. That is an **API/SDK decision**, so per the phase brief it is proposed, not built:
an optional `business_key` on the start request, defaulting to `workflow_id`, carried in the
`WorkflowExecutionStarted` payload beside `code_version` (which already works exactly this
way). The engine must never derive it from wall-clock time, a UUID, or the payload bytes —
all three would make "the same operation" unrecognisable across runs.

## 4. Same key, different payload

The recipient must reject it, and the engine must treat that rejection as **non-retryable**.
A key that has been reused with different content means the caller has a bug, and silently
honouring the first payload would hide it while silently honouring the second would double
the effect. `extsim` currently does **neither** — it returns the remembered response and
ignores the new payload, which is lenient and therefore unsafe as a model of a strict
provider. A conforming design records the payload digest beside the key and answers `409`
on a mismatch. **Not implemented, and `extsim` would need to change to test it.**

## 5. Retention and expiration

A key is only useful while the recipient still remembers it. `extsim` has a `KeyTTL`, real
providers typically remember for 24h–7d. The engine's retry budget must therefore be
**shorter than the recipient's key retention**, or a late retry becomes a fresh effect. This
is a contract term that has to be stated per integration; the engine has no way to discover
it. Nothing today checks or records it.

---

# Compensation for external effects

Phase 7 scenario E: an external payment succeeded, a later step failed, the workflow ended
`FAILED`, and **0 compensation events ran** — because `ExternalPayment` registers no
compensation. The eight questions, answered honestly:

| Question | Answer today | Proposal |
|---|---|---|
| How does a successful effect register its compensation? | It does not. `activity.Definition.Compensation` exists and is used by the delivery demo, but `PayDefinition` deliberately leaves it empty | Register the compensation at the same time as the activity, and let the **engine** record a compensation *intent* in the same transaction as the result event |
| How does the compensation get its own key? | No mechanism | Derive it from the effect's key: `compensation_key = sha256(scheme ‖ "compensate" ‖ effect_key)`. Stable, unique, and derivable from history alone — so replay reproduces it |
| How does it relate to the effect key? | — | One-to-one and *derived*, never independent. An independently generated key would differ on replay and refund twice |
| Across retries, crashes, replay? | — | Identical to a forward activity: journal the compensation's result, read the key from history. A compensation is an external effect and needs the same treatment, not a weaker one |
| Compensation succeeds, response lost? | — | Exactly the Phase 7 scenario A case, and the same answer: retry under the same key, relying on recipient dedup. No new mechanism; the same conditional guarantee |
| How are compensation failures represented? | `COMPENSATING_FAILED` status exists | Keep it, and make it terminal-with-alarm. A failed compensation is the one state that genuinely needs a human |
| Original failure vs compensation failure? | Distinguished by status today | Preserve. `FAILED` means forward work failed and rollback succeeded or was unnecessary; `COMPENSATING_FAILED` means money may be stranded |
| **Can compensation run while the effect's outcome is ambiguous?** | — | **No, and this is the hard one.** Refunding a payment that may never have happened can *create* a negative effect. The only safe sequence is: resolve the ambiguity first — retry the forward operation under its key until the recipient gives a definite answer — and only compensate a **confirmed** effect. That requires the forward call to be idempotent *and* queryable, and the engine has no notion of querying a recipient |

**Why nothing is implemented.** Steps 1–6 are mechanical and the architecture already
supports them. Step 8 is not: a correct compensation model needs a way to resolve an
ambiguous outcome, which means either a recipient that can be *asked* about a key, or a
durable intent record plus an operator reconciliation path. Building the mechanical parts
without that would produce a system that compensates confidently on uncertain evidence —
worse than today's honest refusal to compensate at all.

**Regression guarantee that must hold meanwhile:** a failed workflow must never appear to
have undone an external effect it did not undo. Phase 7's evidence for this is scenario E
(`FAILED`, 0 compensation events, effect still in the ledger) and it is preserved as a
recorded result rather than asserted.
