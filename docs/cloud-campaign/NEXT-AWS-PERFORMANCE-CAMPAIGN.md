# Next AWS performance campaign — execution sheet

**Do not execute until the owner approves a new spend cap.** The first-session
$10 authorization is spent/closed and does not authorize this campaign. The
current estimate for A–H plus teardown is **17.8 h / $11.57** at $0.65/h; the
separate fault session adds **3.5 h / $2.28**. These are estimates, not billing
caps. Follow [the experiment matrix](02-EXPERIMENT-MATRIX.md) for full
durations, repeats, metrics, validity rules, interpretation and stage stop
conditions. No HA, exactly-once external-effect, or production-readiness claim
is supported by this pilot.

## Before Stage A

Run all Kubernetes commands from the Fedora laptop on the allowed public IP.
Use a new campaign tag/ID and evidence directory; preserve existing raw runs.
Verify the approved commit, AWS account/region, public IP allowlist, clean
tracked source, local `terraform.tfvars`, Terraform plan and available budget.
The EC2 instance defaults are engine `m7i-flex.large`, harness
`c7i-flex.large`, DB `m7i-flex.large`; record actual type and AZ for each node
and keep them fixed. Flex scheduling variance limits comparisons.

Commands below are the planned sequence only; they have not been run for this
next campaign. Stop on any failed gate. Any plan drift requires review and a
new approval before apply.

## Stage A — bring-up, smoke, policy

```bash
cd engine
export AWS_PROFILE=aiops-deploy AWS_REGION=us-east-1
aws sts get-caller-identity
curl -4 https://checkip.amazonaws.com
terraform -chdir=deploy/aws/eks init
terraform -chdir=deploy/aws/eks validate
terraform -chdir=deploy/aws/eks plan -out=eks.tfplan
terraform -chdir=deploy/aws/eks show eks.tfplan
# After plan and spend approval only:
export DUREXEC_CONFIRM=i-understand-this-costs-money
scripts/aws/eks_publish.sh
terraform -chdir=deploy/aws/eks apply eks.tfplan
aws eks update-kubeconfig --region us-east-1 --name durexec-bench
kubectl get nodes -L durexec.io/role,node.kubernetes.io/instance-type,topology.kubernetes.io/zone
scripts/aws/eks_deploy.sh
scripts/aws/eks_smoke.sh
scripts/aws/eks_fault.sh verify-policy
scripts/aws/collect_eks_evidence.sh next-performance-campaign
```

Record the published digests. Continue only if cluster/deployment, smoke, and
policy checks pass and all nodes match the approved configuration.

## Stage B — idle baselines

No load job may run. For each configuration below, deploy the configuration,
then run three 120-second baselines (3-second sample interval; script checks
quiescence and unchanged execution count). Preserve every run directory.

```bash
# For each setting, use the deploy command, then repeat the idle command 3x:
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=2 ACTIVITY_WORKERS=4 scripts/aws/eks_deploy.sh
DUR_SECONDS=120 SAMPLE_EVERY=3 scripts/aws/eks_idle_baseline.sh
# Repeat for S16 w4x8, S16 w8x16, S32 w2x4, and S64 w1x2:
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=4 ACTIVITY_WORKERS=8 scripts/aws/eks_deploy.sh
DUR_SECONDS=120 SAMPLE_EVERY=3 scripts/aws/eks_idle_baseline.sh
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=8 ACTIVITY_WORKERS=16 scripts/aws/eks_deploy.sh
DUR_SECONDS=120 SAMPLE_EVERY=3 scripts/aws/eks_idle_baseline.sh
ENGINE_REPLICAS=3 SHARD_COUNT=32 WORKFLOW_WORKERS=2 ACTIVITY_WORKERS=4 RESET_DB=i-understand-this-deletes-pilot-data scripts/aws/eks_deploy.sh
DUR_SECONDS=120 SAMPLE_EVERY=3 scripts/aws/eks_idle_baseline.sh
ENGINE_REPLICAS=3 SHARD_COUNT=64 WORKFLOW_WORKERS=1 ACTIVITY_WORKERS=2 RESET_DB=i-understand-this-deletes-pilot-data scripts/aws/eks_deploy.sh
DUR_SECONDS=120 SAMPLE_EVERY=3 scripts/aws/eks_idle_baseline.sh
```

Also baseline the selected Stage G shard/worker setting at replicas 1 and 2,
then return to 3 replicas. When changing shard count, preserve/collect evidence
first and use the explicit reset gate shown above. At each setting run the idle
command 3 times. The exact selected setting is written to the matrix results
after G; do not guess it in advance.

## Stage C — calibration

```bash
# Closed-loop c=1; run 3 trials, 300 s measured plus 15 s warm-up.
for TRIAL in 1 2 3; do EXP=C-calibration KIND=perf MODE=scaling CONC=1 DUR=315s WARMUP=15s MIN_SAMPLES=150 TRIAL="$TRIAL" scripts/aws/eks_loadgen.sh; done
# Set FLOOR to the median c=1 p50 and RATE to 0.25 × measured completed wf/s.
# Run the resulting open-loop point 3 times (300 s measured + 15 s warm-up).
for TRIAL in 1 2 3; do EXP=C-low-arrival KIND=perf MODE=arrival RATE=RATE_VALUE FLOOR=FLOOR_SECONDS DUR=315s WARMUP=15s MIN_SAMPLES=150 TRIAL="$TRIAL" scripts/aws/eks_loadgen.sh; done
```

Do not advance unless the low-arrival latency is within 10% of the floor and
the load generator has headroom. Use this floor for Stage E's `FLOOR` value.

## Stage D — closed-loop concurrency ladder

```bash
# One 120 s measured screen plus 15 s warm-up at each step.
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=4   scripts/aws/eks_loadgen.sh
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=16  scripts/aws/eks_loadgen.sh
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=32  scripts/aws/eks_loadgen.sh
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=64  scripts/aws/eks_loadgen.sh
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=128 scripts/aws/eks_loadgen.sh
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=256 scripts/aws/eks_loadgen.sh
EXP=D-ladder KIND=perf MODE=scaling DUR=135s WARMUP=15s CONC=512 scripts/aws/eks_loadgen.sh
```

Evaluate each row before issuing the next; stop at the first throughput fall,
p99 above 10× floor, or generator saturation. Define the observed knee and
run 3 valid trials total at the knee and one adjacent step on either side.
Count a screening trial only when it has >=1,000 completions and passes all
validity checks. For additional trials, use this command with each selected
concurrency and `TRIAL=1,2,3` (do not duplicate a trial already counted):

```bash
EXP=D-knee KIND=perf MODE=scaling CONC=KNEE_CONCURRENCY DUR=TOTAL_SECONDS WARMUP=15s MIN_SAMPLES=1000 TRIAL=TRIAL_NUMBER scripts/aws/eks_loadgen.sh
```

Each trial needs at least 1,000 completions: set `DUR` to at least
`ceil(1000 / expected_completed_wf_per_s)` plus `WARMUP=15s`. Do not run later
ladder commands after a stop condition.

## Stage E — open-loop arrival curve

Calculate the five rates from the D closed-loop knee throughput. Run three
trials at each; the `DUR` includes warm-up, so use at least
`ceil(1000 / (1.1 × knee_wf_per_s)) + 15` seconds for every point (increase
for lower rates; cap measured time at 1,800 seconds, then mark under-sampled
INCONCLUSIVE).

```bash
# For each rate 0.5, 0.75, 0.9, 1.0, 1.1 × knee, and TRIAL=1,2,3:
EXP=E-arrival KIND=perf MODE=arrival RATE=RATE_VALUE FLOOR=FLOOR_SECONDS DUR=TOTAL_SECONDS WARMUP=15s MIN_SAMPLES=1000 TRIAL=TRIAL_NUMBER scripts/aws/eks_loadgen.sh
```

## Stage F — worker sensitivity

At r3/S16, run each worker setting with its Stage B idle baseline. Find a
closed-loop knee using the Stage D ladder; then take 3 valid knee trials at
>=1,000 completions each using the Stage D knee command. Change only worker
counts between configurations.

```bash
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=2 ACTIVITY_WORKERS=4 scripts/aws/eks_deploy.sh
DUR_SECONDS=120 scripts/aws/eks_idle_baseline.sh
# Repeat at w4x8 and w8x16, then run the D ladder and knee repeats per setting.
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=4 ACTIVITY_WORKERS=8 scripts/aws/eks_deploy.sh
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=8 ACTIVITY_WORKERS=16 scripts/aws/eks_deploy.sh
```

## Stage G — shard sensitivity

Hold total configured workers at 192: S16/w4x8, S32/w2x4, S64/w1x2. Capture
actual ownership and `shard_skew` on every point; expected ring skews are
1.69/1.31/1.22. For every setting, collect its idle baseline, run the closed
ladder to bracket the knee, then run 3 valid knee trials with >=1,000
completions using the Stage D knee command.

```bash
ENGINE_REPLICAS=3 SHARD_COUNT=16 WORKFLOW_WORKERS=4 ACTIVITY_WORKERS=8 scripts/aws/eks_deploy.sh
# Then S32 and S64; collect prior evidence before each explicit DB reset.
ENGINE_REPLICAS=3 SHARD_COUNT=32 WORKFLOW_WORKERS=2 ACTIVITY_WORKERS=4 RESET_DB=i-understand-this-deletes-pilot-data scripts/aws/eks_deploy.sh
ENGINE_REPLICAS=3 SHARD_COUNT=64 WORKFLOW_WORKERS=1 ACTIVITY_WORKERS=2 RESET_DB=i-understand-this-deletes-pilot-data scripts/aws/eks_deploy.sh
```

## Stage H — replica sensitivity

Use the shard/worker selection from G and the same three engine nodes. Run
replicas 1, 2, and 3, collecting the matching B idle baseline and three
>=1,000-completion trials at each configuration's measured knee.

```bash
ENGINE_REPLICAS=1 SHARD_COUNT=G_SHARD_COUNT WORKFLOW_WORKERS=G_WORKFLOW_WORKERS ACTIVITY_WORKERS=G_ACTIVITY_WORKERS scripts/aws/eks_deploy.sh
ENGINE_REPLICAS=2 SHARD_COUNT=G_SHARD_COUNT WORKFLOW_WORKERS=G_WORKFLOW_WORKERS ACTIVITY_WORKERS=G_ACTIVITY_WORKERS scripts/aws/eks_deploy.sh
ENGINE_REPLICAS=3 SHARD_COUNT=G_SHARD_COUNT WORKFLOW_WORKERS=G_WORKFLOW_WORKERS ACTIVITY_WORKERS=G_ACTIVITY_WORKERS scripts/aws/eks_deploy.sh
```

For each, run the Stage D ladder/bracketing and three knee trials. Do not infer
replica scaling from a single point; use the Stage D knee command with
`CONC=KNEE_CONCURRENCY` and `TRIAL=1,2,3`. Every real `eks_*.sh` invocation records
stdout/stderr in the evidence command directory.

## Close-out after H

```bash
scripts/aws/collect_eks_evidence.sh next-performance-campaign
DUREXEC_CONFIRM=i-understand-this-costs-money scripts/aws/eks_teardown.sh
```

Verify AWS campaign resources are gone and capture teardown evidence before
writing conclusions. The fault campaign is a separately approved session;
its scenarios and acceptance criteria are in the matrix.
