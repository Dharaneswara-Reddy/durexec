#!/usr/bin/env bash
# Fault injectors for the EKS pilot. Every injector is SCOPED: it refuses any
# target that is not an engine pod in the pilot namespace, a node of the
# durexec-bench cluster, or the Terraform-managed pilot database instance.
# The account is shared with unrelated projects; nothing here takes a name
# pattern or a free-form target.
#
#   eks_fault.sh delete    <pod> [grace_s]   kubectl delete pod (grace 0 = --force)
#   eks_fault.sh kill      <pod>             SIGKILL engined on its node (process crash, same pod)
#   eks_fault.sh freeze    <pod>             SIGSTOP engined (a stalled node: GC pause, VM stall)
#   eks_fault.sh thaw      <pod>             SIGCONT
#   eks_fault.sh netem     <pod> <spec...>   tc netem on the pod's node, ONLY for traffic to the DB
#   eks_fault.sh netem-clear <pod>
#   eks_fault.sh partition <pod>             NetworkPolicy: deny that pod's egress to the DB
#   eks_fault.sh heal      <pod>             remove the partition policy
#   eks_fault.sh verify-policy               prove NetworkPolicy is ENFORCED, using the probe pod
#   eks_fault.sh sever-db  <pod>             pg_terminate_backend every connection from that pod
#   eks_fault.sh drain     <pod>             kubectl drain the pod's node
#   eks_fault.sh uncordon  <pod-or-node>
#   eks_fault.sh terminate-node <pod>        EC2 terminate the pod's node (managed group replaces it)
#   eks_fault.sh reboot-db                   EC2 reboot of the pilot database host
#
# Every action prints `mark <epoch_ms> <action> <target>` on stdout so a
# caller can place it on the recovery timeline.
set -euo pipefail
cd "$(dirname "$0")/../.."
. scripts/aws/_common.sh
enable_evidence_tee
require_confirmation "inject a fault into the EKS pilot"
NS="${NS:-durexec-lab}"
. scripts/aws/_k8s.sh
CLUSTER="${CLUSTER:-durexec-bench}"
REGION="${AWS_REGION:-us-east-1}"
DEBUG_IMAGE="${DEBUG_IMAGE:-public.ecr.aws/docker/library/busybox:1.36}"
NET_IMAGE="${NET_IMAGE:-nicolaka/netshoot:v0.13}"

mark() { echo "mark $(date +%s%3N) $1 ${2:-}"; }
die()  { echo "FATAL: $*" >&2; exit 1; }

# An engine pod of THIS namespace, and nothing else.
engine_pod() {
  local p="$1"
  [[ "$p" =~ ^durexec-[0-9]+$ ]] || die "'$p' is not an engine pod name (durexec-N)"
  kubectl -n "$NS" get pod "$p" -l app=durexec -o name >/dev/null 2>&1 || die "no engine pod $p in $NS"
}
node_of() { kubectl -n "$NS" get pod "$1" -o jsonpath='{.spec.nodeName}'; }
# A node of the pilot cluster carrying the engine role.
engine_node() {
  local n="$1" role
  role=$(kubectl get node "$n" -o jsonpath='{.metadata.labels.durexec\.io/role}' 2>/dev/null || true)
  [ "$role" = engine ] || die "node $n is not a durexec engine node (role='$role')"
}
db_ip() { terraform -chdir=deploy/aws/eks output -raw database_private_ip; }

# Run one privileged command in the host PID and network namespaces of a node,
# then remove the debug pod. `kubectl debug node` pods run with hostPID and
# hostNetwork; --profile=sysadmin makes them privileged.
on_node() {
  local node="$1" image="$2"; shift 2

  kubectl -n "$NS" debug "node/$node" --profile=sysadmin --image="$image" \
    --attach=false -q -- sh -c "$*" >/dev/null
  # kubectl debug names the pod node-debugger-<node>-<rand>; find the newest.
  local pod
  pod=$(kubectl -n "$NS" get pods --sort-by=.metadata.creationTimestamp -o name \
          | grep "node-debugger-${node}" | tail -1)
  kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Succeeded "$pod" --timeout=90s >/dev/null 2>&1 \
    || { kubectl -n "$NS" logs "$pod" >&2 || true; kubectl -n "$NS" delete "$pod" --wait=false >/dev/null 2>&1; die "debug command failed on $node"; }
  kubectl -n "$NS" logs "$pod" 2>/dev/null || true
  kubectl -n "$NS" delete "$pod" --wait=false >/dev/null 2>&1 || true
}

signal_engined() { # pod signal
  engine_pod "$1"; local n; n=$(node_of "$1"); engine_node "$n"
  # Exactly one engined per node is guaranteed by required anti-affinity; refuse otherwise.
  on_node "$n" "$DEBUG_IMAGE" "c=\$(pgrep -x engined | wc -l); [ \"\$c\" = 1 ] || { echo \"found \$c engined\"; exit 3; }; kill -$2 \$(pgrep -x engined)"
}

policy_name() { echo "durexec-fault-deny-db-$1"; }

case "${1:-}" in
  delete)
    engine_pod "$2"; g="${3:-30}"
    mark delete "$2 grace=$g"
    if [ "$g" = 0 ]; then kubectl -n "$NS" delete pod "$2" --force --grace-period=0 >/dev/null 2>&1
    else kubectl -n "$NS" delete pod "$2" --grace-period="$g" --wait=false >/dev/null; fi ;;
  kill)   mark kill "$2";   signal_engined "$2" KILL ;;
  freeze) mark freeze "$2"; signal_engined "$2" STOP ;;
  thaw)   signal_engined "$2" CONT; mark thaw "$2" ;;
  netem)
    engine_pod "$2"; n=$(node_of "$2"); engine_node "$n"; ip=$(db_ip); shift 2
    spec="$*"; [[ "$spec" =~ ^[a-z0-9.%\ ]+$ ]] || die "netem spec '$spec' has unexpected characters"
    # prio qdisc; band 3 gets the netem; only packets to the DB IP go to band 3.
    # The engine pod is the only workload on an engine node that talks to the DB.
    mark netem "$2 $spec dst=$ip"
    on_node "$n" "$NET_IMAGE" "dev=\$(ip route get $ip | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p'); \
      tc qdisc add dev \$dev root handle 1: prio && \
      tc qdisc add dev \$dev parent 1:3 handle 30: netem $spec && \
      tc filter add dev \$dev protocol ip parent 1:0 prio 3 u32 match ip dst $ip/32 flowid 1:3 && \
      echo applied on \$dev && tc qdisc show dev \$dev" ;;
  netem-clear)
    engine_pod "$2"; n=$(node_of "$2"); engine_node "$n"; ip=$(db_ip)
    on_node "$n" "$NET_IMAGE" "dev=\$(ip route get $ip | sed -n 's/.* dev \\([^ ]*\\).*/\\1/p'); tc qdisc del dev \$dev root 2>/dev/null; tc qdisc show dev \$dev"
    mark netem-clear "$2" ;;
  partition|verify-policy)
    ip=$(db_ip)
    if [ "$1" = partition ]; then engine_pod "$2"; sel="statefulset.kubernetes.io/pod-name: $2"; name=$(policy_name "$2")
    else sel="app: durexec-probe"; name="durexec-fault-verify"; fi
    kubectl apply -f - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: $name, namespace: $NS, labels: { durexec.io/fault: "true" } }
spec:
  podSelector: { matchLabels: { $sel } }
  policyTypes: [Egress]
  egress:
    - to: [ { ipBlock: { cidr: 0.0.0.0/0, except: [ "$ip/32" ] } } ]
EOF
    if [ "$1" = partition ]; then mark partition "$2 dst=$ip"; exit 0; fi
    # Enforcement PROOF: the probe must fail to reach the DB while the policy
    # is in place, and succeed again once it is removed. A policy the CNI
    # ignores would leave the first query succeeding.
    sleep 5; probe_ensure
    if echo "select 1;" | probe_sql >/dev/null 2>&1; then blocked=no; else blocked=yes; fi
    kubectl -n "$NS" delete networkpolicy "$name" >/dev/null; sleep 5
    if echo "select 1;" | probe_sql >/dev/null 2>&1; then restored=yes; else restored=no; fi
    echo "networkpolicy_enforced=$blocked restored_after_delete=$restored"
    [ "$blocked" = yes ] && [ "$restored" = yes ] ;;
  heal)
    engine_pod "$2"; kubectl -n "$NS" delete networkpolicy "$(policy_name "$2")" --ignore-not-found >/dev/null
    mark heal "$2" ;;
  sever-db)
    engine_pod "$2"; pip=$(kubectl -n "$NS" get pod "$2" -o jsonpath='{.status.podIP}')
    [[ "$pip" =~ ^[0-9.]+$ ]] || die "no pod IP for $2"
    mark sever-db "$2 ip=$pip"
    echo "select count(pg_terminate_backend(pid)) from pg_stat_activity where client_addr = '$pip'::inet;" | probe_sql ;;
  drain)
    engine_pod "$2"; n=$(node_of "$2"); engine_node "$n"
    mark drain "$n"
    kubectl drain "$n" --ignore-daemonsets --delete-emptydir-data --timeout=180s >/dev/null 2>&1 || true ;;
  uncordon)
    t="$2"; [[ "$t" =~ ^durexec-[0-9]+$ ]] && t=$(node_of "$t"); engine_node "$t"
    kubectl uncordon "$t" >/dev/null; mark uncordon "$t" ;;
  terminate-node)
    engine_pod "$2"; n=$(node_of "$2"); engine_node "$n"
    iid=$(kubectl get node "$n" -o jsonpath='{.spec.providerID}' | sed 's#.*/##')
    [[ "$iid" =~ ^i-[0-9a-f]+$ ]] || die "cannot resolve instance id for $n"
    owner=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$iid" \
      --query "Reservations[0].Instances[0].Tags[?Key=='eks:cluster-name'].Value|[0]" --output text)
    [ "$owner" = "$CLUSTER" ] || die "instance $iid belongs to cluster '$owner', not $CLUSTER"
    [ "${CONFIRM_TERMINATE:-}" = "$iid" ] || die "set CONFIRM_TERMINATE=$iid to terminate node $n"
    mark terminate-node "$n $iid"
    aws ec2 terminate-instances --region "$REGION" --instance-ids "$iid" >/dev/null ;;
  reboot-db)
    iid=$(terraform -chdir=deploy/aws/eks output -raw database_instance_id)
    proj=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$iid" \
      --query "Reservations[0].Instances[0].Tags[?Key=='Project'].Value|[0]" --output text)
    [ "$proj" = durexec ] || die "instance $iid is not tagged Project=durexec"
    mark reboot-db "$iid"
    aws ec2 reboot-instances --region "$REGION" --instance-ids "$iid" ;;
  *) sed -n '2,24p' "$0"; exit 2 ;;
esac
