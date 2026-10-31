#!/usr/bin/env python3
"""Design and method diagrams for the report, drawn on a white background.

    python3 figures/fig_design.py          (run from Report_Template_AIDS_LaTeX/)

The style follows the result figures (fig_results.py): serif labels, thin black
outlines, white fills, one accent colour. Each function writes one PNG.
"""
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.patches import FancyArrowPatch, FancyBboxPatch, Rectangle  # noqa: E402

OUT = os.path.dirname(os.path.abspath(__file__))
plt.rcParams.update({
    "font.family": "serif",
    "font.serif": ["Liberation Serif", "Times New Roman", "DejaVu Serif"],
    "font.size": 9,
    "figure.facecolor": "white",
    "savefig.facecolor": "white",
    "savefig.dpi": 300,
    "savefig.bbox": "tight",
    "savefig.pad_inches": 0.03,
})
INK = "#1f2933"
ACC = "#1f4e79"
RED = "#c0504d"
GREEN = "#4f7f3a"
LIGHT = "#f2f2f2"
W = 5.8


def canvas(h, xlim=100, ylim=None):
    fig, ax = plt.subplots(figsize=(W, h))
    ax.set_xlim(0, xlim)
    ax.set_ylim(0, ylim if ylim else xlim * h / W)
    ax.axis("off")
    return fig, ax


def box(ax, x, y, w, h, text, bold=False, fill="white", edge=INK, lw=0.8, size=7.6, style="round,pad=0.1,rounding_size=0.6", mono=False):
    ax.add_patch(FancyBboxPatch((x, y), w, h, boxstyle=style, facecolor=fill, edgecolor=edge, linewidth=lw))
    ax.text(x + w / 2, y + h / 2, text, ha="center", va="center", fontsize=size,
            fontweight="bold" if bold else "normal", color=INK, linespacing=1.15,
            family="monospace" if mono else None)


def group(ax, x, y, w, h, label, dashed=True, bottom=False):
    ax.add_patch(Rectangle((x, y), w, h, facecolor="white", edgecolor="#8c8c8c", linewidth=0.7,
                           linestyle=(0, (4, 2)) if dashed else "-"))
    if bottom:
        ax.text(x + w - 1.0, y + 0.8, label, ha="right", va="bottom", fontsize=8, style="italic", color="#404040")
    else:
        ax.text(x + 1.0, y + h - 1.0, label, ha="left", va="top", fontsize=8, style="italic", color="#404040")


def arrow(ax, x1, y1, x2, y2, label=None, color=INK, style="-|>", ls="-", lw=0.8, lpos=0.5, loff=(0, 1.2), size=7.5):
    ax.add_patch(FancyArrowPatch((x1, y1), (x2, y2), arrowstyle=style, mutation_scale=8, color=color,
                                 linewidth=lw, linestyle=ls, shrinkA=1, shrinkB=1))
    if label:
        ax.text(x1 + (x2 - x1) * lpos + loff[0], y1 + (y2 - y1) * lpos + loff[1], label, ha="center",
                va="center", fontsize=size, color=color, backgroundcolor="white")


def save(fig, name):
    fig.savefig(os.path.join(OUT, name))
    plt.close(fig)
    print("wrote", name)


# --- architecture of one engine node and its database ---------------------------
def fig_architecture():
    fig, ax = canvas(3.7, 100, 64)
    group(ax, 1, 47, 98, 16, "Clients")
    for i, t in enumerate(["Order application\n(HTTP)", "Agent application\n(plan, tool, approve)", "Load generator\n(benchmark)", "Operator tools\n(status, faults)"]):
        box(ax, 3 + i * 24.5, 49, 21, 8.5, t)
    group(ax, 1, 11.2, 98, 33.3, "")
    box(ax, 3, 28.5, 29, 8.5, "HTTP API and shard router\nNotOwner redirect")
    box(ax, 35.5, 28.5, 29, 8.5, "Workflow workers\nreplay history and\nemit commands")
    box(ax, 68, 28.5, 29, 8.5, "Activity workers and\ntimer scanner")
    box(ax, 3, 15.8, 29, 9.5, "Lease manager\nacquire, renew, release")
    box(ax, 35.5, 15.8, 29, 9.5, "Decision committer\none transaction per workflow\ntask, fencing token checked", edge=ACC, lw=1.2)
    box(ax, 68, 15.8, 29, 9.5, "Idempotency and saga\nexact or semantic dedup,\nLIFO compensation")
    box(ax, 14, 1.2, 72, 8, "PostgreSQL 16, the only infrastructure dependency\nevents, executions, tasks, timers, shard_leases, activity_intents", edge=ACC, lw=1.2)
    for x0, x1 in ((13.5, 17.5), (38, 50), (62.5, 50), (87, 82.5)):
        arrow(ax, x0, 48.8, x1, 37.2)
    for x in (17.5, 50, 82.5):
        arrow(ax, x, 28.3, x, 24.7)
    arrow(ax, 17.5, 14.8, 30, 9.4)
    arrow(ax, 50, 14.8, 50, 9.4)
    arrow(ax, 82.5, 14.8, 70, 9.4)
    save(fig, "fig_4_1_architecture.png")


# --- workflow -> shard -> node ---------------------------------------------------
def fig_mapping():
    fig, ax = canvas(3.2, 100, 58)
    ax.text(12, 56.5, "Workflow identifiers", ha="center", fontsize=8.5, style="italic")
    ax.text(50, 56.5, "Fixed shards (S = 16)", ha="center", fontsize=8.5, style="italic")
    ax.text(88, 56.5, "Engine nodes (ring)", ha="center", fontsize=8.5, style="italic")
    wfs = ["order-1041", "order-1042", "agent-77", "invoice-9"]
    for i, w in enumerate(wfs):
        box(ax, 2, 33 - i * 8.5, 20, 5.5, w)
    shard_y = {}
    for j in range(8):
        y = 38 - j * 4.6
        sid = [0, 3, 5, 7, 9, 11, 13, 15][j]
        box(ax, 42, y, 16, 3.2, f"shard {sid}", size=7.2)
        shard_y[sid] = y + 1.6
    ax.text(50, 1.0, "...", ha="center", fontsize=9)
    nodes = {"node A": 33, "node B": 20, "node C": 7}
    for n, y in nodes.items():
        box(ax, 78, y, 20, 6, n)
    for i, s_ in enumerate([3, 9, 5, 15]):
        arrow(ax, 22.4, 33 - i * 8.5 + 2.75, 41.6, shard_y[s_], color=ACC)
    owner = {0: "node A", 3: "node B", 5: "node A", 7: "node C", 9: "node B", 11: "node A", 13: "node B", 15: "node C"}
    for s_, n in owner.items():
        arrow(ax, 58.4, shard_y[s_], 77.6, nodes[n] + 3, color="#7f7f7f", lw=0.6)
    ax.text(32, 46.3, "hash(id) mod S:\nfixed for the life\nof the workflow", ha="center", fontsize=7.3, color=ACC)
    ax.text(68, 46.3, "consistent-hash ring:\nonly ownership moves\nwhen nodes change", ha="center", fontsize=7.3, color="#505050")
    save(fig, "fig_4_2_two_level_mapping.png")


# --- lease, expiry and fencing -----------------------------------------------------
def fig_fencing():
    fig, ax = canvas(2.9, 100, 50)
    ax.plot([3, 97], [31, 31], color=INK, linewidth=0.8)
    pts = [(11, "t = 0", "A owns shard 5\ntoken = 7"), (30, "t = 3 s", "A is frozen\n(SIGSTOP)"),
           (50, "t = 12.5 s", "lease expires,\nB acquires with\ntoken = 8"), (70, "t = 33 s", "A thaws, still\nbelieves it owns"),
           (89, "t = 36 s", "A's commit is\nrejected")]
    for x, lab, t in pts:
        ax.plot([x, x], [30, 32], color=INK, linewidth=0.8)
        ax.text(x, 28.3, lab, ha="center", va="top", fontsize=7.3, color="#404040")
        box(ax, x - 8.5, 37, 17, 8.5, t, size=7.2)
        arrow(ax, x, 36.8, x, 32.3, lw=0.6)
    ax.text(25, 21.6, "Every mutating transaction re-checks the token", ha="center", fontsize=7.6, style="italic")
    ax.add_patch(Rectangle((2, 2), 46, 17, facecolor="white", edgecolor=INK, linewidth=0.8))
    ax.text(4, 10.5, "BEGIN;\nSELECT fencing_token FROM shard_leases\n  WHERE shard_id = 5 FOR UPDATE;  -- 8\n-- the caller holds 7: ROLLBACK\nCOMMIT;",
            ha="left", va="center", fontsize=6.1, family="monospace", color=INK, linespacing=1.25)
    box(ax, 53, 2, 45, 17, "Measured on AWS (freeze 35 s, TTL 10 s)\nleases expired after 12.2 to 12.8 s\nshards re-owned after 32 to 48 s\nstale writes rejected: 22, 4 and 5\njournal invariants violated: 0", size=7.2,
        edge=ACC, lw=1.1, style="square,pad=0.2")
    save(fig, "fig_4_3_lease_fencing.png")


# --- the workflow task loop (replay) ------------------------------------------------
def fig_task_loop():
    fig, ax = canvas(2.0, 100, 34)
    steps = ["Poll a task\n(FOR UPDATE\nSKIP LOCKED)", "Load the run's\nevent history", "Re-execute the\nworkflow function\nagainst history",
             "Collect new\ncommands\n(schedule, timer)", "Commit events,\ntasks and status\nin one transaction"]
    xs = [1, 21.25, 41.5, 61.75, 82]
    for x, t in zip(xs, steps):
        box(ax, x, 17, 17, 12, t, size=7.2)
    for a, b in zip(xs[:-1], xs[1:]):
        arrow(ax, a + 17.3, 23, b - 0.3, 23)
    arrow(ax, 90.5, 16.8, 90.5, 10, style="-")
    arrow(ax, 90.5, 10, 9.5, 10, style="-")
    arrow(ax, 9.5, 10, 9.5, 16.8)
    ax.text(50, 6.2, "the fencing token is re-checked inside the commit, so a stale owner writes nothing", ha="center", fontsize=7.4, style="italic", color=ACC)
    ax.text(50, 2.0, "completed activities return their recorded results during replay, so no side effect runs twice", ha="center", fontsize=7.4, style="italic", color="#404040")
    save(fig, "fig_4_4_task_loop.png")


# --- AWS testbed ------------------------------------------------------------------------
def fig_testbed():
    fig, ax = canvas(3.4, 100, 58)
    group(ax, 1, 1, 98, 56, "AWS us-east-1, one VPC (10.91.0.0/16), availability zone us-east-1a", dashed=False)
    group(ax, 3, 21, 63, 31, "Amazon EKS cluster (Kubernetes 1.37)")
    for i in range(3):
        box(ax, 5.5 + i * 20, 37, 18, 9.5, f"engine node {i + 1}\nm7i-flex.large\npod durexec-{i}", size=7.2)
    box(ax, 5.5, 24, 28, 9.5, "harness node\nc7i-flex.large (tainted)\nload generator, probe", size=7.2)
    box(ax, 36.5, 24, 27, 9.5, "EKS control plane\npublic API: operator IP only\nprivate endpoint", size=7.2)
    box(ax, 71, 36, 27, 14, "PostgreSQL 16 host\nm7i-flex.large, 30 GB gp3\nsynchronous_commit on\nsingle host, no standby", size=7.2, edge=ACC, lw=1.2)
    box(ax, 71, 21, 27, 10.5, "in-VPC operator\nc7i-flex.large\nno inbound; SSM only", size=7.2)
    box(ax, 5.5, 4, 28, 10, "Amazon ECR\nengine and load-generator\nimages (cf986fb)", size=7.2)
    box(ax, 36.5, 4, 27, 10, "AWS Secrets Manager\ndatabase credentials", size=7.2)
    box(ax, 71, 4, 27, 10, "Amazon S3\nTerraform state and\nevidence", size=7.2)
    arrow(ax, 66.2, 43, 70.6, 43)
    ax.text(68.4, 45.5, "5432", ha="center", fontsize=6.8)
    arrow(ax, 70.6, 26.5, 63.8, 28.5, ls="--")
    ax.text(67.2, 30.6, "kubectl", ha="center", fontsize=6.8)
    arrow(ax, 19.5, 14.3, 19.5, 23.7, ls="--", lw=0.6)
    arrow(ax, 50, 14.3, 50, 23.7, ls="--", lw=0.6)
    arrow(ax, 84.5, 20.7, 84.5, 14.3, ls="--", lw=0.6)
    save(fig, "fig_6_1_aws_testbed.png")


# --- what a fault trial measures --------------------------------------------------------
def fig_fault_timeline():
    fig, ax = canvas(1.9, 100, 32)
    ax.add_patch(Rectangle((6, 14.2), 88, 2.4, facecolor=LIGHT, edgecolor="none"))
    ax.plot([3, 97], [15.4, 15.4], color=INK, linewidth=0.8)
    ax.text(6, 30.5, "open-loop load at 65.6 workflows/s for 180 s (shaded)", ha="left", fontsize=7.4, color="#404040", style="italic")
    marks = [(14, "t_inject", "fault applied", True), (26, "t_progress", "first new event\non those shards", False),
             (44, "t_expire", "victim's leases\nexpired or moved", False), (62, "t_claim", "all shards re-held\nwith a higher token", True),
             (84, "t_ring", "all pods Ready,\nno shard unowned", True)]
    for x, name, desc, up in marks:
        ax.plot([x, x], [13.6, 17.2], color=RED if name == "t_inject" else ACC, linewidth=1.2)
        ax.text(x, 23.5 if up else 7.0, f"{name}\n{desc}", ha="center", va="center", fontsize=7.2, color=INK, linespacing=1.1)
    ax.text(97, 2.0, "afterwards: drain, journal invariants, verdict", ha="right", fontsize=7.2, style="italic", color="#404040")
    save(fig, "fig_6_2_fault_timeline.png")


if __name__ == "__main__":
    for fn in (fig_architecture, fig_mapping, fig_fencing, fig_task_loop, fig_testbed, fig_fault_timeline):
        fn()
