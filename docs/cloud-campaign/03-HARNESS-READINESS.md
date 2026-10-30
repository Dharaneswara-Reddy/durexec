# Harness readiness — defects found before any money was spent

2026-10-30. The EKS pilot harness (`engine/scripts/aws/eks_*.sh`,
`collect_eks_evidence.sh`, the EKS overlay and the DB host bootstrap) was
reviewed against the code it drives. Each defect below would have failed the
first paid session, or would have produced a number or a verdict that looked
right and was not. All are fixed in commit `8487d3e` and later.

**No AWS resource was used to find or verify any of this.** The session's AWS
credentials are placeholders (`InvalidClientTokenId`). Verification used
PostgreSQL 16 (`pgvector/pgvector:pg16`, the cloud image) in Docker, the real
`engined` and `loadgen` binaries, and a `kubectl` test double. A local `kind`
cluster could not start in this container (nested cgroups), so nothing was
verified against a real Kubernetes API. The **Untested against Kubernetes**
column says exactly what remains unproven.

## Blocking defects (the session would have stopped)

| # | Defect | Effect on a paid run | Fix | Verified by | Untested against Kubernetes |
|---|---|---|---|---|---|
| D1 | The engine image is `distroless/static`: no `sh`, `wget` or `psql`. `eks_smoke.sh` S4–S7 and the collector's metrics and DB sections all ran `kubectl exec <engine> -- sh -c ...` | Smoke FAILS on a healthy cluster → runbook stop condition 4 → teardown. Collector marks metrics "MISSING (required)" every time | `_k8s.sh`: HTTP via the API server's pod proxy (`kubectl get/create --raw .../pods/<p>:8080/proxy/...`); SQL via a probe pod (`pgvector:pg16`) on the harness node, reading `DATABASE_URL` from the engine's own Secret | `docker run --entrypoint sh distroless/static` → `exec: "sh": executable file not found`. Smoke S2–S9 and the collector run end to end against a real engine and DB through the test double | Pod-proxy reachability on EKS (depends on the cluster SG allowing control plane → pod :8080; managed node groups share it by default). `kubectl create --raw` POST through the proxy |
| D2 | `eks_deploy.sh` verified the DB host by searching the init container's **command**; the overlay passes it as **env** `DB_HOST` | `FATAL: init container does not reference the external database host` on every correct render, after the ~12 min infra build | Check env `DB_HOST` (or the command) | The original check run on the original render fails; the fixed check passes for r1/r3/r4 and still rejects a wrong host and a 2-name ring under 4 replicas | — (pure rendering) |
| D3 | `_common.sh` forced `AWS_PROFILE=aiops-deploy` | With credentials in the environment and no such profile, every `aws` call fails with "profile not found" | Default the profile only when no env credentials exist; safe under `set -u` | Sourced under `set -u` with the profile unset | — |

## Silent-misreport defects (green but wrong)

| # | Defect | Effect | Fix | Verified by |
|---|---|---|---|---|
| D4 | The EKS overlay inherited base `replicas: 2` and a 2-name ring (`NODE_COUNT=2`) | The "3-engine" pilot ran **2 engines**; S1/S2 passed; every result would have been labelled 3-node | Replicas default to the number of engine-role nodes; ring, `NODE_COUNT`, replicas, `SHARD_COUNT`, workers and `SYNC_COMMIT` are written together and verified; smoke S2b fails on any mismatch | Render patch tested at 1/3/4 replicas; tampered ring rejected |
| D5 | Generator CPU came from `kubectl top` (no metrics-server on the pilot) **after** the Job finished | `gen_cpu_cores` always empty → `GENERATOR_SATURATED` could never fire → a generator-bound run would be published as engine capacity | kubelet Summary API (`/api/v1/nodes/<n>/proxy/stats/summary`) sampled every 3 s **during** the run; cumulative CPU-ns give exact cores; a run with unmeasured generator CPU is `INCONCLUSIVE_generator_cpu_unmeasured` | Reducer tested on synthetic samples (exact values) and on real process CPU through the test double; forced-threshold run gives `GENERATOR_SATURATED` |
| D6 | Engine CPU was summed from one `kubectl top` sample taken after the run | Idle CPU reported as load CPU | Per-pod CPU over the generator's own start/finish window | as D5 |
| D7 | The DB host started PostgreSQL without `pg_stat_statements`, `track_io_timing`, `track_wal_io_timing`, `log_lock_waits`, `log_checkpoints` | Cloud DB figures would come from fewer sources than the lab's, so a local-vs-cloud comparison compares different measurements | Bootstrap flags added; `eks_deploy.sh` creates the extension after every reset; smoke S9 checks it | The exact bootstrap `docker run` line boots PG16; extension creates; probe role is superuser |
| D8 | The collector read `/metrics` from **one** pod | One process described as the cluster | Every pod; healthz per pod | Collector run through the test double |
| D9 | No drain before judging "no lost work" | In-flight workflows at the snapshot instant read as lost, or hide loss | Drain up to `DRAIN_WAIT` and **record** whether it drained; stuck-RUNNING > 5 min counted | Collector run: `drained=yes`; runner rows showed `RUNNING:25` at job end, which is why |
| D10 | Zero matching journal rows make every invariant 0 | A run whose ID prefix matched nothing would "pass" | `journal_runs < completed` is a violation; missing journal evidence → `INCONCLUSIVE_no_correctness_evidence` | Forced DB outage run gives that verdict |
| D11 | `eks_publish.sh` tagged images with the short SHA from a **dirty** tree | Image provenance claims a commit the image does not contain | Refuse a dirty tree unless `TAG` is explicit; record digests in `images.txt` | syntax/shellcheck only (needs ECR) |
| D12 | `eks_teardown.sh` exited 0 after a failed `terraform destroy`; the sweep filtered EC2 by `Project=durexec`, which **EKS-launched node instances do not carry** (no launch template, so `default_tags` do not reach them) | "Nothing billing" could be printed while node instances or a VPC survived | Destroy exit code propagated; sweep adds `eks:cluster-name=durexec-bench` instances, EIPs, ENIs in the pilot VPC, the VPC itself, EKS log groups | syntax/shellcheck only |
| D13 | Cost reconciliation filtered Cost Explorer by `Project=durexec` | Node-hour cost (the largest item) excluded for the same reason as D12 | Cost report must use `eks:cluster-name` **and** `Project` (see `README.md` §cost) | — |
| D14 | loadgen arrival verdict said p50 was "within 5 %" of the declared floor when it was 25 % **below** it; the floor is a declared constant (2.01 s), not measured on the deployment | A floor declared too LOW makes an idle engine look SATURATED; demonstrated: `FLOOR=1.2s` gave loadgen `SATURATED` at 13 % engine CPU | Message corrected and a recalibration note added (`TestVerdictFlagsAFloorDeclaredTooHigh`); the runner labels loadgen `SATURATED` with no resource ≥ 85 % as `SATURATED_UNATTRIBUTED`; matrix stage C0 measures the floor first | go test; runner end to end |
| D15 | `set -euo pipefail` + a `grep` that matches nothing inside `$(...)` aborts the runner | A run with a legitimately missing series would die and write no row, which is precisely the case that must be recorded | `{ grep ... || true; }` in every lookup helper | Missing-series run writes its row |
| D16 | Fault oracle's expiry check compared each shard with **any** shard's previous token | `t_expire` could be early or late when shards hold different tokens | Per-shard positional comparison | Three token cases against live lease rows |

## Additional instrumentation (new measurements, not fixes)

| Added | Gives | Validated against |
|---|---|---|
| `sql/pgstat_snapshot.sql` + `pgstat_delta.py` | commits/s, rollbacks, WAL MB/s, FPI, checkpoints, buffer hit ratio, statement mean latency, deadlocks, temp bytes, peak connections, lock waits, **wait-event profile**, DB-host CPU / iowait / steal, disk IOPS / MB/s / utilisation | pgbench load on PG16: 2 976 commits/s, 4 148 write IOPS, 55.6 % CPU, profile `LWLock:WALWrite 31 %, IO:WALSync 8 %, Lock:transactionid 8 %` — plausible and internally consistent |
| `metrics_delta.py` | per-run engine deltas over every pod: pool acquires / empty / wait, task failures, fencing rejections, lease changes (not renewals), replays, GC pause, goroutines, queue age | Real engine burst: 23 287 acquires, 119 empty, 1.95 s wait, 8 renewals |
| `sql/run_latency_and_invariants.sql` | **latency decomposition from the journal** (first decision, activity sched→done, decision turnaround, timer lag, end-to-end; p50/p90/p95/p99/max) and invariants (duplicate seq, duplicate activity completion, orphan completion, seq gaps, multiple terminals, status counts, pending tasks) for exactly one run's workflows | 113-run and 268-run local loads; invariants 0 |
| Probe RTT | harness → DB `select 1` round trip (psql `\timing`) | local: 0.059 ms p50 |
| `MODE=arrival` in the runner | open-loop points with offered rate, loadgen verdict, dropped arrivals, schedule lag, in-flight slope | runner end to end |
| `eks_fault.sh`, `eks_fault_trial.sh` | scoped injectors (delete, SIGKILL, SIGSTOP/CONT, DB-only netem, NetworkPolicy partition with an **enforcement proof**, connection sever, drain, EC2 terminate, DB reboot) and a trial runner with the Phase 9 oracle on the PG clock, rotation, cool-down, crash-loop detection, drain and invariants | oracle SQL against live lease rows; **injectors are untested against a cluster** |

## Still unproven until the first cloud session

1. Pod-proxy HTTP and `create --raw` POST on EKS (D1).
2. The kubelet Summary API through the node proxy on EKS 1.37 (D5).
3. `kubectl debug node --profile=sysadmin` pods can signal `engined`
   (hostPID) and run `tc` in the host network namespace (F3–F5).
4. NetworkPolicy enforcement by the VPC CNI. `verify-policy` proves or
   disproves it on the cluster before F6 is allowed to run.
5. `pg_read_file('/proc/...')` from the EC2-hosted container (it worked in
   Docker locally; the same image and user).

Each is checked by stage G before any measurement. A failure there stops the
campaign for a fix rather than producing uninterpretable rows.

## Local observations made while validating (NOT results)

These came from a single engine node on this container with 16–64
concurrency. They are recorded because they shaped the matrix, **not** as
performance figures:

- Timer lag p50 **651–709 ms**, p99 **1.07–1.19 s** for a 50 ms durable timer
  → hypothesis H3.
- About **104 pool acquisitions per completed workflow** → most database
  traffic is polling.
- `SELECT fencing_token FROM shard_leases ... FOR UPDATE` among the top
  statements by count → hypothesis H2.
- About **47 disk write I/Os per completed workflow** at low concurrency →
  hypothesis H7 against gp3's 3 000 IOPS.
