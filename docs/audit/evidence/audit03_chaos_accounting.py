#!/usr/bin/env python3
"""AUDIT 03: submit workflows under node failure and ACCOUNT FOR EVERY ONE.

Unlike cmd/loadgen -mode duplicates, a request that the client gave up on is
not dropped from the report: after the run, every submitted id is looked up in
the database, so a request the client saw as FAILED but that left an
execution row behind (an orphan) is counted, and so is every accepted
workflow that never reached a terminal state.

Failure injection (local cluster only): docker stop (SIGTERM, graceful) and
docker kill (SIGKILL) of engine nodes at fixed offsets, then docker start.
The kill instants are wall-clock offsets, NOT transaction boundaries.

Rows are appended to the CSV and fsynced as each workflow resolves, so a run
killed half way leaves usable data. Summary is recomputed from the CSV.

usage: python3 -I audit03_chaos_accounting.py RUN_TAG N DURATION_S "t:action:node,..."
  e.g. python3 -I audit03_chaos_accounting.py r1 300 60 "10:stop:node-c,25:start:node-c,35:kill:node-b,50:start:node-b"
resume: re-running with the same RUN_TAG skips ids already in the CSV.
"""
import csv, json, os, queue, subprocess, sys, threading, time, urllib.request, urllib.error

PORTS = {"node-a": 8081, "node-b": 8082, "node-c": 8083}
HERE = os.path.dirname(os.path.abspath(__file__))
PSQL = ["docker", "exec", "deploy-postgres-leader-1", "psql", "-U", "durexec", "-d", "durexec", "-At", "-F", "|", "-c"]

def psql(sql):
    return subprocess.run(PSQL + [sql], capture_output=True, text=True, timeout=60).stdout.strip()

def http(method, url, body=None, timeout=8):
    req = urllib.request.Request(url, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={"content-type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
    except Exception as e:  # connection refused, timeout, reset
        return -1, type(e).__name__

def main():
    tag, n, dur, plan = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), sys.argv[4]
    prefix = f"a03-{tag}-"
    csv_path = os.path.join(HERE, f"chaos-{tag}.csv")
    log_path = os.path.join(HERE, f"chaos-{tag}.events.log")
    done = set()
    if os.path.exists(csv_path):
        with open(csv_path) as f:
            done = {r["id"] for r in csv.DictReader(f)}
    new = not os.path.exists(csv_path)
    fcsv = open(csv_path, "a", newline="")
    w = csv.writer(fcsv)
    if new:
        w.writerow(["id", "attempts", "attempt_codes", "accepted", "first_ok_after_s"]); fcsv.flush()
    lock = threading.Lock()
    flog = open(log_path, "a")
    def log(msg):
        line = f"{time.strftime('%H:%M:%S')} {msg}"
        with lock:
            flog.write(line + "\n"); flog.flush(); os.fsync(flog.fileno())
        print(line, flush=True)
    log(f"RUN {tag} n={n} duration={dur}s plan={plan}  (resume: same command; ids already in {os.path.basename(csv_path)} are skipped)")
    t0 = time.time()

    def chaos():
        for item in plan.split(","):
            t, act, node = item.split(":")
            delay = float(t) - (time.time() - t0)
            if delay > 0: time.sleep(delay)
            log(f"CHAOS t={time.time()-t0:.1f}s docker {act} {node}")
            subprocess.run(["docker", act, node], capture_output=True, text=True, timeout=60)
    ct = threading.Thread(target=chaos); ct.start()

    q = queue.Queue()
    ids = [f"{prefix}{i:04d}" for i in range(n) if f"{prefix}{i:04d}" not in done]
    for i, wid in enumerate(ids): q.put((i, wid))
    interval = dur / max(n, 1)
    nodes = list(PORTS)
    def worker(k):
        while True:
            try: i, wid = q.get_nowait()
            except queue.Empty: return
            wait = t0 + i * interval - time.time()
            if wait > 0: time.sleep(wait)
            body = {"workflow_id": wid, "workflow_type": "DeliveryOrder", "input": {
                "order_id": wid, "customer_id": "c-audit", "restaurant_id": "r-audit",
                "items": [{"sku": "SKU-1", "quantity": 1}], "amount_cents": 1500, "currency": "USD",
                "prep_time_ns": 50000000}}
            codes, ok_at, start = [], None, time.time()
            for a in range(10):
                node = nodes[(i + a) % 3]
                code, _ = http("POST", f"http://localhost:{PORTS[node]}/v1/workflows", body)
                codes.append(str(code))
                if code in (200, 201):
                    ok_at = time.time() - start; break
                time.sleep(1.0)
            with lock:
                w.writerow([wid, len(codes), "/".join(codes), int(ok_at is not None), "" if ok_at is None else f"{ok_at:.2f}"])
                fcsv.flush(); os.fsync(fcsv.fileno())
    ths = [threading.Thread(target=worker, args=(k,)) for k in range(8)]
    [t.start() for t in ths]; [t.join() for t in ths]; ct.join()
    log("submission and chaos finished; waiting for cluster to settle")
    for _ in range(60):
        owned = psql("select count(*) from shard_leases where owner_node is not null and expires_at > now()")
        if owned == "16": break
        time.sleep(2)
    log(f"live lease rows: {owned}/16")
    time.sleep(2)
    summarise(tag, prefix, csv_path, log)

def summarise(tag, prefix, csv_path, log):
    with open(csv_path) as f: rows = list(csv.DictReader(f))
    acc = [r for r in rows if r["accepted"] == "1"]
    rej = [r for r in rows if r["accepted"] != "1"]
    deadline = time.time() + 240
    while time.time() < deadline:
        running = psql(f"select count(*) from executions where workflow_id like '{prefix}%' and status='RUNNING' and last_seq>0")
        if running == "0": break
        time.sleep(5)
    st = psql(f"select status, count(*) from executions where workflow_id like '{prefix}%' group by 1 order by 1").replace("\n", "; ")
    orphans = psql(f"select count(*) from executions where workflow_id like '{prefix}%' and status='RUNNING' and last_seq=0")
    stuck = psql(f"select count(*) from executions where workflow_id like '{prefix}%' and status='RUNNING' and last_seq>0")
    present = {l.split('|')[0] for l in psql(f"select workflow_id from executions where workflow_id like '{prefix}%'").splitlines()}
    acc_missing = [r["id"] for r in acc if r["id"] not in present]
    rej_present = [r["id"] for r in rej if r["id"] in present]
    dup_k = psql(f"""select count(*) from (select shard_id,workflow_id,run_id,(payload->>'k') k,count(*) from events
        where workflow_id like '{prefix}%' and event_type='ActivityTaskCompleted' group by 1,2,3,4 having count(*)>1) x""")
    k_total = psql(f"select count(*) from events where workflow_id like '{prefix}%' and event_type='ActivityTaskCompleted'")
    # every completed workflow must have exactly 4 completed activities (ChargePayment..DispatchDriver) - a short or long history is flagged
    odd = psql(f"""select count(*) from (select e.workflow_id,count(*) c from events e join executions x using(shard_id,workflow_id,run_id)
        where e.workflow_id like '{prefix}%' and x.status='COMPLETED' and e.event_type='ActivityTaskCompleted' group by 1 having count(*)<>4) y""")
    out = [
        f"SUMMARY run={tag} (recomputed from {os.path.basename(csv_path)} and the live DB)",
        f"  submitted ids in CSV              {len(rows)}",
        f"  accepted by client (HTTP 200/201) {len(acc)}   ({sum(1 for r in acc if int(r['attempts'])>1)} needed a retry)",
        f"  never accepted (10 attempts)      {len(rej)}",
        f"  execution status in DB            {st}",
        f"  ORPHANS (RUNNING, last_seq=0)     {orphans}",
        f"  stuck RUNNING with history        {stuck}",
        f"  accepted but no DB row            {len(acc_missing)} {acc_missing[:5]}",
        f"  never-accepted but DB row exists  {len(rej_present)} {rej_present[:5]}",
        f"  ActivityTaskCompleted total       {k_total}",
        f"  duplicate (shard,wf,run,k)        {dup_k}",
        f"  COMPLETED runs with !=4 activities {odd}",
    ]
    for l in out: log(l)

if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--summary":
        tag = sys.argv[2]; p = os.path.join(HERE, f"chaos-{tag}.csv")
        summarise(tag, f"a03-{tag}-", p, print)
    else:
        main()
