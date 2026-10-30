#!/usr/bin/env python3
"""Regression test for k8s_usage.py's time window.

    python3 scripts/aws/k8s_usage_test.py

The reducer once read kubelet's UTC timestamps as LOCAL time, so on a machine
outside UTC every sample fell outside the window and CPU was reported
"unavailable". The same input must give the same answer in every zone.
"""
import json
import os
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
TIMES = ["2026-10-30T08:00:00Z", "2026-10-30T08:00:10Z", "2026-10-30T08:00:20Z"]


def epoch(s):
    return int(datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp())


class WindowIsTimezoneIndependent(unittest.TestCase):
    def setUp(self):
        fd, self.path = tempfile.mkstemp(suffix=".jsonl")
        with os.fdopen(fd, "w") as f:
            for i, t in enumerate(TIMES):
                pod = {"podRef": {"namespace": "durexec-lab", "name": "durexec-0"},
                       "cpu": {"time": t, "usageCoreNanoSeconds": i * 5 * 10**9},
                       "memory": {"workingSetBytes": 100}}
                f.write(json.dumps({"node": "eng-a", "summary": {"pods": [pod]}}) + "\n")

    def tearDown(self):
        os.unlink(self.path)

    def test_same_cpu_in_every_zone(self):
        lo, hi = epoch("2026-10-30T07:59:55Z"), epoch("2026-10-30T08:00:25Z")
        for tz in ("UTC", "Asia/Kolkata", "America/New_York"):
            out = subprocess.run(
                [sys.executable, os.path.join(HERE, "k8s_usage.py"), self.path, str(lo), str(hi)],
                env={**os.environ, "TZ": tz}, capture_output=True, text=True, check=True).stdout
            self.assertIn("pod|durexec-lab/durexec-0|eng-a|cpu_cores=0.500", out, f"TZ={tz}:\n{out}")


if __name__ == "__main__":
    unittest.main()
