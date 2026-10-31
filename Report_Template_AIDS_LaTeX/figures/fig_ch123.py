"""Figures for Chapters 1 to 3 of the project report.

Run from this directory:  python3 fig_ch123.py
"""

from fig_common import FAINT, INK, PALETTE, WIRE, arrow, band, box, canvas, label, save


# ---------------------------------------------------------------- Fig 1.1
def fig_crash_dilemma():
    """The two outcomes available to a naive system when a crash lands
    halfway through a multi-step process, and why both are wrong."""
    fig, ax = canvas(7.0, 4.3, ylim=(0, 72))

    steps = [
        ("Charge\ncard", "green"),
        ("Reserve\nstock", "green"),
        ("Notify\nrestaurant", "grey"),
        ("Dispatch\ndriver", "grey"),
    ]
    xs = [12, 30, 56, 76]
    for (txt, col), x in zip(steps, xs):
        box(ax, x, 60, 15, 11, txt, colour=col, size=8.5)

    arrow(ax, (19.5, 60), (22.5, 60), lw=1.1)
    arrow(ax, (37.5, 60), (41, 60), lw=1.1)
    arrow(ax, (48.5, 60), (63.5, 60), lw=1.1, dashed=True)

    # crash marker between step 2 and step 3
    ax.plot([44.5, 52.5], [66.5, 53.5], color=PALETTE["red"][1], lw=2.0, zorder=6)
    ax.plot([52.5, 44.5], [66.5, 53.5], color=PALETTE["red"][1], lw=2.0, zorder=6)
    label(ax, 48.5, 70.5, "node crashes here", size=8.5, bold=True,
          colour=PALETTE["red"][1])

    label(ax, 12, 50, "side effect is\nreal and permanent", size=7.5, colour=FAINT)
    label(ax, 30, 50, "side effect is\nreal and permanent", size=7.5, colour=FAINT)
    label(ax, 66, 50, "never happened", size=7.5, colour=FAINT)

    label(ax, 50, 41,
          "on restart, a system without a durable history has only two options",
          size=8.5, bold=True)

    box(ax, 26, 24, 40, 20,
        "OPTION 1  —  start over\n\nSteps 1 and 2 run a second time.\n"
        "The card is charged twice and the\nstock is double-reserved.",
        colour="red", size=8.2)
    box(ax, 74, 24, 40, 20,
        "OPTION 2  —  give up\n\nSteps 3 and 4 never run. Money is\n"
        "taken, stock is held, and no food\never arrives.",
        colour="amber", size=8.2)

    label(ax, 26, 9.5, "DUPLICATED WORK", size=9, bold=True, colour=PALETTE["red"][1])
    label(ax, 74, 9.5, "LOST WORK", size=9, bold=True, colour=PALETTE["amber"][1])
    label(ax, 50, 9.5, "both are\nincorrect", size=8.5, bold=True, colour=INK)

    save(fig, "fig_1_1_crash_dilemma.png")


# ---------------------------------------------------------------- Fig 1.2
def fig_dependency_count():
    """Infrastructure components each published orchestrator requires, against
    this engine's one. Counts come from each system's own documentation and
    are listed again in Table 2.1."""
    fig, ax = canvas(7.0, 4.0, ylim=(0, 100))

    systems = [
        ("Netflix Conductor", ["Database", "Elasticsearch", "Queue", "Server", "Workers"], "grey"),
        ("Uber Cadence", ["Cassandra", "Queue", "Timer svc", "Matching", "Workers"], "grey"),
        ("Apache Airflow", ["Postgres", "Redis / RabbitMQ", "Scheduler", "Workers"], "grey"),
        ("Meta Shard Manager", ["Datastore", "ZooKeeper", "Scheduler", "Workers"], "grey"),
        ("Temporal", ["Datastore", "History svc", "Matching svc", "Frontend"], "grey"),
        ("durexec (this work)", ["PostgreSQL"], "green"),
    ]

    y = 90
    for name, parts, col in systems:
        green = col == "green"
        label(ax, 1, y, name, size=8.6, ha="left", bold=green,
              colour=PALETTE["green"][1] if green else INK)
        x = 36
        for p in parts:
            w = 2.1 + 1.05 * len(p)
            box(ax, x + w / 2, y, w, 8.2, p, colour=col, size=7.4, radius=1.0)
            x += w + 1.6
        label(ax, 97, y, str(len(parts)), size=10, bold=True, ha="right",
              colour=PALETTE["green"][1] if green else WIRE)
        y -= 15

    ax.plot([34.5, 34.5], [4, 96], color=FAINT, lw=0.8, ls=(0, (3, 3)))
    label(ax, 97, 99.5, "parts", size=7.5, ha="right", colour=FAINT)
    save(fig, "fig_1_2_dependency_count.png")


# ---------------------------------------------------------------- Fig 2.1
def fig_related_work_map():
    """Where the published systems sit on the two axes this project cares
    about: whether execution is partitioned across machines, and whether a
    coordination service outside the data store is required."""
    fig, ax = canvas(6.6, 4.8)

    arrow(ax, (10, 12), (95, 12), lw=1.2)
    arrow(ax, (10, 12), (10, 93), lw=1.2)
    label(ax, 52, 5.0, "horizontal partitioning of execution  →", size=9, bold=True)
    label(ax, 4.0, 52, "external coordination service required  →",
          size=9, bold=True, rot=90)

    label(ax, 26, 8.5, "single node", size=7.8, colour=FAINT)
    label(ax, 78, 8.5, "sharded across nodes", size=7.8, colour=FAINT)
    label(ax, 7.0, 24, "none", size=7.8, colour=FAINT, rot=90)
    label(ax, 7.0, 76, "ZooKeeper / Raft", size=7.8, colour=FAINT, rot=90)

    ax.plot([10, 95], [52, 52], color=FAINT, lw=0.7, ls=(0, (3, 3)))
    ax.plot([52, 52], [12, 93], color=FAINT, lw=0.7, ls=(0, (3, 3)))

    pts = [
        (26, 31, "DBOS\n(VLDB 2022, 2025)", "blue", 20, 9),
        (25, 20, "AC/DC\n(CIDR 2026)", "blue", 15, 9),
        (27, 62, "Quartz\nclustering", "grey", 14, 9),
        (31, 77, "Airflow\n(Celery)", "grey", 13, 9),
        (74, 81, "Shard Manager\n(SOSP 2021)", "amber", 19, 9),
        (82, 67, "Cadence /\nTemporal", "amber", 15, 9),
        (84, 45, "Restate\n(replicated log)", "purple", 17, 9),
        (78, 23, "durexec\n(this work)", "green", 16, 10),
    ]
    for x, y, t, c, w, h in pts:
        box(ax, x, y, w, h, t, colour=c, size=7.6,
            bold=(c == "green"), lw=1.7 if c == "green" else 1.2)

    box(ax, 71, 8.2, 46, 6.4,
        "target quadrant: sharded, no coordination service",
        colour="green", size=7.2, radius=0.9, lw=1.0, dashed=True)
    save(fig, "fig_2_1_related_work_map.png")


# ---------------------------------------------------------------- Fig 2.2
def fig_semantic_gap():
    """The crash-induced, semantically duplicate side effect that exact-match
    idempotency provably cannot catch."""
    fig, ax = canvas(7.0, 4.6, ylim=(0, 82))

    label(ax, 50, 78, "first execution", size=9, bold=True, colour=WIRE)

    row = [
        (14, "Plan v1", "purple", 'decides\nbook_flight("BLR→DEL Fri morning")'),
        (42, "Tool call A", "green", "FLIGHT BOOKED\nresult written to history"),
        (72, "Plan v2", "purple", "LLM call starts …"),
    ]
    for x, t, c, sub in row:
        box(ax, x, 64, 21, 9, t, colour=c, size=8.6, bold=True)
        label(ax, x, 54.0, sub, size=7.3, colour=INK)

    arrow(ax, (24.5, 64), (31.5, 64))
    arrow(ax, (52.5, 64), (61.5, 64))

    ax.plot([86, 94], [69, 58], color=PALETTE["red"][1], lw=2.0, zorder=6)
    ax.plot([94, 86], [69, 58], color=PALETTE["red"][1], lw=2.0, zorder=6)
    label(ax, 90, 73.5, "CRASH", size=8.4, bold=True, colour=PALETTE["red"][1])
    label(ax, 90, 51.0, "before Plan v2\nis journalled", size=7.1,
          colour=PALETTE["red"][1])

    ax.plot([3, 97], [43, 43], color=FAINT, lw=0.9, ls=(0, (4, 3)))
    label(ax, 50, 39.0, "after recovery  —  replay, then forward", size=9,
          bold=True, colour=WIRE)

    row2 = [
        (14, "Plan v1", "grey", "resolved from history\n— not re-run"),
        (42, "Tool call A", "grey", "resolved from history\n— not re-run"),
        (72, "Plan v2", "purple", "re-runs at temperature > 0\n→ different wording"),
    ]
    for x, t, c, sub in row2:
        box(ax, x, 28, 21, 9, t, colour=c, size=8.6, bold=True)
        label(ax, x, 18.0, sub, size=7.3, colour=INK)
    arrow(ax, (24.5, 28), (31.5, 28))
    arrow(ax, (52.5, 28), (61.5, 28))

    box(ax, 50, 7.0, 94, 11,
        'issues  book_flight("Bangalore to Delhi, Friday AM")   —   '
        "same intent, different bytes\n"
        "exact-match hash differs from every recorded key   →   "
        "THE ACTION EXECUTES A SECOND TIME",
        colour="red", size=8.0)
    save(fig, "fig_2_2_semantic_gap.png")


# ---------------------------------------------------------------- Fig 3.1
def fig_testbench():
    """The container topology the requirements in this chapter are sized
    against, and the one every measurement in the report was taken on."""
    fig, ax = canvas(6.8, 4.9, ylim=(0, 96))

    band(ax, 50, 48, 98, 92,
         "single development host  —  Docker Compose project", colour="grey")

    band(ax, 31, 76, 58, 22, "engine tier  —  one Go binary per node", colour="blue")
    for i, (n, x) in enumerate([("node-a", 12), ("node-b", 31), ("node-c", 50)]):
        box(ax, x, 73, 16, 11, f"{n}\n:808{i + 1}", colour="blue", size=8.2, bold=True)

    band(ax, 82, 76, 30, 22, "observability", colour="purple")
    box(ax, 82, 78, 25, 7.5, "Prometheus  :9090", colour="purple", size=7.8)
    box(ax, 82, 69, 25, 7.5, "Grafana  :3000", colour="purple", size=7.8)

    band(ax, 31, 40, 58, 24,
         "storage tier  —  PostgreSQL 16 (pgvector)", colour="green")
    box(ax, 17, 37, 25, 12, "postgres-leader\n16 shards, 6 tables",
        colour="green", size=8.0, bold=True)
    box(ax, 46, 37, 25, 12, "postgres-standby\nstreaming replica",
        colour="green", size=8.0)
    arrow(ax, (29.5, 37), (33.5, 37), label="WAL", size=7.2, dy=3.0)

    band(ax, 82, 40, 30, 24, "test-only services", colour="amber")
    box(ax, 82, 42, 25, 7.5, "embedder (MiniLM)", colour="amber", size=7.8)
    box(ax, 82, 33, 25, 7.5, "extsim — effect ledger", colour="amber", size=7.8)

    arrow(ax, (12, 67.5), (14, 49), lw=1.0)
    arrow(ax, (31, 67.5), (28, 49), lw=1.0)
    arrow(ax, (50, 67.5), (43, 49), lw=1.0)
    arrow(ax, (59, 74), (69, 76), lw=1.0, dashed=True, label="scrape", size=7.0, dy=2.4)
    arrow(ax, (59, 70), (69, 42), lw=1.0, dashed=True)

    box(ax, 50, 11, 84, 12,
        "client / load generator  —  HTTP:  POST /v1/workflows,  "
        "POST /v1/workflows/{id}/signals\n"
        "fault injection:  make kill-owner,  FAULT_CRASH_AT=<crash point>",
        colour="white", size=7.8)
    arrow(ax, (50, 17), (50, 25), lw=1.1)
    save(fig, "fig_3_1_testbench.png")


if __name__ == "__main__":
    fig_crash_dilemma()
    fig_dependency_count()
    fig_related_work_map()
    fig_semantic_gap()
    fig_testbench()
