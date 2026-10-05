-- The engine's central safety claim, in one query: two workers can never
-- successfully append the same (shard_id, workflow_id, run_id, seq) tuple to
-- the event log. The primary key on events already enforces this inside a
-- single Postgres instance, so under normal operation this query returns
-- zero rows. That is exactly why it belongs in CI rather than only in the
-- schema: a passing run here is live proof the constraint was never bypassed
-- (no COPY around it, no second writer path, no replica promoted and written
-- to directly) during a run that deliberately killed a node mid-flight.
--
-- Any row this returns is a violation of the invariant, by construction: it
-- names a tuple that appears more than once, which the primary key says
-- cannot happen. deploy/assert_no_duplicates.sh treats output as failure.
SELECT
    shard_id,
    workflow_id,
    run_id,
    seq,
    count(*)              AS occurrences,
    array_agg(event_type) AS event_types,
    min(created_at)        AS first_seen,
    max(created_at)        AS last_seen
FROM events
GROUP BY shard_id, workflow_id, run_id, seq
HAVING count(*) > 1
ORDER BY shard_id, workflow_id, run_id, seq;
