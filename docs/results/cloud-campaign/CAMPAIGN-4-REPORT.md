# Cloud campaign 4 — mixed workloads and the generic duplicate check (AWS)

**Date:** 2026-11-01, 06:52–~08:58 UTC (~2 h 5 min). **Images:** `69c9d9e`.
**Infrastructure:** identical to campaigns 1–3 (fresh plan, 44 resources, same
set as campaign 1; cluster API restricted to the operator laptop's address).
Engine configured with `SEMANTIC_FIELDS=generic-v2-places`. Bring-up: smoke
10/10, NetworkPolicy enforced. Driver: `cmd/mixload` via `scripts/aws/eks_mixload.sh`.

## M1 — each workflow type alone (closed loop c=32, 120 s)

| Type | wf/s | p50 | p99 |
|---|---|---|---|
| DeliveryOrder (saga + timer) | 16.5 | 1.94 s | 3.07 s |
| MixPipeline (20 steps) | 15.3 | 2.04 s | 2.76 s |
| MixFanOut (8-wide + join) | 26.9 | 1.13 s | 2.24 s |
| MixApproval (waits for a signal) | 23.3 | 1.33 s | 1.94 s |
| MixRetry (2 failures per call) | 20.6 | 1.53 s | 2.35 s |
| DomainAgent (40 task kinds) | 101.4 | 0.31 s | 0.52 s |

## M2 — default mix (D30 P15 F15 A15 R10 G15), closed loop

| c | wf/s (trials) | p99 (all) | DB CPU |
|---|---|---|---|
| 32 | 22.4 | 2.86 s | 21 % |
| 64 | 50.2 | 2.45 s | 44 % |
| 128 | 96.0 / 94.4 / 99.3 | 2.5–3.0 s | 84–88 % |
| 256 | 109.9 / 102.6 / 108.8 | 11.4–12.9 s | 93–95 % |

Past c=128 the pipeline suffers most (p99 17–21 s): each of its 20 steps waits
again in the same database-bound queue.

## M3 — default mix, open loop

40, 60 and 80 wf/s sustained exactly (p99 2.35–2.55 s); 100 wf/s overloaded
(70.6 wf/s completed, p99 94.6 s).

## M4 — faults under the mix (c=64, 180 s)

* SIGKILL engined on durexec-1 at +75 s: p99 2.65 s, every run completed.
* SIGSTOP durexec-2 for 35 s (> 10 s lease): p99 of delivery and approval
  ~30.5 s, every run of every type completed.

## Correctness

20 measurement points, 172,280 runs: 0 duplicate (run, seq), 0 duplicate
activity completion, 0 orphan completion, 0 sequence gap, 0 multiple
terminal; every run COMPLETED, no pending tasks.

## Duplicate check inside the engine (DomainAgent, generic-v2+places)

M1 alone, 12,173 runs over all four corpora: repeats suppressed / distinct
actions suppressed — flight 888/1075, 0/1263; multidomain 853/1642, 11/1646;
heldout 720/1642, 11/1647; heldout2 898/1644, 32/1646. Matches the offline
evaluation (`docs/results/GENERIC-V2-EXTRACTOR.md`). 0 verdict/side-effect
mismatches in every point.

The MiniLM tie-break study ran on the operator in this campaign
(`docs/results/minilm-pair-cosines.csv`).

## Teardown and cost

In-cluster objects deleted from the operator, then `terraform destroy` from the
cloud session: **44 destroyed**, state empty. Read-only sweep: no EKS cluster,
pilot instances, volumes (none unattached), VPC, load balancers, secrets or log
groups. The one Elastic IP in the account (3.82.134.71) is associated with an
unrelated project's resource and was not touched. ~2 h 5 min at ~$0.75/h ≈ **$1.60**.
