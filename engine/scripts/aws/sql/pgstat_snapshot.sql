-- One point-in-time snapshot of PostgreSQL and database-HOST counters, as
-- key=value lines. Taken before and after a load window; the difference of
-- two snapshots is the per-run figure. Cumulative counters only, except the
-- lines prefixed g_ which are gauges at the instant of the snapshot.
--
-- The DB host runs no exporter, so host CPU, memory and disk counters are read
-- through pg_read_file. That needs superuser; the pilot's `durexec` role is
-- POSTGRES_USER of the official image and therefore is one. Inside a Docker
-- container /proc/stat, /proc/meminfo and /proc/diskstats are the HOST's
-- (they are not namespaced), which is what we want: the instance, not the
-- container. If any read is refused the line reads host_*=unavailable and the
-- run records it as missing rather than guessing.
\set QUIET on
\pset tuples_only on
\pset format unaligned

select 'ts_epoch_ms=' || (extract(epoch from clock_timestamp()) * 1000)::bigint;
select 'server_version=' || current_setting('server_version_num');

select 'xact_commit='   || xact_commit   || E'\n' ||
       'xact_rollback=' || xact_rollback || E'\n' ||
       'blks_read='     || blks_read     || E'\n' ||
       'blks_hit='      || blks_hit      || E'\n' ||
       'tup_inserted='  || tup_inserted  || E'\n' ||
       'tup_updated='   || tup_updated   || E'\n' ||
       'tup_deleted='   || tup_deleted   || E'\n' ||
       'conflicts='     || conflicts     || E'\n' ||
       'temp_bytes='    || temp_bytes    || E'\n' ||
       'deadlocks='     || deadlocks     || E'\n' ||
       'blk_read_time_ms='  || round(blk_read_time::numeric, 3)  || E'\n' ||
       'blk_write_time_ms=' || round(blk_write_time::numeric, 3)
  from pg_stat_database where datname = current_database();

select 'wal_records=' || wal_records || E'\n' ||
       'wal_fpi='     || wal_fpi     || E'\n' ||
       'wal_bytes='   || wal_bytes   || E'\n' ||
       'wal_buffers_full=' || wal_buffers_full || E'\n' ||
       'wal_write='   || wal_write   || E'\n' ||
       'wal_sync='    || wal_sync
  from pg_stat_wal;

select 'ckpt_timed='     || checkpoints_timed || E'\n' ||
       'ckpt_req='       || checkpoints_req   || E'\n' ||
       'ckpt_write_ms='  || round(checkpoint_write_time::numeric, 0) || E'\n' ||
       'ckpt_sync_ms='   || round(checkpoint_sync_time::numeric, 0)  || E'\n' ||
       'buffers_checkpoint=' || buffers_checkpoint || E'\n' ||
       'buffers_backend='    || buffers_backend    || E'\n' ||
       'buffers_backend_fsync=' || buffers_backend_fsync
  from pg_stat_bgwriter;

select 'g_db_size_bytes=' || pg_database_size(current_database());
select 'g_conns_total='   || count(*) from pg_stat_activity where datname = current_database();
select 'g_conns_active='  || count(*) filter (where state = 'active') || E'\n' ||
       'g_conns_idle='    || count(*) filter (where state = 'idle')   || E'\n' ||
       'g_conns_idle_in_txn=' || count(*) filter (where state like 'idle in transaction%') || E'\n' ||
       'g_conns_waiting_lock=' || count(*) filter (where wait_event_type = 'Lock')
  from pg_stat_activity where datname = current_database() and pid <> pg_backend_pid();
select 'g_max_connections=' || current_setting('max_connections');
select 'g_locks_not_granted=' || count(*) from pg_locks where not granted;

-- Wait events of active backends at this instant. One line per (type,event);
-- sampled repeatedly during the window by the runner to build a profile.
select 'g_wait|' || coalesce(wait_event_type, 'CPU') || '|' || coalesce(wait_event, 'running') || '=' || count(*)
  from pg_stat_activity
 where datname = current_database() and state = 'active' and pid <> pg_backend_pid()
 group by wait_event_type, wait_event;

select exists(select 1 from pg_extension where extname = 'pg_stat_statements') as has_pss \gset
\if :has_pss
select 'stmt_calls='   || coalesce(sum(calls), 0) || E'\n' ||
       'stmt_exec_ms=' || round(coalesce(sum(total_exec_time), 0)::numeric, 3)
  from pg_stat_statements;
\else
select 'stmt_calls=unavailable' || E'\n' || 'stmt_exec_ms=unavailable';
\endif

select current_setting('is_superuser') = 'on' as su \gset
\if :su
select 'host_cpu|' || split_part(pg_read_file('/proc/stat'), E'\n', 1);
select 'host_mem|' || string_agg(l, ' ')
  from regexp_split_to_table(pg_read_file('/proc/meminfo'), E'\n') l
 where l ~ '^(MemTotal|MemAvailable|Dirty|Writeback):';
select 'host_disk|' || string_agg(regexp_replace(trim(l), '\s+', ' ', 'g'), ';')
  from regexp_split_to_table(pg_read_file('/proc/diskstats'), E'\n') l
 where l ~ '\s(nvme[0-9]+n[0-9]+|xvd[a-z]+|vd[a-z]+|sd[a-z]+)\s';
select 'host_loadavg|' || trim(pg_read_file('/proc/loadavg'));
\else
select 'host_cpu|unavailable';
\endif
