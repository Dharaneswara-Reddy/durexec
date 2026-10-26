-- One shared database for the whole cluster. Sharding is a shard_id column on
-- every table, not a database per shard: all three nodes connect to the same
-- PostgreSQL, and a node is confined to its shards by the lease it holds and by
-- the shard_id in every statement, not by which database it is pointed at.
--
-- Six tables; every mechanism in the engine is these tables plus the code that
-- manipulates them.

CREATE EXTENSION IF NOT EXISTS vector;

-- The append-only log. Never UPDATEd, never DELETEd. The primary key IS the
-- concurrency control: two workers appending the same seq, one loses.
CREATE TABLE IF NOT EXISTS events (
    shard_id    INT         NOT NULL,
    workflow_id TEXT        NOT NULL,
    run_id      UUID        NOT NULL,
    seq         BIGINT      NOT NULL,
    event_type  TEXT        NOT NULL,
    payload     JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shard_id, workflow_id, run_id, seq)
);

-- The mutable pointer. Keyed WITHOUT run_id so at most one run is open per
-- business key, which makes StartWorkflow idempotent at the front door.
CREATE TABLE IF NOT EXISTS executions (
    shard_id      INT    NOT NULL,
    workflow_id   TEXT   NOT NULL,
    run_id        UUID   NOT NULL,
    workflow_type TEXT   NOT NULL,
    status        TEXT   NOT NULL,
    last_seq      BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shard_id, workflow_id)
);
CREATE INDEX IF NOT EXISTS exec_running ON executions (shard_id, status) WHERE status = 'RUNNING';

-- A lease queue, not delete-on-read. A dead worker's lock expires and the task
-- becomes visible again; that is the whole crash-recovery story for in-flight work.
CREATE TABLE IF NOT EXISTS tasks (
    shard_id            INT         NOT NULL,
    task_id             BIGSERIAL   NOT NULL,
    task_type           TEXT        NOT NULL,
    workflow_id         TEXT        NOT NULL,
    run_id              UUID        NOT NULL,
    scheduled_event_seq BIGINT,
    visible_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until        TIMESTAMPTZ,
    lock_token          UUID,
    attempt             INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (shard_id, task_id)
);
-- The poll index lives in 0002 (tasks_poll_ready), not here.
--
-- This file used to create tasks_pollable (shard_id, task_type, visible_at,
-- task_id) and 0002 drops it. Because there is no schema_migrations table —
-- every *.up.sql is re-applied on every node boot — that pair made each boot
-- BUILD a large index on tasks under ACCESS EXCLUSIVE and then drop it again,
-- blocking every poll for the length of the build. It also deadlocked against
-- concurrent pollers when a second migrator ran (observed as
-- "apply 0002_poll_ready_index.up.sql: deadlock detected" with test packages
-- in parallel). Removing the create makes both files genuine no-ops after the
-- first pass; 0002's DROP INDEX IF EXISTS still removes the index from any
-- database that was created before this line was deleted.

CREATE TABLE IF NOT EXISTS timers (
    shard_id    INT         NOT NULL,
    workflow_id TEXT        NOT NULL,
    run_id      UUID        NOT NULL,
    timer_seq   BIGINT      NOT NULL,
    fire_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (shard_id, workflow_id, run_id, timer_seq)
);
CREATE INDEX IF NOT EXISTS timers_due ON timers (shard_id, fire_at);

-- Four columns that replace ZooKeeper. Read FOR UPDATE inside every mutating
-- transaction, which is what makes a fenced-out node unable to write.
CREATE TABLE IF NOT EXISTS shard_leases (
    shard_id      INT         PRIMARY KEY,
    owner_node    TEXT,
    fencing_token BIGINT      NOT NULL DEFAULT 0,
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Meaning-vectors for the semantic check. Candidate sets are bounded by
-- workflow history length, not corpus size, so no ANN index is warranted.
CREATE TABLE IF NOT EXISTS activity_intents (
    shard_id      INT         NOT NULL,
    workflow_id   TEXT        NOT NULL,
    run_id        UUID        NOT NULL,
    scheduled_seq BIGINT      NOT NULL,
    activity_type TEXT        NOT NULL,
    intent_text   TEXT        NOT NULL,
    embedding     VECTOR(384) NOT NULL,
    PRIMARY KEY (shard_id, workflow_id, run_id, scheduled_seq)
);
CREATE INDEX IF NOT EXISTS intents_by_type
    ON activity_intents (shard_id, workflow_id, run_id, activity_type);
