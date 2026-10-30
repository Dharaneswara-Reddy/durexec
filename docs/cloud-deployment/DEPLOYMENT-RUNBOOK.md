# Deployment runbook — durexec on AWS

Every billable step refuses to run unless you set
`DUREXEC_CONFIRM=i-understand-this-costs-money` **in the same command**. That
is deliberate: nothing here can start an EC2 instance by tab-completion.

Target: the **`aiops-deploy`** profile, **us-east-1**. Confirm before you start
with `aws sts get-caller-identity --profile aiops-deploy`; every billable script
prints the account it is pointed at before acting.

That account also holds the **codearena, churn-ml-prod and aiops-demo**
projects. Nothing here touches them; the teardown is scoped to Terraform state
and never deletes by pattern.

(The account number is deliberately not written down here. It is not a
credential, but it lets someone enumerate role ARNs, and this repository is
headed for publication alongside the paper. The profile name is what you type
anyway.)

All paths are relative to `engine/`.

---

## 0. Local, safe, and already done once

Costs nothing, reaches no AWS write API. Re-run any time.

```bash
make aws-preflight     # memory, swap, disk, toolchain
make aws-validate      # terraform fmt + validate, and prove nothing is publicly bound
make aws-build         # build durexec-engine and durexec-extsimd for linux/amd64
make aws-package       # one checksummed archive under .artifacts/
make test-integration  # 1220 tests against a throwaway database
```

## 1. One-time setup

```bash
ssh-keygen -t ed25519 -f ~/.ssh/durexec-aws -C durexec-aws

cp deploy/aws/terraform/terraform.tfvars.example deploy/aws/terraform/terraform.tfvars
curl -s https://checkip.amazonaws.com        # your address
$EDITOR deploy/aws/terraform/terraform.tfvars   # set operator_cidr to that/32
```

`operator_cidr` has **no default** and rejects `0.0.0.0/0`. Terraform refuses
to plan without it rather than opening SSH to the world.

Optional but recommended — a $15 budget alarm (see COST-ESTIMATE.md).

## 2. Review the plan (read-only)

```bash
make aws-plan
```

Expect **18 resources** and exactly **one ingress rule**: tcp/22 from your
`/32`. If you see any other ingress rule, or `0.0.0.0/0` on an ingress, stop.

## 3. Provision — FIRST BILLABLE STEP

```bash
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/provision.sh
./scripts/aws/wait_ready.sh          # ~2 min; waits for the bootstrap marker
```

Billing starts at `apply`. ~$0.0510/hr.

## 4. Deploy the engine

```bash
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/deploy_stack.sh
./scripts/aws/smoke.sh
```

`deploy_stack.sh` re-runs the public-binding assertion, verifies the archive
checksum **on the instance** (a truncated `scp` must not become a half-loaded
image), and starts the stack **without** the `extsim` profile.

`smoke.sh` passes only when all 16 shards are owned exactly once, with no
unowned and no expired lease — not merely when `/healthz` returns 200.

## 5. Reach the dashboards

```bash
terraform -chdir=deploy/aws/terraform output -raw tunnel_command | bash
```

Then Grafana `http://localhost:3000`, Prometheus `:9090`, nodes `:8081–8083`.
Grafana's password: `terraform -chdir=deploy/aws/terraform output -raw grafana_password`.

There is no public HTTP port. The tunnel is the only way in, by design.

## 6. Tier 1 acceptance

```bash
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/acceptance_tier1.sh
```

Writes one CSV row per case, flushed, to `docs/results/cloud-tier1.csv`.
**Resumable** — re-run the identical command and it skips recorded cases. The
summary is recomputed from the file on disk, so a partial run still reports
validly. The resume command is in the CSV's own header.

## 7. Collect evidence — BEFORE disarming or restarting anything

```bash
./scripts/aws/collect_evidence.sh tier1-baseline
```

**Order matters.** Phase 7 and Phase 8 each destroyed their own evidence by
restarting a container to disarm a fault before reading the log that proved the
fault had fired. Collect first. Always.

## 8. Tier 2 — durability (see ACCEPTANCE-MATRIX.md for expected results)

```bash
# Backup, inside the instance
ssh -i ~/.ssh/durexec-aws ec2-user@$(terraform -chdir=deploy/aws/terraform output -raw public_ip)
set -a; . /opt/durexec/.env; set +a
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  pg_dump -U durexec -d durexec -Fc -f /tmp/durexec.dump
docker exec durexec-postgres-leader-1 pg_restore -l /tmp/durexec.dump | head

# Restore into a SEPARATE database and compare row counts
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  createdb -U durexec durexec_restore
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" durexec-postgres-leader-1 \
  pg_restore -U durexec -d durexec_restore /tmp/durexec.dump
```

**Instance loss (the test minikube could not do).** Note the `data_volume_id`
output, set `data_volume_retain_on_destroy = true`, `terraform destroy`,
re-apply, and attach the retained volume. `user_data.sh.tftpl` detects an
existing filesystem and **refuses to format it**, so the journal survives; then
re-run `smoke.sh` and compare counts against the evidence bundle from step 7.
Delete the retained volume afterwards or it bills $1.60/mo for ever.

## 9. External-effect scenarios — only when wanted

```bash
ssh ... ec2-user@<ip>
cd /opt/durexec/deploy/aws && set -a && . /opt/durexec/.env && set +a
docker compose -f docker-compose.cloud.yml --profile extsim up -d extsim
# then set EXTSIM_URL=http://extsim:8090 in /opt/durexec/.env and recreate the nodes
```

The simulator publishes **no port**. Read its ledger with
`docker exec extsim ...` or from inside the compose network. Do not publish it.

Arming a crash boundary needs `FAULT_INJECTION=true` **and** `FAULT_CRASH_AT=<point>`;
`config.Validate` refuses the second without the first. Arming is **per process,
not per workflow** — quiesce the cluster first (zero non-terminal executions,
zero outstanding tasks) or an unrelated workflow will trip the armed point, as
happened in Phase 8.

## 10. Teardown — do this the same day

```bash
DUREXEC_CONFIRM=i-understand-this-costs-money ./scripts/aws/teardown.sh
```

It destroys only what is in Terraform state, then prints a **read-only** sweep
of anything still tagged `Project=durexec`. Any non-empty line there is still
billing:

```bash
aws ec2 delete-volume   --volume-id  <vol-id>    # a retained data volume
aws ec2 delete-snapshot --snapshot-id <snap-id>  # snapshots persist for ever
```

The Elastic IP listing is intentionally **not** tag-filtered: the account has a
pre-existing associated EIP belonging to another project. If a row has no
`Project=durexec` tag, it is not ours — leave it alone.

## If something is wrong

| Symptom | Check |
|---|---|
| `wait_ready.sh` times out | `sudo tail -50 /var/log/durexec-bootstrap.log` on the instance |
| A node will not start | `docker logs node-a`. A config refusal is the engine working: `FAULT_CRASH_AT` set without `FAULT_INJECTION=true` is rejected on purpose. |
| `/mnt/durexec-data` not mounted | `findmnt`; fstab mounts by `LABEL=durexec-data` with `nofail` |
| Shards unowned after a kill | `LEASE_TTL` is 10s; wait 25s before judging |
| Grafana empty | Prometheus rejects samples after a clock change — `docker restart durexec-prometheus-1` |
| `docker build` fails on a gpg error | `build_images.sh` already uses a throwaway `DOCKER_CONFIG` |

## Prepared but NOT run: ECR

Only needed with more than one instance or a CI deployer. Pushing incurs
storage charges, so none of this was executed.

```bash
aws ecr create-repository --repository-name durexec-engine
aws ecr get-login-password | docker login --username AWS --password-stdin \
  <acct>.dkr.ecr.us-east-1.amazonaws.com
docker tag durexec-engine:<ver> <acct>.dkr.ecr.us-east-1.amazonaws.com/durexec-engine:<ver>
docker push <acct>.dkr.ecr.us-east-1.amazonaws.com/durexec-engine:<ver>
```

Do **not** push `durexec-extsimd`. It is test-only and has no business in a
registry a production deployment can reach.
