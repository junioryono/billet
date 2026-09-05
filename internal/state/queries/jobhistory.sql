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
-- THE HOST IS COALESCE(node, target_node), the way the rest of the arithmetic
-- reads a lease: `node` is filled at bind and escrow chose the machine long
-- before that, so a lease that never binds -- assigned by GitHub, then the
-- process dies -- recorded no host at all, and the jobs most worth investigating
-- are exactly the ones that end that way.
INSERT INTO job_history (lease_id, tier, node, run_id, request_id, queued_at, assigned_at)
VALUES (@lease_id, @tier, @node, @run_id, @request_id, @queued_at, @assigned_at)
ON CONFLICT (lease_id) DO UPDATE SET
  run_id = excluded.run_id, request_id = excluded.request_id,
  assigned_at = excluded.assigned_at;

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
INSERT INTO job_history
     (lease_id, tier, node, run_id, request_id, conclusion, failure_reason,
      disruption, disrupted_at, chosen_provider, instance_type, vcpu, memory, site,
      price_micros_per_hour, image_cache, cache_generation, actions_cache,
      queued_at, finished_at)
VALUES (@lease_id, @tier, @node, @run_id, @request_id, @conclusion, @failure_reason,
        @disruption, @disrupted_at, @chosen_provider, @instance_type, @vcpu, @memory,
        @site, @price_micros_per_hour, @image_cache, @cache_generation,
        @actions_cache, @queued_at, @finished_at)
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
                          THEN excluded.actions_cache ELSE job_history.actions_cache END;

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
       image_cache, cache_generation, actions_cache
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
       COALESCE(finished_at, '') AS finished_at
  FROM job_history
 ORDER BY queued_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);
