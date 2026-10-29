#!/usr/bin/env bash
# Block until the instance has finished its bootstrap. Read-only: it creates
# nothing, so it needs no confirmation.
#
# It waits for the marker file user_data.sh.tftpl touches at the very end,
# rather than for SSH to answer. SSH answers long before docker and the data
# volume are ready, and a deploy that starts in that window fails in a way
# that looks like a bug in the engine.
source "$(dirname "$0")/_common.sh"

IP=$(instance_ip)
echo "waiting for ec2-user@$IP to finish bootstrapping (up to 5 minutes)"

for i in $(seq 1 60); do
  if ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 \
       -i "$(ssh_key)" "ec2-user@$IP" \
       'test -f /opt/durexec/.bootstrap-complete' 2>/dev/null; then
    echo "bootstrap complete after about $((i*5))s"
    echo
    echo "=== instance state ==="
    remote 'docker --version; docker compose version; findmnt /mnt/durexec-data; systemctl is-active node_exporter'
    exit 0
  fi
  printf '.'
  sleep 5
done

echo
echo "FATAL: the bootstrap marker never appeared. Read the log:" >&2
echo "  ssh -i $(ssh_key) ec2-user@$IP 'sudo tail -50 /var/log/durexec-bootstrap.log'" >&2
exit 1
