#!/usr/bin/env python3
"""AUDIT 03: does a client that gives up mid-request leave an orphan workflow?

Sends POST /v1/workflows with a very short client-side timeout (random 2-30 ms),
so some connections are closed while the server is between Executions.Create and
CommitDecision. Then counts executions with this prefix that are RUNNING with
last_seq=0 (no event, no task = will never run). Local cluster only; ids are
unique and prefixed, nothing else is touched. Rows are written to the CSV as
each request resolves.

usage: python3 -I audit03_client_abort_orphans.py TAG N
"""
import csv, json, os, random, subprocess, sys, threading, time, socket, http.client
HERE = os.path.dirname(os.path.abspath(__file__))
def psql(sql):
    return subprocess.run(["docker","exec","deploy-postgres-leader-1","psql","-U","durexec","-d","durexec","-At","-c",sql],capture_output=True,text=True,timeout=60).stdout.strip()
tag, n = sys.argv[1], int(sys.argv[2]); prefix = f"a03-{tag}-"
path = os.path.join(HERE, f"abort-{tag}.csv"); f = open(path, "a", newline=""); w = csv.writer(f)
w.writerow(["id","timeout_ms","outcome"]); f.flush()
lock = threading.Lock(); ctr = iter(range(n))
def worker():
    while True:
        with lock:
            try: i = next(ctr)
            except StopIteration: return
        wid = f"{prefix}{i:05d}"; to = random.uniform(0.002, 0.030)
        body = json.dumps({"workflow_id": wid, "workflow_type": "DeliveryOrder", "input": {"order_id": wid, "customer_id": "c", "restaurant_id": "r", "items": [{"sku": "S", "quantity": 1}], "amount_cents": 100, "currency": "USD", "prep_time_ns": 50000000}})
        port = (8081, 8082, 8083)[i % 3]
        try:
            c = http.client.HTTPConnection("localhost", port, timeout=to)
            c.request("POST", "/v1/workflows", body, {"content-type": "application/json"})
            r = c.getresponse(); out = str(r.status); r.read(); c.close()
        except (socket.timeout, TimeoutError):
            out = "client-timeout"
            try: c.close()
            except Exception: pass
        except Exception as e:
            out = type(e).__name__
        with lock:
            w.writerow([wid, f"{to*1000:.1f}", out]); f.flush(); os.fsync(f.fileno())
ths = [threading.Thread(target=worker) for _ in range(16)]; [t.start() for t in ths]; [t.join() for t in ths]
time.sleep(25)  # let anything that did start finish
print("rows by outcome:", psql(f"select count(*) from executions where workflow_id like '{prefix}%'"), "executions total")
print("status:", psql(f"select status||'/last_seq='||(case when last_seq=0 then '0' else '>0' end), count(*) from executions where workflow_id like '{prefix}%' group by 1").replace("\n","; "))
print("ORPHANS (RUNNING, last_seq=0):", psql(f"select count(*) from executions where workflow_id like '{prefix}%' and status='RUNNING' and last_seq=0"))
print("client outcomes:", end=" ")
import collections
print(dict(collections.Counter(r[2] for r in csv.reader(open(path)) if r[0] != "id")))
