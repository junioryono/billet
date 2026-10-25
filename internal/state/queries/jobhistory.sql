-- Job history: what GitHub concluded, kept apart from what billet's lifecycle
-- did.
--
-- result IS GITHUB'S OWN WORD AND conclusion IS THE LEASE'S TERMINAL PHASE, and
-- they answer different questions. A job GitHub reports as failed on a lease
-- billet tore down perfectly is `done` in conclusion, and always was -- so nothing
-- in the ledger could tell an operator their build failed until result existed.

-- name: ReadJobResult :one
-- What GitHub concluded about the job one lease ran.
--
-- AN EMPTY ANSWER IS ONE OF THREE THINGS and no caller may collapse them: the job
-- has not finished, this lease never ran one, or the row predates the column.
-- sql.ErrNoRows is a fourth and separate state -- no history row at all.
SELECT result FROM job_history WHERE lease_id = @lease_id;

-- name: RecordJobResult :exec
-- Store GitHub's conclusion, verbatim.
--
-- STORED VERBATIM, and the caller's trim is only ever a blankness TEST.
-- Normalising here would decide the report: `" succeeded "` trimmed to
-- `"succeeded"` vanishes from it, and the one thing this column must not do is
-- turn a value billet does not recognise into one it does. An unknown result
-- fails OPEN into the report, quoted, where a person can see the padding.
UPDATE job_history SET result = @result, result_at = @result_at WHERE lease_id = @lease_id;

-- name: RecordJobRun :exec
-- Fill in the workflow run when the ledger has none, and only then.
--
-- NEVER AN OVERWRITE. A pooled runner is launched before GitHub chooses its job,
-- so the escrow records run 0 and the lease's own launch request id; the run an
-- operator needs is the one on the COMPLETION. But a recorded run is the one this
-- lease was assigned, and replacing it would let a swapped pool member rewrite
-- another job's history -- which is why the filter is in the statement rather
-- than in a branch above it.
UPDATE job_history SET run_id = @run_id
 WHERE lease_id = @lease_id AND COALESCE(run_id, 0) = 0;

-- name: RecordJobIdentity :exec
-- Record which of GitHub's jobs this lease ran, filling only what is empty.
--
-- EACH COLUMN IS WRITE-ONCE, and only for the job already named. A pooled
-- runner learns its job on JobStarted and a direct assignment at assignment;
-- a completion then fills only what neither recorded. The job-id filter is in
-- the statement so a message about a different job can never splice its name
-- or repository onto the one this row recorded, which is also why the job id
-- itself needs no CASE: the filter admits only an empty one or the same.
--
-- GITHUB'S TIMES FOLLOW THE SAME RULE: each is written once, from the first
-- message about this job that carried it, and an empty one (a time the message
-- left out) never replaces anything. They are GitHub's clock, kept verbatim.
UPDATE job_history
   SET github_job_id = CAST(@github_job_id AS TEXT),
       repo          = CASE WHEN COALESCE(repo, '') = '' THEN CAST(@repo AS TEXT)
                            ELSE repo END,
       workflow_ref  = CASE WHEN workflow_ref = '' THEN CAST(@workflow_ref AS TEXT)
                            ELSE workflow_ref END,
       job_name      = CASE WHEN job_name = '' THEN CAST(@job_name AS TEXT)
                            ELSE job_name END,
       event         = CASE WHEN event = '' THEN CAST(@event AS TEXT) ELSE event END,
       github_queued_at = CASE WHEN github_queued_at = ''
                               THEN CAST(@github_queued_at AS TEXT) ELSE github_queued_at END,
       github_scale_set_assigned_at = CASE WHEN github_scale_set_assigned_at = ''
                               THEN CAST(@github_scale_set_assigned_at AS TEXT)
                               ELSE github_scale_set_assigned_at END,
       github_runner_assigned_at = CASE WHEN github_runner_assigned_at = ''
                               THEN CAST(@github_runner_assigned_at AS TEXT)
                               ELSE github_runner_assigned_at END,
       github_run_id = CASE WHEN github_run_id = 0 THEN CAST(@github_run_id AS BIGINT)
                            ELSE github_run_id END
 WHERE lease_id = @lease_id
   AND (github_job_id = '' OR github_job_id = CAST(@github_job_id AS TEXT));

-- name: ReadJobWait :one
-- The two ends of one job's queue wait, and its tier: GitHub's queue time and
-- billet's start, each empty while unrecorded.
SELECT tier, github_queued_at, COALESCE(started_at, '') AS started_at
  FROM job_history WHERE lease_id = @lease_id;

-- name: ListJobWaits :many
-- The jobs that started since a moment, with every time their wait is split
-- by, newest start first, bounded: a window past the bound loses its oldest
-- jobs, never the ones an operator is looking at now.
--
-- WINDOWED ON started_at, because a wait is known once the job starts: a job
-- still queued has no row here that says so, and the report counts those from
-- the listeners' own reports instead. Every empty time is "not recorded", which
-- the report keeps out of the parts it cannot compute rather than reading it
-- as zero. vcpu is the charged shape, written when the row opens.
SELECT lease_id, tier, vcpu, github_queued_at, github_scale_set_assigned_at,
       github_runner_assigned_at, bought_at, COALESCE(assigned_at, '') AS assigned_at,
       COALESCE(started_at, '') AS started_at
  FROM job_history
 WHERE started_at IS NOT NULL AND started_at >= @since
 ORDER BY started_at DESC, lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListJobsAwaitingQueueTime :many
-- The jobs that started since a moment and still have no GitHub queue time,
-- with what finds each in GitHub's REST record of its run: the repository, the
-- run and GitHub's job id (the lease names the runner). Newest start first,
-- bounded, so a backlog is worked from the jobs an operator is looking at now.
--
-- THE RUN IS THE ONE THAT STARTED: github_run_id, which the JobStarted wrote
-- (migration 62). A pooled runner's run_id is the run of the job it was
-- launched for, and GitHub may give the runner a job of another run, so
-- run_id is read only for a row with no github_run_id, which a row written
-- before the migration has, and a direct assignment's run_id names the job it
-- ran. A run that is wrong finds no job on the runner, never another job's
-- time, because the runner's name is unique.
--
-- PAGED FROM A CURSOR, (before_started, before_lease), the last row of the page
-- before; an empty before_started is the newest. A reader that filters what it
-- cannot use yet pages on rather than stopping at one page of it.
SELECT lease_id, tier, COALESCE(repo, '') AS repo,
       CAST(CASE WHEN github_run_id > 0 THEN github_run_id ELSE COALESCE(run_id, 0) END
            AS BIGINT) AS run_id,
       github_job_id, COALESCE(started_at, '') AS started_at, github_scale_set_assigned_at
  FROM job_history
 WHERE started_at IS NOT NULL AND started_at >= @since
   AND github_queued_at = '' AND github_job_id != '' AND COALESCE(repo, '') != ''
   AND (github_run_id > 0 OR COALESCE(run_id, 0) > 0)
   AND (CAST(@before_started AS TEXT) = ''
        OR started_at < CAST(@before_started AS TEXT)
        OR (started_at = CAST(@before_started AS TEXT) AND lease_id < CAST(@before_lease AS TEXT)))
 ORDER BY started_at DESC, lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListAttributedFailures :many
-- Jobs that did not succeed while billet's own infrastructure was disrupted.
--
-- TWO FACTS, NOT A VERDICT. Nothing here says the disruption caused the failure;
-- billet cannot tell a broken host from a broken build, and the report that
-- renders this says so.
--
-- WINDOWED ON result_at RATHER THAN finished_at, which is written when the LEASE
-- terminalizes and stays empty for as long as a destroy is retrying -- a job
-- whose teardown is wedged is exactly one worth reporting.
--
-- `result != @succeeded` RATHER THAN A LIST OF FAILURE WORDS. The vendored
-- scale-set client enumerates no results, so the only value this codebase has
-- ever seen confirmed is the one node teardown already keys on; everything else,
-- including a spelling GitHub adds tomorrow, is treated as not succeeded. An
-- unknown result is "could not tell", and collapsing that into "fine" would hide
-- exactly the job an operator came here looking for.
SELECT lease_id, tier, COALESCE(node, '') AS node, COALESCE(run_id, 0) AS run_id,
       result, failure_reason, disruption, disrupted_at, result_at
  FROM job_history
 WHERE disruption != '' AND result != '' AND result != @succeeded
   AND result_at >= @since
 ORDER BY result_at DESC, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: RecordJobStart :exec
-- Record that GitHub started a job on this lease, once.
--
-- THE LEDGER'S OWN EVIDENCE THAT A JOB RAN, written when the lease reaches
-- `busy`, and kept at its first value: the phase moves on to custody or
-- teardown and forgets it was ever busy, while a reason a node sends later
-- may claim the launch never ran. A row that never reached busy keeps NULL.
UPDATE job_history SET started_at = @started_at
 WHERE lease_id = @lease_id AND started_at IS NULL;

-- name: ReadJobStarted :one
-- Whether GitHub was ever seen to start a job on this lease.
--
-- EXISTS RATHER THAN A NULLABLE READ, so "no history row" and "a row that
-- never started" are the same answer -- neither is evidence a job ran.
SELECT EXISTS(
  SELECT 1 FROM job_history WHERE lease_id = @lease_id AND started_at IS NOT NULL);

-- name: BackfillFailureReason :exec
-- Explain a failure the reaper archived without a reason, on both rows.
--
-- ONLY WHERE NOTHING EXPLAINS IT YET. A launch that failed conclusively can
-- have its parked release lose the race to the reaper, which terminalizes an
-- escrow-only lease outright; the retry then arrives at a failed row and still
-- knows why. A reason already recorded is the earlier fact and stands.
UPDATE job_history SET failure_reason = @failure_reason
 WHERE lease_id = @id AND failure_reason = ''
   AND EXISTS (SELECT 1 FROM leases
                WHERE id = @id AND phase = 'failed' AND failure_reason = '');

-- name: RecordJobAssignment :exec
-- Open the history row when the job is assigned.
--
-- AT ASSIGNMENT RATHER THAN AT TERMINALIZATION, so the row carries a real
-- assignment time instead of one fabricated when the lease closes.
--
-- WITH WHEN THE LEASE WAS BOUGHT AND ITS CHARGED SHAPE, which a queue-wait
-- report splits a wait by and groups jobs under while the lease still runs;
-- the archive overwrites the shape with the same terminal facts. The purchase
-- time is the lease row's own, read here because that row is reaped and this
-- one is what an operator reads, and kept at its first value.
--
-- THE HOST IS COALESCE(node, target_node), the way the rest of the arithmetic
-- reads a lease: `node` is filled at bind and escrow chose the machine long
-- before that, so a lease that never binds -- assigned by GitHub, then the
-- process dies -- recorded no host at all, and the jobs most worth investigating
-- are exactly the ones that end that way.
INSERT INTO job_history (lease_id, tier, node, run_id, request_id, queued_at, assigned_at,
                         vcpu, memory, bought_at)
VALUES (@lease_id, @tier, @node, @run_id, @request_id, @queued_at, @assigned_at,
        @vcpu, @memory,
        COALESCE((SELECT created_at FROM leases WHERE id = @lease_id), ''))
ON CONFLICT (lease_id) DO UPDATE SET
  run_id = excluded.run_id, request_id = excluded.request_id,
  assigned_at = excluded.assigned_at,
  vcpu = excluded.vcpu, memory = excluded.memory,
  bought_at = CASE WHEN job_history.bought_at = '' THEN excluded.bought_at
                   ELSE job_history.bought_at END;

-- name: ArchiveJobHistory :exec
-- Close the history row when the lease terminalizes.
--
-- EVERY COALESCE HERE PRESERVES WHAT THE ASSIGNMENT ROW ALREADY KNEW. A lease
-- terminalizing may carry less than the row does -- a reap has no run id -- and
-- overwriting with a null would erase the identity of the job that ran.
--
-- THE DISRUPTION IS WRITE-ONCE-OR-KEEP for the same reason it is on the lease:
-- the FIRST observation is the one that can still have been causal, and a
-- disruption recorded during teardown must not replace the spot interruption that
-- caused the teardown.
--
-- THE PLACEMENT FACTS ARE THE LEASE'S TERMINAL ONES and overwrite: the backend it
-- ran on, the shape placement bought, the charged vCPU and memory, the site and
-- the price that shape was charged at were all decided on the lease row, which
-- is about to be reaped, and the history is where they survive. The price is
-- what the lease recorded when the shape was charged, never today's catalogue.
--
-- THE CACHE OBSERVATIONS ARE WRITE-ONCE-OR-KEEP like the disruption, and for the
-- same reason: they are written onto this row the moment the node observes them,
-- and an archive arriving from a caller that did not load them must not blank an
-- observation already here. The generation moves with the image-cache token, so
-- a report never shows a generation with nothing to attribute it to.
--
-- THE HOLDER IS THE LEASE'S LAST, the node process that held the compute when
-- the lease ended (migration 65), and an archive from a caller that did not
-- load it keeps what an earlier one wrote.
INSERT INTO job_history
     (lease_id, tier, node, run_id, request_id, conclusion, failure_reason,
      disruption, disrupted_at, chosen_provider, instance_type, vcpu, memory, site,
      price_micros_per_hour, image_cache, cache_generation, actions_cache,
      sticky_cache, git_cache, bazel_cache, go_cache, npm_cache, holder_incarnation, queued_at,
      finished_at)
VALUES (@lease_id, @tier, @node, @run_id, @request_id, @conclusion, @failure_reason,
        @disruption, @disrupted_at, @chosen_provider, @instance_type, @vcpu, @memory,
        @site, @price_micros_per_hour, @image_cache, @cache_generation,
        @actions_cache, @sticky_cache, @git_cache, @bazel_cache, @go_cache, @npm_cache,
        @holder_incarnation, @queued_at, @finished_at)
ON CONFLICT (lease_id) DO UPDATE SET
  conclusion     = excluded.conclusion,
  failure_reason = excluded.failure_reason,
  finished_at    = excluded.finished_at,
  node           = COALESCE(excluded.node, job_history.node),
  run_id         = COALESCE(excluded.run_id, job_history.run_id),
  request_id     = COALESCE(excluded.request_id, job_history.request_id),
  disruption     = CASE WHEN excluded.disruption != ''
                        THEN excluded.disruption ELSE job_history.disruption END,
  disrupted_at   = CASE WHEN excluded.disruption != ''
                        THEN excluded.disrupted_at ELSE job_history.disrupted_at END,
  chosen_provider       = excluded.chosen_provider,
  instance_type         = excluded.instance_type,
  vcpu                  = excluded.vcpu,
  memory                = excluded.memory,
  site                  = excluded.site,
  price_micros_per_hour = excluded.price_micros_per_hour,
  image_cache      = CASE WHEN excluded.image_cache != ''
                          THEN excluded.image_cache ELSE job_history.image_cache END,
  cache_generation = CASE WHEN excluded.image_cache != ''
                          THEN excluded.cache_generation ELSE job_history.cache_generation END,
  actions_cache    = CASE WHEN excluded.actions_cache != ''
                          THEN excluded.actions_cache ELSE job_history.actions_cache END,
  sticky_cache     = CASE WHEN job_history.sticky_cache = ''
                          THEN excluded.sticky_cache ELSE job_history.sticky_cache END,
  git_cache        = CASE WHEN job_history.git_cache = ''
                          THEN excluded.git_cache ELSE job_history.git_cache END,
  bazel_cache      = CASE WHEN job_history.bazel_cache = ''
                          THEN excluded.bazel_cache ELSE job_history.bazel_cache END,
  go_cache         = CASE WHEN job_history.go_cache = ''
                          THEN excluded.go_cache ELSE job_history.go_cache END,
  npm_cache        = CASE WHEN job_history.npm_cache = ''
                          THEN excluded.npm_cache ELSE job_history.npm_cache END,
  holder_incarnation = CASE WHEN excluded.holder_incarnation != ''
                            THEN excluded.holder_incarnation
                            ELSE job_history.holder_incarnation END;

-- name: RecordJobPhase :exec
-- Record when a lease first entered a phase, on the row that outlives it.
--
-- EACH COLUMN IS WRITE-ONCE, and the phase's own: a repeated move into a phase,
-- or a move into another, keeps the first time. The incarnation travels with
-- the launch, read from the lease, which Bind wrote as the process launching
-- it. A lease with no history row ran no job and updates nothing. Every
-- comparison reads the row as it was before this statement, so the launch's
-- incarnation is written exactly when its time is.
UPDATE job_history
   SET launched_at = CASE WHEN CAST(@phase AS TEXT) = 'launching' AND launched_at = ''
                          THEN CAST(@at AS TEXT) ELSE launched_at END,
       launch_incarnation = CASE WHEN CAST(@phase AS TEXT) = 'launching' AND launched_at = ''
                                 THEN CAST(@incarnation AS TEXT) ELSE launch_incarnation END,
       custody_at = CASE WHEN CAST(@phase AS TEXT) = 'custody' AND custody_at = ''
                         THEN CAST(@at AS TEXT) ELSE custody_at END,
       teardown_at = CASE WHEN CAST(@phase AS TEXT) = 'teardown' AND teardown_at = ''
                          THEN CAST(@at AS TEXT) ELSE teardown_at END,
       quarantined_at = CASE WHEN CAST(@phase AS TEXT) = 'quarantine' AND quarantined_at = ''
                             THEN CAST(@at AS TEXT) ELSE quarantined_at END
 WHERE lease_id = @lease_id;

-- name: RecordJobRegistration :exec
-- Record when the plane journaled the runner it minted for a lease, and
-- GitHub's id for it, once.
--
-- THE RUNNER'S ID IS KEPT IF ALREADY LEARNED: a start or a recovery may have
-- written it first, and it is written once.
UPDATE job_history
   SET registered_at = @registered_at,
       runner_id = CASE WHEN runner_id = 0 THEN CAST(@runner_id AS BIGINT) ELSE runner_id END
 WHERE lease_id = @lease_id AND registered_at = '';

-- name: RecordJobRunnerID :exec
-- Record GitHub's id for a lease's runner once a start or a recovery learns
-- it, for a runner journaled without one: one billet adopted rather than
-- minted. Written once; the registration time is left as it was.
UPDATE job_history SET runner_id = @runner_id
 WHERE lease_id = @lease_id AND runner_id = 0;

-- name: ReadJobPlacement :one
-- What one lease was charged for and what the cache did, from the row that
-- outlives the lease.
--
-- ZERO IS NOT A PRICE. A host-backed lease buys nothing, so its instance_type is
-- empty and its price is zero; a remote row written before the column existed
-- has a shape and a zero, and a reader renders that as unknown, never as $0.
-- The cache tokens are empty when nothing was observed, and a token this binary
-- does not recognise is a NEWER binary's observation, rendered verbatim rather
-- than dropped.
SELECT chosen_provider, instance_type, vcpu, memory, site, price_micros_per_hour,
       image_cache, cache_generation, actions_cache, sticky_cache, git_cache,
       bazel_cache, go_cache, npm_cache
  FROM job_history WHERE lease_id = @lease_id;

-- name: ListJobConclusionsForRequest :many
-- Every recorded outcome for one scheduler request, oldest first.
--
-- `conclusion IS NOT NULL` IS LOAD-BEARING, AND MORE SO THAN IT USED TO BE. A row
-- is inserted at ASSIGNMENT with no conclusion and filled in only when the lease
-- terminalizes, so a job in flight is not an outcome. The hand-written scan used
-- to fail outright on a NULL, which made the filter partly a convenience;
-- generated code reads it as a sql.NullString, so without this an unfinished
-- lease would arrive silently as an empty outcome beside the real ones.
SELECT conclusion FROM job_history
 WHERE request_id = @request_id AND conclusion IS NOT NULL
 ORDER BY finished_at;

-- name: ReadJobConclusion :one
-- What billet's own lifecycle concluded about one lease.
SELECT conclusion FROM job_history WHERE lease_id = @lease_id;

-- name: ReadJobNode :one
-- The host a lease's job was attributed to, which outlives the lease row.
--
-- A LEASE THE LEDGER HAS ENDED STILL BELONGS TO A HOST. The wire admits a
-- registration removal for an ended lease on the strength of this, because the
-- lease row's placement is gone by then and a node must not be able to name
-- another host's lease and withdraw its runner.
SELECT node FROM job_history WHERE lease_id = @lease_id;

-- name: ReadJobFailureReason :one
-- Why it concluded that.
SELECT failure_reason FROM job_history WHERE lease_id = @lease_id;

-- name: ListJobHistory :many
-- Every job this deployment recorded, oldest first, bounded.
--
-- FOR AN ISOLATED ACCEPTANCE RUN, whose whole ledger is minutes old and holds
-- exactly the jobs that run represents -- so "every row" is a bounded set rather
-- than an unbounded scan, and the bound is here anyway because nothing about the
-- table's size is this query's to assume.
--
-- BOTH VERDICTS, because they are different facts and an acceptance report needs
-- to be able to disagree. `result` is what GITHUB said about the job; `conclusion`
-- is what billet's own lifecycle concluded about the lease. A run whose job went
-- green while billet concluded `failed` is exactly the shape worth catching, and
-- reporting one of the two would hide it.
--
-- EXPLICIT COLUMNS, never a wildcard projection, so an ordinary ALTER TABLE ADD
-- COLUMN cannot change a byte of the generated code. (Spelling the wildcard out
-- in this comment is what TestNoQueryUsesAWildcardProjection catches -- it reads
-- the file rather than the parsed statement, which is the right direction for a
-- guard whose job is to be impossible to talk past.)
SELECT lease_id, tier, COALESCE(node, '') AS node, COALESCE(run_id, 0) AS run_id,
       COALESCE(request_id, 0) AS request_id, conclusion, failure_reason,
       result, disruption, queued_at, COALESCE(started_at, '') AS started_at,
       COALESCE(finished_at, '') AS finished_at, github_job_id,
       COALESCE(repo, '') AS repo, workflow_ref, job_name, event
  FROM job_history
 ORDER BY queued_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListCacheOutcomes :many
-- What each cache did for the jobs assigned since a moment, one row per job.
--
-- WINDOWED ON assigned_at, when the history row opens: the node reports the
-- build caches when a job's session ends, and a job whose lease is still being
-- torn down has already been served by its caches. Empty tokens are jobs whose
-- cache outcome was not observed, which a report counts as such.
SELECT tier, image_cache, actions_cache, sticky_cache, git_cache, bazel_cache, go_cache,
       npm_cache
  FROM job_history
 WHERE assigned_at >= @since
 ORDER BY assigned_at DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ReadJob :one
-- One job's whole history row, for `billet jobs show`.
--
-- sql.ErrNoRows means the lease was never assigned a job. Every empty string
-- here is "not recorded", which a reader renders as such rather than as a
-- value.
SELECT lease_id, tier, COALESCE(node, '') AS node, COALESCE(run_id, 0) AS run_id,
       COALESCE(request_id, 0) AS request_id, github_job_id,
       COALESCE(repo, '') AS repo, workflow_ref, job_name, event,
       COALESCE(conclusion, '') AS conclusion, failure_reason, result, disruption,
       disrupted_at, queued_at, COALESCE(assigned_at, '') AS assigned_at,
       COALESCE(started_at, '') AS started_at, COALESCE(finished_at, '') AS finished_at,
       chosen_provider, instance_type, vcpu, memory, site, github_queued_at,
       github_scale_set_assigned_at, github_runner_assigned_at, bought_at,
       detail_retired_at, github_run_id, image_cache, cache_generation, actions_cache,
       sticky_cache, git_cache, bazel_cache, go_cache, npm_cache, launched_at, registered_at,
       runner_id, custody_at, teardown_at, quarantined_at, launch_incarnation,
       holder_incarnation
  FROM job_history WHERE lease_id = @lease_id;

-- name: ListJobsOfRun :many
-- The leases whose jobs belong to one of GitHub's workflow runs, oldest
-- assignment first, bounded.
--
-- THE RUN IS THE ONE THAT STARTED, as ListJobsAwaitingQueueTime reads it:
-- github_run_id, which the JobStarted wrote, and run_id only for a row with
-- none. A pooled runner launched for one run's job may be given another run's,
-- and is then that other run's; a lease that never started is the run it was
-- assigned. The two arms are separate so each is served by its own index.
SELECT lease_id FROM job_history
 WHERE github_run_id = @run_id OR (github_run_id = 0 AND run_id = @run_id)
 ORDER BY COALESCE(assigned_at, queued_at), lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListJobRuntimes :many
-- The jobs that started in [since, until) and finished before until, with the
-- names their runtime is learned under, newest start first, bounded: a window
-- past the bound loses its oldest jobs, never the newest.
--
-- FINISHED BEFORE until, because a job still running at until was not known to
-- have finished then: a prediction made at until could not have used it, and a
-- backtest that did would be graded against its own answer. The runtime is
-- finished_at less started_at, both billet's clock: the archive is when the
-- lease's room comes back, which is what a host freeing up means.
SELECT lease_id, tier, COALESCE(repo, '') AS repo, workflow_ref, job_name,
       COALESCE(started_at, '') AS started_at, COALESCE(finished_at, '') AS finished_at
  FROM job_history
 WHERE started_at IS NOT NULL AND started_at >= @since AND started_at < @until
   AND finished_at IS NOT NULL AND finished_at < @until
 ORDER BY started_at DESC, lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);
