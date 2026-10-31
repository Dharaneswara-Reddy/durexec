#!/usr/bin/env python3
"""Generate the result figures of Chapter 7 from the AWS evidence.

    python3 figures/fig_results.py            (run from Report_Template_AIDS_LaTeX/)

Every number plotted is read from docs/results/cloud-campaign/ (results.csv,
faults.csv and the idle-baseline summaries); nothing is typed in by hand.
Figures are written as 300-dpi PNGs on a white background.
"""
import csv
import glob
import os
import statistics as st
from collections import defaultdict

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
EVID = os.path.normpath(os.path.join(HERE, "..", "..", "docs", "results", "cloud-campaign"))
OUT = HERE

plt.rcParams.update({
    "font.family": "serif",
    "font.serif": ["Liberation Serif", "Times New Roman", "DejaVu Serif"],
    "font.size": 9.5,
    "axes.titlesize": 9.5,
    "axes.labelsize": 9.5,
    "legend.fontsize": 8.5,
    "xtick.labelsize": 8.5,
    "ytick.labelsize": 8.5,
    "axes.linewidth": 0.7,
    "axes.grid": True,
    "grid.color": "#d9d9d9",
    "grid.linewidth": 0.5,
    "axes.axisbelow": True,
    "figure.facecolor": "white",
    "axes.facecolor": "white",
    "savefig.facecolor": "white",
    "savefig.dpi": 300,
    "savefig.bbox": "tight",
    "savefig.pad_inches": 0.04,
    "lines.linewidth": 1.4,
    "lines.markersize": 4.5,
})
INK = "#1f2933"
C1, C2, C3, C4 = "#1f4e79", "#c0504d", "#4f7f3a", "#8064a2"
GREY = "#7f7f7f"
W = 5.8  # inches, the text width of the report


def rows(name):
    with open(os.path.join(EVID, name)) as fh:
        return list(csv.DictReader(l for l in fh if not l.startswith("#")))


def f(r, k):
    try:
        return float(r[k])
    except (KeyError, TypeError, ValueError):
        return None


R = rows("results.csv")
FAULTS = [r for r in rows("faults.csv") if r["verdict"] == "ok"]


def by_conc(exps):
    """{concurrency: [rows]} for valid closed-loop rows of the given experiments."""
    d = defaultdict(list)
    for r in R:
        if r["exp"] in exps and r["mode"] == "scaling" and r["verdict"] in ("RECORDED", "SMOKE_ok"):
            d[int(r["concurrency"])].append(r)
    return dict(sorted(d.items()))


def mean_sd(vals):
    vals = [v for v in vals if v is not None]
    if not vals:
        return None, None
    return st.mean(vals), (st.stdev(vals) if len(vals) > 1 else 0.0)


def save(fig, name):
    fig.savefig(os.path.join(OUT, name))
    plt.close(fig)
    print("wrote", name)


# --- 1. closed-loop throughput and latency against concurrency (Stage D) ------
def fig_closed_loop():
    d = by_conc({"D-ladder", "D-knee"})
    cs = list(d)
    thr = [mean_sd([f(r, "wf_per_s") for r in d[c]]) for c in cs]
    p50 = [mean_sd([f(r, "p50_s") for r in d[c]])[0] for c in cs]
    p99 = [mean_sd([f(r, "p99_s") for r in d[c]])[0] for c in cs]
    fig, (a, b) = plt.subplots(1, 2, figsize=(W, 2.35))
    a.errorbar(cs, [m for m, _ in thr], yerr=[s for _, s in thr], color=C1, marker="o", capsize=2.5)
    a.set_xscale("log", base=2)
    a.set_xticks(cs, [str(c) for c in cs])
    a.set_xlabel("Concurrent workflows (closed loop)")
    a.set_ylabel("Completed workflows / s")
    a.set_title("(a) Throughput")
    b.plot(cs, p50, color=C1, marker="o", label="p50")
    b.plot(cs, p99, color=C2, marker="s", label="p99")
    b.set_xscale("log", base=2)
    b.set_xticks(cs, [str(c) for c in cs])
    b.set_xlabel("Concurrent workflows (closed loop)")
    b.set_ylabel("End-to-end latency (s)")
    b.set_title("(b) Latency")
    b.legend(frameon=False)
    fig.tight_layout(w_pad=1.5)
    save(fig, "fig_7_1_closed_loop.png")


# --- 2. where the resources go as load rises (Stage D) -------------------------
def fig_resources():
    d = by_conc({"D-ladder", "D-knee"})
    cs = list(d)
    db = [mean_sd([f(r, "dbhost_cpu_pct") for r in d[c]])[0] for c in cs]
    node = [mean_sd([f(r, "eng_node_cpu_pct_max") for r in d[c]])[0] for c in cs]
    gen = [100 * mean_sd([f(r, "gen_cpu_cores") for r in d[c]])[0] / 2.0 for c in cs]
    pool = [100 * mean_sd([f(r, "eng_pool_empty_frac") for r in d[c]])[0] for c in cs]
    fig, ax = plt.subplots(figsize=(W, 2.5))
    ax.plot(cs, db, color=C2, marker="s", label="Database host CPU")
    ax.plot(cs, node, color=C1, marker="o", label="Busiest engine node CPU")
    ax.plot(cs, gen, color=C3, marker="^", label="Load generator CPU")
    ax.plot(cs, pool, color=C4, marker="D", linestyle="--", label="Engine pool empty on acquire")
    ax.set_xscale("log", base=2)
    ax.set_xticks(cs, [str(c) for c in cs])
    ax.set_ylim(0, 100)
    ax.set_xlabel("Concurrent workflows (closed loop)")
    ax.set_ylabel("Percent")
    ax.legend(frameon=False, loc="upper left", ncol=2)
    fig.tight_layout()
    save(fig, "fig_7_2_resources.png")


# --- 3. database wait-event profile (Stage D) -----------------------------------
def fig_waits():
    d = by_conc({"D-ladder", "D-knee"})
    cats = ["WALWrite", "WALSync", "transactionid", "tuple", "ClientRead", "other"]
    labels = ["WAL write (LWLock)", "WAL sync (I/O)", "Row lock: transaction", "Row lock: tuple",
              "Waiting for client", "Other"]
    colours = [C1, "#5b9bd5", C2, "#e6a19e", C3, "#bfbfbf"]
    cs = [c for c in d if c >= 16]
    share = {k: [] for k in cats}
    for c in cs:
        acc = defaultdict(list)
        for r in d[c]:
            prof = {}
            for part in (r.get("pg_wait_profile") or "").split(";"):
                if ":" not in part:
                    continue
                name, pct = part.rsplit(":", 1)
                prof[name.split("|")[-1]] = float(pct.rstrip("%"))
            got = 0.0
            for k in cats[:-1]:
                v = prof.get(k, 0.0)
                acc[k].append(v)
                got += v
            acc["other"].append(max(0.0, 100 - got))
        for k in cats:
            share[k].append(st.mean(acc[k]))
    fig, ax = plt.subplots(figsize=(W, 2.4))
    x = range(len(cs))
    bottom = [0.0] * len(cs)
    for k, lab, col in zip(cats, labels, colours):
        ax.bar(x, share[k], bottom=bottom, color=col, width=0.62, label=lab, edgecolor="white", linewidth=0.4)
        bottom = [b + v for b, v in zip(bottom, share[k])]
    ax.set_xticks(list(x), [str(c) for c in cs])
    ax.set_ylim(0, 100)
    ax.grid(axis="x", visible=False)
    ax.set_xlabel("Concurrent workflows (closed loop)")
    ax.set_ylabel("Share of sampled waits (%)")
    ax.legend(frameon=False, bbox_to_anchor=(1.01, 1), loc="upper left")
    fig.tight_layout()
    save(fig, "fig_7_3_db_waits.png")


# --- 4. open loop: offered against completed, and the tail (Stage E) ------------
def fig_open_loop():
    d = defaultdict(list)
    for r in R:
        if r["exp"] in ("E-arrival", "E-bracket", "C-low-arrival") and r["mode"] == "arrival":
            d[float(r["offered_rate"])].append(r)
    rates = sorted(d)
    fig, (a, b) = plt.subplots(1, 2, figsize=(W, 2.35))
    for rate in rates:
        for r in d[rate]:
            # Sustained = nothing dropped and the tail stayed bounded. The
            # runner's own verdict is not used here: it also flags low-rate
            # calibration points whose latency was merely near the floor.
            ok = (f(r, "dropped") or 0) == 0 and (f(r, "p99_s") or 0) < 10
            a.scatter(rate, f(r, "wf_per_s"), s=16, color=C1 if ok else C2, marker="o" if ok else "x", zorder=3)
            b.scatter(rate, f(r, "p99_s"), s=16, color=C1 if ok else C2, marker="o" if ok else "x", zorder=3)
    lim = max(rates) * 1.08
    a.plot([0, lim], [0, lim], color=GREY, linestyle=":", linewidth=1, label="completed = offered")
    a.set_xlim(0, lim)
    a.set_ylim(0, lim)
    a.set_xlabel("Offered arrival rate (workflows / s)")
    a.set_ylabel("Completed workflows / s")
    a.set_title("(a) Goodput")
    a.scatter([], [], color=C1, s=16, label="sustained")
    a.scatter([], [], color=C2, marker="x", s=16, label="overloaded")
    a.legend(frameon=False, loc="upper left")
    b.set_yscale("log")
    b.set_yticks([2, 5, 10, 20, 50], ["2", "5", "10", "20", "50"])
    b.minorticks_off()
    b.set_xlim(0, lim)
    b.set_xlabel("Offered arrival rate (workflows / s)")
    b.set_ylabel("p99 latency (s, log scale)")
    b.set_title("(b) Tail latency")
    fig.tight_layout(w_pad=1.5)
    save(fig, "fig_7_4_open_loop.png")


# --- 5. idle cost of polling (Stage B) ------------------------------------------
def fig_idle():
    g = defaultdict(list)
    for p in glob.glob(os.path.join(EVID, "idle-baselines", "*", "50-idle-summary.txt")):
        cfg = os.path.basename(os.path.dirname(p)).split("-", 1)[1]
        kv = dict(l.strip().split("=", 1) for l in open(p) if "=" in l)
        g[cfg].append(float(kv["pg_commits_per_s"]))
    order = ["r3-S16-w2x4-scon", "r3-S16-w4x8-scon", "r3-S16-w8x16-scon", "r3-S32-w2x4-scon", "r3-S64-w1x2-scon"]
    labels = ["16 shards\n2+4 workers", "16 shards\n4+8 workers", "16 shards\n8+16 workers",
              "32 shards\n2+4 workers", "64 shards\n1+2 workers"]
    order = [o for o in order if o in g]
    m = [st.mean(g[o]) for o in order]
    s = [st.stdev(g[o]) if len(g[o]) > 1 else 0 for o in order]
    fig, ax = plt.subplots(figsize=(W, 2.3))
    cols = [C1, C1, C1, C3, C3]
    ax.bar(range(len(order)), m, yerr=s, color=cols[:len(order)], width=0.6, capsize=2.5)
    for i, (v, e) in enumerate(zip(m, s)):
        ax.text(i, v + e + 8, f"{v:.0f}", ha="center", va="bottom", fontsize=8.5, color=INK)
    ax.set_xticks(range(len(order)), labels[:len(order)])
    ax.grid(axis="x", visible=False)
    ax.set_ylabel("Commits / s with no workload")
    ax.set_ylim(0, max(m) * 1.2)
    fig.tight_layout()
    save(fig, "fig_7_5_idle_cost.png")


# --- 6. sensitivity: workers, shards and replicas at the knee (Stages F, G, H) ---
def knee_mean(prefix_exps, conc):
    vals = [f(r, "wf_per_s") for r in R if r["exp"] in prefix_exps and r["concurrency"] == str(conc)
            and r["mode"] == "scaling" and r["verdict"] == "RECORDED"]
    return mean_sd(vals)


def fig_sensitivity():
    groups = [
        ("Workers per shard\n(16 shards, 3 replicas)", [
            ("2+4", {"F-w2x4-ladder", "F-w2x4-knee"}),
            ("4+8", {"D-ladder", "D-knee"}),
            ("8+16", {"F-w8x16-ladder", "F-w8x16-knee"})]),
        ("Shards at 192 workers\n(3 replicas)", [
            ("16", {"D-ladder", "D-knee"}),
            ("32", {"G-S32-ladder", "G-S32-knee"}),
            ("64", {"G-S64-ladder", "G-S64-knee"})]),
        ("Engine replicas\n(16 shards, 4+8 workers)", [
            ("1", {"H-r1-ladder", "H-r1-knee"}),
            ("2", {"H-r2-ladder", "H-r2-knee"}),
            ("3", {"D-ladder", "D-knee"})]),
    ]
    fig, axes = plt.subplots(1, 3, figsize=(W, 2.45), sharey=True)
    for ax, (title, items) in zip(axes, groups):
        ms, ss = zip(*[knee_mean(e, 256) for _, e in items])
        ax.bar(range(3), ms, yerr=ss, color=C1, width=0.6, capsize=2.5)
        for i, (v, e) in enumerate(zip(ms, ss)):
            ax.text(i, v + e + 3, f"{v:.0f}", ha="center", va="bottom", fontsize=8.5, color=INK)
        ax.set_xticks(range(3), [n for n, _ in items])
        ax.set_title(title)
        ax.grid(axis="x", visible=False)
        ax.set_ylim(0, 160)
    axes[0].set_ylabel("Workflows / s at 256 concurrent")
    fig.tight_layout(w_pad=0.8)
    save(fig, "fig_7_6_sensitivity.png")


# --- 7. fault recovery (fault campaign) --------------------------------------------
def fig_faults():
    order = ["delete-graceful", "kill", "drain", "freeze", "reboot-db"]
    names = ["Graceful\ndelete", "SIGKILL", "Node\ndrain", "Freeze\n35 s", "Database\nreboot"]
    claim, ring = defaultdict(list), defaultdict(list)
    for r in FAULTS:
        if r["scenario"] in order:
            if f(r, "t_claim_ms") is not None:
                claim[r["scenario"]].append(f(r, "t_claim_ms") / 1000)
            if f(r, "t_ring_ms") is not None:
                ring[r["scenario"]].append(f(r, "t_ring_ms") / 1000)
    fig, ax = plt.subplots(figsize=(W, 2.45))
    x = list(range(len(order)))
    for i, sc in enumerate(order):
        ax.scatter([i - 0.13] * len(claim[sc]), claim[sc], color=C1, s=18, zorder=3)
        ax.scatter([i + 0.13] * len(ring[sc]), ring[sc], color=C2, marker="s", s=18, zorder=3)
        if claim[sc]:
            ax.hlines(st.mean(claim[sc]), i - 0.25, i - 0.01, color=C1, linewidth=1.2)
        if ring[sc]:
            ax.hlines(st.mean(ring[sc]), i + 0.01, i + 0.25, color=C2, linewidth=1.2)
    ax.scatter([], [], color=C1, s=18, label="all shards re-owned (t_claim)")
    ax.scatter([], [], color=C2, marker="s", s=18, label="cluster fully healthy (t_ring)")
    ax.set_xticks(x, names)
    ax.grid(axis="x", visible=False)
    ax.set_ylabel("Seconds after injection")
    ax.set_ylim(0, 62)
    ax.legend(frameon=False, loc="upper left")
    fig.tight_layout()
    save(fig, "fig_7_7_fault_recovery.png")


# --- 8. latency floor decomposition (Stage C) ---------------------------------------
def fig_floor():
    rs = [r for r in R if r["exp"] == "C-calibration" and r["verdict"] == "RECORDED"]
    parts = [("first_decision_p50_ms", "Start to first\ndecision"),
             ("act_p50_ms", "Activity\nscheduled to done"),
             ("decision_p50_ms", "Decision\nturnaround"),
             ("timer_lag_p50_ms", "Timer lag\n(50 ms timer)")]
    m = [mean_sd([f(r, k) for r in rs])[0] or 0 for k, _ in parts]
    fig, ax = plt.subplots(figsize=(W, 2.1))
    ax.barh(range(len(parts)), m, color=C1, height=0.55)
    for i, v in enumerate(m):
        ax.text(v + 10, i, f"{v:.0f} ms", va="center", fontsize=8.5, color=INK)
    ax.set_yticks(range(len(parts)), [n for _, n in parts])
    ax.invert_yaxis()
    ax.grid(axis="y", visible=False)
    ax.set_xlabel("Median duration from the event journal (ms), one workflow at a time")
    ax.set_xlim(0, max(m) * 1.25)
    fig.tight_layout()
    save(fig, "fig_7_8_latency_floor.png")


if __name__ == "__main__":
    for fn in (fig_closed_loop, fig_resources, fig_waits, fig_open_loop, fig_idle,
               fig_sensitivity, fig_faults, fig_floor):
        fn()
