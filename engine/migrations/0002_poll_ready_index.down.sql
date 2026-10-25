-- Restore 0001's index before dropping the new one, so there is no instant at
-- which the poll has no index at all.
CREATE INDEX IF NOT EXISTS tasks_pollable ON tasks (shard_id, task_type, visible_at, task_id);
DROP INDEX IF EXISTS tasks_poll_ready;
