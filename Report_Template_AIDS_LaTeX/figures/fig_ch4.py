"""Figures for Chapter 4 (System Design) of the project report.

Run from this directory:  python3 fig_ch4.py
"""

import math

from matplotlib.patches import Circle

from fig_common import FAINT, INK, PALETTE, WIRE, arrow, band, box, canvas, label, save


# ---------------------------------------------------------------- Fig 4.1
def fig_architecture():
    """The five architectural tiers and the single dependency underneath."""
    fig, ax = canvas(7.0, 5.6, ylim=(0, 108))

    band(ax, 50, 99, 96, 14, "client tier", colour="grey")
    for x, t in [(18, "Web UI\n(order form)"), (41, "Admin panel\n(fail-a-step)"),
                 (64, "AI agent app\n(plan, tool, approve)"),
                 (86, "Load generator\n(benchmark)")]:
        box(ax, x, 97, 21, 9.5, t, colour="grey", size=7.4)

    band(ax, 50, 81, 96, 15,
         "gateway tier  —  stateless, replicated on every node", colour="blue")
    box(ax, 22, 78, 36, 9.5, "HTTP API\nPOST /v1/workflows,  signals,  cancel",
        colour="blue", size=7.0)
    box(ax, 55, 78, 26, 9.5, "shard router\nfnv32(workflow_id) % 16",
        colour="blue", size=7.4)
    box(ax, 84, 78, 24, 9.5, "NotOwner redirect\n+ ring refresh",
        colour="blue", size=7.4)

    band(ax, 50, 59, 96, 24,
         "execution tier  —  one Go process per node, N owned shards",
         colour="green")
    box(ax, 19, 61, 30, 10, "workflow workers  × 4\nreplay history, emit commands",
        colour="green", size=7.4)
    box(ax, 52, 61, 30, 10, "activity workers  × 8\nperform side effects",
        colour="green", size=7.4)
    box(ax, 84, 61, 24, 10, "timer scanner  × 1\nfire_at ≤ now()",
        colour="green", size=7.4)
    box(ax, 30, 50, 56, 8.5,
        "decision committer  —  one transaction per workflow task",
        colour="green", size=7.4, bold=True)
    box(ax, 78, 50, 38, 8.5, "saga manager  —  LIFO compensation",
        colour="green", size=7.4)

    band(ax, 50, 36, 96, 14, "control tier", colour="amber")
    box(ax, 22, 34, 32, 9.5, "lease manager\nacquire / renew / hand back",
        colour="amber", size=7.0)
    box(ax, 55, 34, 28, 9.5, "fencing guard\nre-check token in every tx",
        colour="amber", size=7.0)
    box(ax, 84, 34, 24, 9.5, "idempotency\nexact + semantic", colour="amber", size=7.4)

    band(ax, 50, 16, 96, 18,
         "persistence tier  —  the only infrastructure dependency", colour="purple")
    box(ax, 28, 13, 46, 11,
        "PostgreSQL 16 leader\nevents · executions · tasks\n"
        "timers · shard_leases · activity_intents",
        colour="purple", size=7.0, bold=True)
    box(ax, 77, 13, 38, 11,
        "PostgreSQL 16 standby\nstreaming replication (WAL)",
        colour="purple", size=7.4)
    arrow(ax, (51.5, 13), (57.5, 13), label="WAL", size=7.0, dy=2.6)

    for y0, y1 in [(91.5, 88.5), (73.0, 71.0), (46.0, 43.0), (28.5, 25.0)]:
        for x in (25, 50, 75):
            arrow(ax, (x, y0), (x, y1), lw=1.0)

    label(ax, 50, 2.5,
          "every arrow crossing the control tier carries a fencing-token check; "
          "no arrow leaves the host",
          size=7.4, colour=FAINT)
    save(fig, "fig_4_1_architecture.png")


# ---------------------------------------------------------------- Fig 4.2
def fig_two_level_mapping():
    """workflow_id -> shard_id is fixed; shard_id -> node moves. Keeping the
    two mappings separate makes rebalancing an ownership handoff instead of a
    data migration."""
    fig, ax = canvas(7.0, 4.4, ylim=(0, 78))

    box(ax, 13, 66, 24, 10, 'workflow_id\n"order-8f3a"', colour="grey", size=8.2)
    box(ax, 46, 66, 26, 10, "shard_id  =  2\nfnv32(id) mod 16", colour="blue",
        size=8.2, bold=True)
    box(ax, 82, 66, 24, 10, "node\n“node-b”", colour="green",
        size=8.2, bold=True)

    arrow(ax, (25.5, 66), (32.5, 66), label="hash, N fixed at deploy", size=7.2, dy=3.0)
    arrow(ax, (59.5, 66), (69.5, 66), label="consistent hash ring", size=7.2, dy=3.0)

    label(ax, 46, 56.5, "NEVER changes for the lifetime of the workflow",
          size=7.6, bold=True, colour=PALETTE["blue"][1])
    label(ax, 82, 56.5, "changes when a node\njoins or dies", size=7.6, bold=True,
          colour=PALETTE["green"][1])

    cx, cy, r = 27, 25, 19
    ax.add_patch(Circle((cx, cy), r, fill=False, edgecolor=FAINT, lw=1.1,
                        ls=(0, (4, 3)), zorder=2))
    label(ax, cx, cy, "consistent\nhash ring\n~150 vnodes\nper node",
          size=7.6, colour=WIRE)

    for name, deg, col in [("node-a", 60, "green"), ("node-b", 180, "blue"),
                           ("node-c", 300, "amber")]:
        a = math.radians(deg)
        box(ax, cx + (r + 6.5) * math.cos(a), cy + (r + 6.5) * math.sin(a),
            15, 6.5, name, colour=col, size=7.6, bold=True, radius=1.0)

    for i in range(16):
        a = math.radians(i * 22.5 + 11)
        ax.plot([cx + r * math.cos(a)], [cy + r * math.sin(a)], marker="o",
                ms=3.4, color=PALETTE["purple"][1], zorder=5)
    label(ax, cx, cy - r - 7.5, "16 shard positions", size=7.2, colour=FAINT)

    box(ax, 77, 34, 40, 16,
        "node-d joins a 3-node cluster\n\n"
        "5 of 16 shards change owner\n11 shards are untouched",
        colour="green", size=8.2)
    box(ax, 77, 13, 40, 16,
        "had workflow_id hashed straight to\nnodes instead, every workflow in a\n"
        "moved range would need its log\nmigrated mid-flight",
        colour="red", size=8.0)
    label(ax, 77, 23.5, "versus", size=7.6, bold=True, colour=FAINT)
    save(fig, "fig_4_2_two_level_mapping.png")


# ---------------------------------------------------------------- Fig 4.3
def fig_task_loop():
    """The workflow task loop. Step 8 is one transaction; step 9 throws the
    in-memory state away, which is why correctness never depends on it."""
    fig, ax = canvas(5.9, 7.4, ylim=(0, 128))

    for y, t in [
        (118, "1   poll a WORKFLOW task from  tasks"),
        (107, "2   load full history: SELECT * FROM events\n"
              "      WHERE workflow_id, run_id  ORDER BY seq"),
        (94, "3   build a fresh WorkflowContext\n"
             "      seqCounter = 0,  cursor = 0,  commands = []"),
    ]:
        box(ax, 50, y, 84, 9.5 if "\n" not in t else 11, t, colour="blue", size=8.0)
    box(ax, 50, 83, 84, 9, "4   start the workflow function on a coroutine",
        colour="green", size=8.0)

    box(ax, 50, 70, 84, 9,
        "5   code calls ExecuteActivity / Sleep / WaitForSignal\n"
        "      assign  k := seqCounter++   (program order)",
        colour="green", size=8.0)

    box(ax, 50, 57.5, 30, 8, "is there an event\nfor this k ?", colour="amber",
        size=8.0, bold=True)

    box(ax, 19, 44, 34, 11,
        "REPLAY MODE\nresolve the Future from\nhistory;  emit no command",
        colour="purple", size=7.8)
    box(ax, 78, 44, 36, 11,
        "NEW MODE\nappend a command;  return\nan unresolved Future",
        colour="teal", size=7.8)

    arrow(ax, (37, 56), (22, 50), label="yes", size=7.4, dx=-3, dy=1.6)
    arrow(ax, (63, 56), (74, 50), label="no", size=7.4, dx=3, dy=1.6)

    box(ax, 50, 29, 84, 9,
        "6   run until the coroutine blocks on an unresolved Future, or returns\n"
        "7   collect  commands",
        colour="green", size=8.0)

    box(ax, 50, 14.5, 84, 14,
        "8   ONE PostgreSQL TRANSACTION\n"
        "      verify fencing token (FOR UPDATE)  ·  INSERT events  ·  "
        "INSERT tasks / timers\n"
        "      UPDATE executions.last_seq  ·  DELETE this task row   →   COMMIT",
        colour="red", size=7.9, lw=1.7)

    box(ax, 50, 3.5, 84, 7, "9   throw the in-memory workflow state away",
        colour="grey", size=8.0)

    for y0, y1 in [(112.8, 112.0), (101, 100), (88.2, 87.5), (78.2, 74.6),
                   (65.4, 61.6), (33.6, 21.6)]:
        arrow(ax, (50, y0), (50, y1), lw=1.1)
    arrow(ax, (19, 38.4), (19, 33.6), lw=1.1)
    arrow(ax, (78, 38.4), (78, 33.6), lw=1.1)
    arrow(ax, (50, 10.8), (50, 7.4), lw=1.1)
    arrow(ax, (6, 33.6), (6, 70), lw=1.0, dashed=True,
          label="loop until blocked", size=7.0, dx=0, dy=0, zorder=2)
    save(fig, "fig_4_3_task_loop.png")


# ---------------------------------------------------------------- Fig 4.4
def fig_schema():
    """The six tables. Every one carries shard_id, which is what sharding
    physically is in this design."""
    fig, ax = canvas(7.0, 5.0, ylim=(0, 92))

    def table(x, y, w, h, name, cols, pk, colour):
        box(ax, x, y, w, h, "", colour=colour, radius=1.0)
        label(ax, x, y + h / 2 - 2.6, name, size=8.6, bold=True, mono=True,
              colour=PALETTE[colour][1])
        ax.plot([x - w / 2 + 2, x + w / 2 - 2],
                [y + h / 2 - 5.0, y + h / 2 - 5.0],
                color=PALETTE[colour][1], lw=0.9)
        label(ax, x - w / 2 + 2.5, y + h / 2 - 6.6, cols, size=6.9, mono=True,
              ha="left", va="top")
        label(ax, x, y - h / 2 + 2.3, pk, size=6.5, colour=FAINT, mono=True)

    table(21, 68, 38, 40, "events",
          "shard_id     INT\nworkflow_id  TEXT\nrun_id       UUID\n"
          "seq          BIGINT\nevent_type   TEXT\npayload      JSONB\n"
          "created_at   TIMESTAMPTZ",
          "PK (shard_id, workflow_id, run_id, seq)", "purple")

    table(64, 76, 38, 24, "executions",
          "shard_id       INT\nworkflow_id    TEXT\nrun_id         UUID\n"
          "workflow_type  TEXT\nstatus         TEXT\nlast_seq       BIGINT",
          "PK (shard_id, workflow_id)   ← no run_id", "blue")

    table(64, 48, 38, 26, "tasks",
          "shard_id       INT\ntask_id        BIGSERIAL\ntask_type      TEXT\n"
          "visible_at     TIMESTAMPTZ\nlocked_until   TIMESTAMPTZ\n"
          "lock_token     UUID\nattempt        INT",
          "PK (shard_id, task_id)", "green")

    table(21, 29, 38, 19, "timers",
          "shard_id     INT\nfire_at      TIMESTAMPTZ\nworkflow_id  TEXT\n"
          "timer_seq    BIGINT",
          "PK (shard_id, workflow_id, run_id, timer_seq)", "teal")

    table(21, 9, 38, 15, "shard_leases",
          "shard_id       INT\nowner_node     TEXT\nfencing_token  BIGINT\n"
          "expires_at     TIMESTAMPTZ",
          "PK (shard_id)", "amber")

    table(64, 20, 38, 20, "activity_intents",
          "shard_id       INT\nworkflow_id    TEXT\nscheduled_seq  BIGINT\n"
          "activity_type  TEXT\nintent_text    TEXT\nembedding      VECTOR(384)",
          "PK (shard_id, workflow_id, run_id, scheduled_seq)", "red")

    arrow(ax, (40, 72), (45, 76), label="derived from", size=6.8, dy=2.2)
    arrow(ax, (40, 62), (45, 50), label="schedules", size=6.8, dy=2.2)
    arrow(ax, (31, 48), (26, 38.5), label="schedules", size=6.8, dx=-5, dy=0)
    arrow(ax, (45, 24), (40, 56), lw=1.0, dashed=True, label="candidate set",
          size=6.8, dx=-6, dy=0)
    arrow(ax, (33, 9), (24, 19.5), lw=1.0, dashed=True)
    label(ax, 47, 4, "every statement is scoped by  shard_id", size=7.6,
          bold=True, colour=FAINT)
    save(fig, "fig_4_4_schema.png")


# ---------------------------------------------------------------- Fig 4.5
def fig_sequence():
    """A worked trace of order-8f3a across three tasks. Note that seq 5
    records the intent to charge before any money moves."""
    fig, ax = canvas(7.0, 6.2, ylim=(0, 112))

    for name, x, col in [("Client", 9, "grey"), ("Gateway", 26, "blue"),
                         ("Workflow\nworker", 46, "green"),
                         ("Activity\nworker", 68, "teal"),
                         ("PostgreSQL", 90, "purple")]:
        box(ax, x, 106, 17, 8, name, colour=col, size=8.0, bold=True)
        ax.plot([x, x], [4, 101], color=FAINT, lw=0.9, ls=(0, (3, 3)), zorder=1)

    def msg(y, x0, x1, text, up=1.9, size=7.3):
        arrow(ax, (x0, y), (x1, y), lw=1.1)
        label(ax, (x0 + x1) / 2, y + up, text, size=size, colour=WIRE, bg=True)

    label(ax, 6, 97, "TASK 1  —  the workflow starts", size=8.2, bold=True,
          ha="left", colour=PALETTE["blue"][1])
    msg(92, 9, 26, "POST /v1/workflows")
    msg(86, 26, 90, "tx:  seq 1 WorkflowExecutionStarted,  seq 2 WorkflowTaskScheduled,\n"
                    "executions(RUNNING),  tasks(WORKFLOW)", up=3.2)
    msg(76, 90, 46, "poll → WORKFLOW task,  history = [1,2]")
    label(ax, 46, 70.5, "run code:  k=0 ExecuteActivity(“ChargePayment”)\n"
                        "no event for k=0  →  NEW MODE  →  block",
          size=7.2, bg=True)
    msg(62, 46, 90, "tx:  seq 3-4 task started/completed,\n"
                    "seq 5 ActivityTaskScheduled{k:0, dedup_key:“8f3a:0”},  "
                    "tasks(ACTIVITY)", up=3.2)

    ax.plot([4, 97], [55, 55], color=FAINT, lw=0.8, ls=(0, (5, 3)))
    label(ax, 6, 51, "TASK 2  —  the activity runs", size=8.2, bold=True,
          ha="left", colour=PALETTE["teal"][1])
    msg(45, 90, 68, "poll → ACTIVITY task,  read seq 5 for input")
    label(ax, 68, 39, "call the payment API\n→ txn_id = ch_991", size=7.2,
          colour=PALETTE["red"][1], bold=True, bg=True)
    msg(32, 68, 90, "tx:  seq 6-7 started/completed{result},\n"
                    "seq 8 WorkflowTaskScheduled", up=3.2)

    ax.plot([4, 97], [25, 25], color=FAINT, lw=0.8, ls=(0, (5, 3)))
    label(ax, 6, 21, "TASK 3  —  replay, then forward", size=8.2, bold=True,
          ha="left", colour=PALETTE["purple"][1])
    msg(15, 90, 46, "poll → WORKFLOW task,  history = [1..8]")
    label(ax, 48, 9.5,
          "re-runs from line 1:  k=0 resolves from history — NO API CALL\n"
          "k=1 NotifyRestaurant has no event  →  NEW MODE",
          size=7.2, bg=True)
    msg(3, 46, 90, "tx:  seq 9-11,  next ACTIVITY task", up=2.0)
    save(fig, "fig_4_5_sequence.png")


# ---------------------------------------------------------------- Fig 4.6
def fig_lease_fencing():
    """Why the lease alone is not what makes failover safe. The token
    re-check inside the write transaction is."""
    fig, ax = canvas(7.0, 5.0, ylim=(0, 90))

    label(ax, 50, 86, "a frozen node cannot corrupt the shard it no longer owns",
          size=9, bold=True)

    ax.plot([8, 92], [62, 62], color=WIRE, lw=1.2)
    for x, t in [(10, "t+0s"), (27, "t+3s"), (44, "t+10s"), (61, "t+10.1s"),
                 (80, "t+10.3s")]:
        ax.plot([x], [62], marker="|", ms=9, color=WIRE)
        label(ax, x, 58.5, t, size=7.3, colour=FAINT)

    box(ax, 15, 73, 24, 12, "node A owns shard 2\ntoken = 7\npolls a task, works",
        colour="green", size=7.6)
    box(ax, 36, 73, 20, 12, "node A freezes\nGC pause / VM\nsuspend",
        colour="amber", size=7.6)
    box(ax, 58, 73, 20, 12, "lease expires\nnode B wins the\nacquire UPDATE",
        colour="blue", size=7.6)
    box(ax, 82, 73, 22, 12, "node B replays\nfrom seq 1 and\nresumes",
        colour="blue", size=7.6)
    for x in (15, 36, 58, 82):
        arrow(ax, (x, 66.8), (x, 63.4), lw=1.0)

    label(ax, 50, 50, "node A now wakes up, still believing it holds the lease",
          size=8.4, bold=True, colour=PALETTE["red"][1])

    box(ax, 26, 29, 44, 30,
        "WITHOUT fencing\n\n"
        "BEGIN;\nINSERT INTO events … ;\nCOMMIT;\n\n"
        "Both A and B write.\nSplit-brain: duplicate events,\n"
        "duplicate side effects.",
        colour="red", size=7.8)

    box(ax, 74, 29, 46, 30,
        "WITH fencing  (what we do)\n\n"
        "BEGIN;\nSELECT fencing_token … FOR UPDATE;\n"
        "  → reads 8,  myToken = 7\nif tok != myToken: ROLLBACK\n\n"
        "A commits nothing — not one event,\nnot one task row.",
        colour="green", size=7.8)

    box(ax, 50, 6.5, 92, 9,
        "invariant:  a node that has lost ownership cannot mutate shard state, "
        "whatever it believes.\nFOR UPDATE serialises the ownership check against "
        "the write, so no window exists between them.",
        colour="white", size=7.8)
    save(fig, "fig_4_6_lease_fencing.png")


# ---------------------------------------------------------------- Fig 4.7
def fig_crash_windows():
    """Every place a crash can land during one activity. Window C is
    irreducible, and the engine says so rather than claiming exactly-once."""
    fig, ax = canvas(7.0, 4.3, ylim=(0, 74))

    ax.plot([8, 92], [52, 52], color=WIRE, lw=1.3)
    for x, t in [(12, "seq 5\nActivityTask\nScheduled"),
                 (34, "worker polls\nthe task"),
                 (56, "payment API\nreturns OK"),
                 (78, "seq 7\nActivityTask\nCompleted")]:
        ax.plot([x], [52], marker="o", ms=6, color=PALETTE["blue"][1], zorder=5)
        label(ax, x, 62, t, size=7.2)

    for x, name, state, col, outcome in [
        (21, "A", "task row exists,\nunlocked", "green",
         "another worker\npolls it; the activity\nruns once"),
        (45, "B", "task locked,\nlock expiring", "green",
         "locked_until passes,\nthe task is redelivered;\nruns once"),
        (69, "C", "money moved,\nhistory silent", "red",
         "lock expires, task is\nredelivered, THE API IS\nCALLED AGAIN"),
        (90, "D", "event 7 written", "green",
         "replay resumes;\nthe activity is\nnot re-run"),
    ]:
        w = 20 if name != "D" else 16
        box(ax, x, 46, w, 7, f"window {name}", colour=col, size=8.0, bold=True)
        label(ax, x, 38, state, size=7.0, colour=FAINT)
        box(ax, x, 26, w + 1, 15, outcome, colour=col, size=7.0)
        label(ax, x, 15.5, "SAFE" if col == "green" else "NOT SAFE",
              size=8.0, bold=True, colour=PALETTE[col][1])

    box(ax, 50, 7.0, 92, 12,
        "window C is irreducible: the side effect and the record of it live in two\n"
        "systems with no transaction between them.  the engine's contribution is a\n"
        "stable dedup key — “8f3a:0” — that the downstream resource deduplicates on.",
        colour="white", size=7.0)
    save(fig, "fig_4_7_crash_windows.png")


# ---------------------------------------------------------------- Fig 4.8
def fig_saga():
    """Compensation is a workflow pattern the engine makes durable, not a
    subsystem. Registration happens after the forward step succeeds."""
    fig, ax = canvas(7.0, 4.2, ylim=(0, 70))

    label(ax, 50, 66, "forward path", size=8.6, bold=True, colour=PALETTE["green"][1])
    for x, t in [(16, "ChargePayment"), (38, "ReserveInventory"),
                 (60, "NotifyRestaurant")]:
        box(ax, x, 56, 20, 8, t, colour="green", size=7.8)
    arrow(ax, (26.5, 56), (27.5, 56))
    arrow(ax, (48.5, 56), (49.5, 56))
    box(ax, 83, 56, 20, 8, "restaurant\nREJECTS", colour="red", size=7.8, bold=True)
    arrow(ax, (70.5, 56), (72.5, 56))

    label(ax, 16, 47.5, "then push\nRefundPayment", size=6.9, colour=FAINT)
    label(ax, 38, 47.5, "then push\nReleaseInventory", size=6.9, colour=FAINT)
    label(ax, 60, 47.5, "fails before any\ncompensation is pushed", size=6.9,
          colour=FAINT)

    box(ax, 14, 28, 26, 22, "", colour="white")
    label(ax, 14, 37.0, "compensation stack\n(in memory only)", size=7.4, bold=True,
          colour=WIRE)
    box(ax, 14, 30.0, 22, 6, "ReleaseInventory", colour="amber", size=7.4, radius=0.8)
    box(ax, 14, 22.5, 22, 6, "RefundPayment", colour="amber", size=7.4, radius=0.8)
    label(ax, 14, 18.6, "top of stack ↑", size=6.8, colour=FAINT)

    label(ax, 58, 40, "Compensate()  pops LIFO", size=8.6, bold=True,
          colour=PALETTE["amber"][1])
    box(ax, 44, 29, 20, 9, "1.  Release\nInventory", colour="amber", size=7.8)
    box(ax, 68, 29, 20, 9, "2.  Refund\nPayment", colour="amber", size=7.8)
    arrow(ax, (54.5, 29), (57.5, 29))
    box(ax, 90, 29, 14, 9, "status\nFAILED", colour="red", size=7.8)
    arrow(ax, (78.5, 29), (82.5, 29))
    arrow(ax, (27.5, 29), (33.5, 29), label="pop", size=7.0, dy=2.4)

    box(ax, 50, 7, 92, 10,
        "the stack is NOT journalled.  it is rebuilt identically on every replay, "
        "because the code is deterministic\nand the successes it depends on are "
        "already in history.  a compensation that exhausts its retries leaves the\n"
        "workflow in COMPENSATING_FAILED, which requires a human  —  "
        "there is no automatic resolution.",
        colour="white", size=7.4)
    save(fig, "fig_4_8_saga.png")


# ---------------------------------------------------------------- Fig 4.9
def fig_semantic_dedup():
    """Where the similarity verdict is computed, and why it must be on the
    workflow path and journalled rather than inside the activity worker."""
    fig, ax = canvas(7.0, 5.4, ylim=(0, 100))

    box(ax, 50, 95, 62, 7,
        "ExecuteActivity(“book_flight”, args)   mode = Semantic,   k = 7",
        colour="grey", size=8.0, bold=True)

    band(ax, 26, 54, 48, 74, "NEW MODE  —  first execution", colour="teal")
    for y, t in [
        (82, "1   intent := canonicalize(type, sorted args)"),
        (73, "2   candidates := prior Semantic activities of the\n"
             "      same type in THIS run that COMPLETED"),
        (63, "3   vec := embed(intent)      ← network call,\n"
             "      non-deterministic"),
        (54, "4   best, sim := argmax cosine(vec, cand.vec)"),
        (45, "5   verdict := DUPLICATE  if sim ≥ τ_high = 0.90\n"
             "                     DISTINCT   if sim < τ_low  = 0.85\n"
             "                     ESCALATE  otherwise"),
    ]:
        box(ax, 26, y, 44, 7 if "\n" not in t else 9.5, t, colour="white", size=7.2)

    box(ax, 26, 32, 44, 9.5,
        "6   APPEND  SemanticDedupEvaluated{\n"
        "       k:7, verdict, matched_k:3, similarity:0.94,\n"
        "       threshold:0.90, model:“all-MiniLM-L6-v2” }",
        colour="red", size=7.1, lw=1.6)

    box(ax, 26, 20, 44, 11,
        "7   DUPLICATE → resolve the Future with the\n"
        "       recorded result of k=3.  NO activity is\n"
        "       scheduled.  NO side effect occurs.",
        colour="amber", size=7.2)

    band(ax, 76, 54, 44, 74, "REPLAY MODE  —  every recovery after", colour="purple")
    box(ax, 76, 76, 40, 11,
        "1   read SemanticDedupEvaluated{k:7}\n      from history",
        colour="white", size=7.4)
    box(ax, 76, 60, 40, 16,
        "2   NO embedding call\n      NO similarity computation\n      NO network",
        colour="green", size=7.6, bold=True)
    box(ax, 76, 42, 40, 12,
        "3   apply the recorded verdict\n      identical outcome, every replay",
        colour="white", size=7.4)
    box(ax, 76, 24, 40, 15,
        "consequence: recovery never depends\non the embedding service being up, "
        "and\nevery verdict is auditable from a row",
        colour="purple", size=7.4)

    arrow(ax, (38, 91.5), (26, 87.5), lw=1.1)
    arrow(ax, (62, 91.5), (76, 83), lw=1.1)
    for y0, y1 in [(78, 77.3), (68.2, 67.8), (58.3, 57.6), (50.4, 49.9),
                   (40.2, 37), (27.2, 25.6)]:
        arrow(ax, (26, y0), (26, y1), lw=1.0)
    arrow(ax, (76, 70.4), (76, 68.2), lw=1.0)
    arrow(ax, (76, 51.8), (76, 48.2), lw=1.0)

    box(ax, 50, 6.0, 94, 11,
        "the verdict is non-deterministic, so it is computed ONCE and journalled —\n"
        "the SideEffect pattern applied to a dedup decision.  computing it inside the\n"
        "activity worker would either diverge on replay or be skipped, and either breaks it.",
        colour="white", size=6.9)
    save(fig, "fig_4_9_semantic_dedup.png")


# ---------------------------------------------------------------- Fig 4.10
def fig_node_runtime():
    """What actually runs inside one node process."""
    fig, ax = canvas(6.4, 4.4, ylim=(0, 82))

    band(ax, 50, 42, 96, 76, "node-a   —   one process, one Go binary",
         colour="blue")

    box(ax, 27, 72, 40, 8, "HTTP server\ngateway API, /healthz, /metrics",
        colour="grey", size=7.4)
    box(ax, 77, 72, 36, 8, "metrics collector  × 1", colour="purple", size=7.6)
    box(ax, 27, 61, 40, 9,
        "lease manager  × 1\nrenew owned shards / 3s,  acquire / 5s",
        colour="amber", size=7.6)
    box(ax, 77, 61, 36, 9, "fault injector  × 1\n(test builds only)",
        colour="red", size=7.6, dashed=True)

    band(ax, 50, 31, 90, 32, "per owned shard", colour="green")
    for i, x in enumerate((22, 50, 78)):
        box(ax, x, 40, 26, 6.5, f"shard {i * 5 + 2}", colour="green", size=7.4,
            bold=True, radius=0.9)
        box(ax, x, 32, 26, 6.5, "timer scanner  × 1", colour="white", size=7.0,
            radius=0.9)
        box(ax, x, 25, 26, 6.5, "workflow workers  × 4", colour="white", size=7.0,
            radius=0.9)
        box(ax, x, 18, 26, 6.5, "activity workers  × 8", colour="white", size=7.0,
            radius=0.9)

    box(ax, 50, 7.0, 92, 11,
        "every worker is a plain loop: poll, work, commit, repeat — with backoff from\n"
        "10 ms to 1 s when the poll is empty.  a worker that loses its shard sees\n"
        "ErrLostOwnership in its own commit and exits; no shutdown coordination needed.",
        colour="white", size=6.9)
    save(fig, "fig_4_10_node_runtime.png")


if __name__ == "__main__":
    fig_architecture()
    fig_two_level_mapping()
    fig_task_loop()
    fig_schema()
    fig_sequence()
    fig_lease_fencing()
    fig_crash_windows()
    fig_saga()
    fig_semantic_dedup()
    fig_node_runtime()
