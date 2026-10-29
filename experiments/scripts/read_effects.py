#!/usr/bin/env python3
"""Print one scenario's effect ledger. Separate file so no shell quoting applies."""
import json
import sys

with open(sys.argv[1]) as f:
    d = json.load(f)
print("    distinct_keys       :", d["distinct_keys"])
print("    duplicate_effects   :", d["duplicate_effect_count"])
for e in d["effects"]:
    # Requests > 1 under one key is the dedup WORKING. Two rows sharing a key
    # would be the duplicate effect this phase hunts for, and that is what
    # duplicate_effects above counts.
    print(f"    effect key={e['Key'][:26]} amount={e['Amount']} payee={e['Payee']} requests={e['Requests']}")
