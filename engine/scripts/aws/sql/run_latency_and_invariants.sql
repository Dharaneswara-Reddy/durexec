-- Per-run latency DECOMPOSITION and correctness invariants for one load
-- point, computed from the event journal itself. Set :prefix to the workflow
-- id prefix loadgen used for the run (e.g. \set prefix 'loadgen-1793343957-s16-').
--
-- Why from the journal: the engine exports no queue-wait, scheduling or timer
-- latency, only end-to-end time as seen by the client. Every event row carries
-- created_at (the committing transaction's start time), so each phase of
-- every workflow can be measured after the fact, with no new instrumentation
-- and no clock other than the database's.
--
-- Phases, per workflow:
--   start_to_first_decision  WorkflowExecutionStarted -> first command event:
--                            workflow-task queue wait + first decision
--   activity_sched_to_done   ActivityTaskScheduled(k) -> ActivityTaskCompleted(k):
--                            activity-task queue wait + execution + journal
--   decision_turnaround      a completion/fired event -> the next event the
--                            workflow writes: workflow-task wake-up + replay + commit
--   timer_lag                TimerFired.created_at - TimerStarted.payload.fire_at:
--                            how late a durable timer fires
--   end_to_end               first event -> last event, completed runs only
\set QUIET on
\pset tuples_only on
\pset format unaligned

create temp table r as
select e.shard_id, e.workflow_id, e.run_id, e.seq, e.event_type, e.payload, e.created_at
  from events e
 where e.workflow_id like :'prefix' || '%';

create temp table phase(kind text, ms double precision);

insert into phase
select 'end_to_end', extract(epoch from max(created_at) - min(created_at)) * 1000
  from r group by run_id
having bool_or(event_type = 'WorkflowExecutionCompleted');

insert into phase
select 'start_to_first_decision', extract(epoch from b.created_at - a.created_at) * 1000
  from r a join r b on b.run_id = a.run_id and b.seq = a.seq + 1
 where a.event_type = 'WorkflowExecutionStarted';

insert into phase
select 'activity_sched_to_done', extract(epoch from c.created_at - s.created_at) * 1000
  from r s join r c on c.run_id = s.run_id and c.event_type = 'ActivityTaskCompleted'
                   and c.payload->>'k' = s.payload->>'k'
 where s.event_type = 'ActivityTaskScheduled';

insert into phase
select 'decision_turnaround', extract(epoch from b.created_at - a.created_at) * 1000
  from r a join r b on b.run_id = a.run_id and b.seq = a.seq + 1
 where a.event_type in ('ActivityTaskCompleted', 'TimerFired');

insert into phase
select 'timer_lag', extract(epoch from f.created_at - (t.payload->>'fire_at')::timestamptz) * 1000
  from r t join r f on f.run_id = t.run_id and f.event_type = 'TimerFired'
                   and f.payload->>'k' = t.payload->>'k'
 where t.event_type = 'TimerStarted';

select 'lat|' || kind
    || '|n=' || count(*)
    || '|p50=' || round(percentile_cont(0.50) within group (order by ms)::numeric, 1)
    || '|p90=' || round(percentile_cont(0.90) within group (order by ms)::numeric, 1)
    || '|p95=' || round(percentile_cont(0.95) within group (order by ms)::numeric, 1)
    || '|p99=' || round(percentile_cont(0.99) within group (order by ms)::numeric, 1)
    || '|max=' || round(max(ms)::numeric, 1)
  from phase group by kind order by kind;

-- Invariants. Each must be 0 for the run to be counted correct; any nonzero
-- value is reported, never suppressed.
select 'inv|runs=' || count(distinct run_id) from r;
select 'inv|dup_run_seq=' || count(*) from (
  select run_id, seq from r group by run_id, seq having count(*) > 1) d;
-- The same activity (run, k) completed more than once in the journal.
select 'inv|dup_activity_completion=' || count(*) from (
  select run_id, payload->>'k' from r where event_type = 'ActivityTaskCompleted'
   group by 1, 2 having count(*) > 1) d;
-- A completion with no matching schedule, or a run whose seq has a gap.
select 'inv|orphan_completion=' || count(*)
  from r c where c.event_type = 'ActivityTaskCompleted'
   and not exists (select 1 from r s where s.run_id = c.run_id
                    and s.event_type = 'ActivityTaskScheduled' and s.payload->>'k' = c.payload->>'k');
select 'inv|seq_gaps=' || count(*) from (
  select run_id from r group by run_id having max(seq) <> count(*)) d;
-- More than one terminal event in a run.
select 'inv|multi_terminal=' || count(*) from (
  select run_id from r where event_type in ('WorkflowExecutionCompleted', 'WorkflowExecutionFailed')
   group by run_id having count(*) > 1) d;
select 'inv|executions_by_status=' || coalesce(string_agg(status || ':' || n, ';' order by status), 'none') from (
  select status, count(*) n from executions where workflow_id like :'prefix' || '%' group by status) s;
select 'inv|pending_tasks=' || count(*) from tasks where workflow_id like :'prefix' || '%';
