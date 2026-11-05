-- What `billet jobs stats` and `billet jobs rightsize` read back: every job that
-- started in a window, with what the host measured it do.

-- name: ListJobFacts :many
-- The jobs that started in [@since, @until), newest start first, bounded: a
-- window past the bound loses its oldest jobs, and the caller says it did.
--
-- WINDOWED ON started_at, the moment GitHub gave the job to a runner, which every
-- job in a report therefore has: a job GitHub never started has no duration, no
-- usage worth comparing and no result of its own to rate. Served by
-- job_history_started_idx, with the usage row found by its primary key.
--
-- THE RUN IS THE ONE THAT STARTED, as ListJobsOfRun reads it: a pooled runner
-- launched for one run's job may be given another run's.
--
-- has_usage SAYS WHETHER A REPORT EXISTS, because every usage column below is
-- zero both for a report that measured zero and for no report at all; a reader
-- that took the zeros would call an unmeasured job idle.
SELECT h.lease_id, h.tier, COALESCE(h.node, '') AS node, COALESCE(h.repo, '') AS repo,
       h.workflow_ref, h.job_name,
       CAST(CASE WHEN h.github_run_id > 0 THEN h.github_run_id ELSE COALESCE(h.run_id, 0) END
            AS BIGINT) AS run_id,
       h.github_run_attempt, h.result, COALESCE(h.conclusion, '') AS conclusion, h.disruption,
       COALESCE(h.started_at, '') AS started_at, h.result_at, h.github_queued_at,
       h.vcpu, h.memory, h.price_micros_per_hour, h.instance_type,
       CAST(CASE WHEN u.lease_id IS NULL THEN 0 ELSE 1 END AS BIGINT) AS has_usage,
       COALESCE(u.unmeasured, '') AS unmeasured,
       CAST(COALESCE(u.window_ms, 0) AS BIGINT) AS window_ms,
       CAST(COALESCE(u.cpu_user_us, 0) AS BIGINT) AS cpu_user_us,
       CAST(COALESCE(u.cpu_system_us, 0) AS BIGINT) AS cpu_system_us,
       CAST(COALESCE(u.guest_cpu_us, 0) AS BIGINT) AS guest_cpu_us,
       CAST(COALESCE(u.memory_peak_bytes, 0) AS BIGINT) AS memory_peak_bytes,
       CAST(COALESCE(u.oom_kills, 0) AS BIGINT) AS oom_kills,
       CAST(COALESCE(u.cpu_some_us, 0) AS BIGINT) AS cpu_some_us,
       CAST(COALESCE(u.energy_active_uj, 0) AS BIGINT) AS energy_active_uj,
       CAST(COALESCE(u.energy_idle_uj, 0) AS BIGINT) AS energy_idle_uj,
       COALESCE(u.energy_source, '') AS energy_source
  FROM job_history h
  LEFT JOIN job_usage u ON u.lease_id = h.lease_id
 WHERE h.started_at IS NOT NULL
   AND h.started_at >= CAST(@since AS TEXT)
   AND h.started_at < CAST(@until AS TEXT)
 ORDER BY h.started_at DESC, h.lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);
