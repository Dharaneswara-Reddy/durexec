#!/usr/bin/env python3
"""Print node name, role, EC2 instance type, and zone from Kubernetes JSON."""

import json
import sys


def inventory(document):
    nodes = document.get("items", [])
    if not nodes:
        raise ValueError("Kubernetes node list is empty")
    rows = []
    for node in nodes:
        metadata = node.get("metadata", {})
        labels = metadata.get("labels", {})
        name = metadata.get("name", "")
        role = labels.get("durexec.io/role", "unassigned")
        instance_type = labels.get("node.kubernetes.io/instance-type", "")
        zone = labels.get("topology.kubernetes.io/zone", "unknown")
        if not name or not instance_type:
            raise ValueError(f"node {name or '<unnamed>'} has no EC2 instance-type label")
        rows.append((name, role, instance_type, zone))
    return sorted(rows)


def main():
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        sample = {"items": [
            {"metadata": {"name": "engine-a", "labels": {
                "durexec.io/role": "engine",
                "node.kubernetes.io/instance-type": "m7i-flex.large",
                "topology.kubernetes.io/zone": "us-east-1a"}}},
            {"metadata": {"name": "harness-a", "labels": {
                "durexec.io/role": "harness",
                "node.kubernetes.io/instance-type": "c7i-flex.large",
                "topology.kubernetes.io/zone": "us-east-1a"}}},
        ]}
        assert inventory(sample) == [
            ("engine-a", "engine", "m7i-flex.large", "us-east-1a"),
            ("harness-a", "harness", "c7i-flex.large", "us-east-1a"),
        ]
        try:
            inventory({"items": [{"metadata": {"name": "missing-type", "labels": {}}}]})
        except ValueError:
            pass
        else:
            raise AssertionError("missing instance type was accepted")
        print("node inventory self-test: PASS")
        return

    try:
        rows = inventory(json.load(sys.stdin))
    except (json.JSONDecodeError, OSError, ValueError) as exc:
        print(f"node inventory: {exc}", file=sys.stderr)
        raise SystemExit(1)
    print("NAME\tROLE\tINSTANCE_TYPE\tZONE")
    for row in rows:
        print("\t".join(row))


if __name__ == "__main__":
    main()
