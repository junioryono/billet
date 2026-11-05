-- Re-running a job billet disrupted (#541): what the loop asks of the ledger, and
-- what it records on the original job. ADR-023 holds the rule these serve.

-- name: ListRerunCandidates :many
-- The jobs that ended red on a lease a cause from the closed list disrupted, that
-- GitHub saw start, and that no re-run decision has been recorded for yet. Newest
-- result first and paged from a cursor, (before_result, before_lease), the last
-- row of the page before, an empty before_result being the newest: the rows of a
-- tier that does not ask are never decided, so a page they fill is paged past.
--
-- `disruption != ''` IS WRITTEN OUT so the planner can use the partial index
-- migration 35 made for this table's rare rows: it does not derive that from the
-- list. The causes are parameters so the one list is alloc's.
--
-- STARTED, because `launching` admits a disruption to a lease whose guest never
-- ran a job, and a job that never ran has nothing to re-run.
--
-- THE RUN IS THE ONE THAT STARTED, as ListJobsAwaitingQueueTime reads it.
SELECT lease_id, tier, COALESCE(repo, '') AS repo,
       CAST(CASE WHEN github_run_id > 0 THEN github_run_id ELSE COALESCE(run_id, 0) END
            AS BIGINT) AS run_id,
       disruption, result_at, rerun_attempt
  FROM job_history
 WHERE disruption != ''
   AND disruption IN (CAST(@cause_one AS TEXT), CAST(@cause_two AS TEXT))
   AND started_at IS NOT NULL
   AND result != '' AND result != @succeeded
   AND result_at >= @since
   AND rerun_decision = ''
   AND github_job_id != '' AND COALESCE(repo, '') != ''
   AND (github_run_id > 0 OR COALESCE(run_id, 0) > 0)
   AND (CAST(@before_result AS TEXT) = ''
        OR result_at < CAST(@before_result AS TEXT)
        OR (result_at = CAST(@before_result AS TEXT) AND lease_id < CAST(@before_lease AS TEXT)))
 ORDER BY result_at DESC, lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListOpenReruns :many
-- The requests that may have landed, whatever became of the jobs that made them,
-- oldest first and paged from a cursor, (after_created, after_repo, after_run), the
-- last row of the page before, an empty after_created being the oldest. With one
-- tier of the jobs each still covers, the first by name, which says an App that can
-- read its run; empty when it covers none undecided.
SELECT repo, run_id, run_attempt, kind, github_job_id, tries, created_at,
       CAST(COALESCE((SELECT MIN(h.tier) FROM job_history h
                       WHERE COALESCE(h.repo, '') = job_reruns.repo AND h.rerun_attempt = job_reruns.run_attempt
                         AND h.rerun_decision = ''
                         AND (h.github_run_id = job_reruns.run_id
                              OR (h.github_run_id = 0 AND h.run_id = job_reruns.run_id))), '') AS TEXT) AS tier
  FROM job_reruns
 WHERE state = 'asking'
   AND (CAST(@after_created AS TEXT) = ''
        OR created_at > CAST(@after_created AS TEXT)
        OR (created_at = CAST(@after_created AS TEXT)
            AND (repo > CAST(@after_repo AS TEXT)
                 OR (repo = CAST(@after_repo AS TEXT) AND run_id > CAST(@after_run AS BIGINT)))))
 ORDER BY created_at, repo, run_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListBoundRerunTiers :many
-- The tiers of the jobs one request covers and that it has not yet decided: which
-- Apps can read its run, and whose limits it must still fit.
SELECT DISTINCT tier FROM job_history
 WHERE COALESCE(repo, '') = CAST(@repo AS TEXT) AND rerun_attempt = @run_attempt AND rerun_decision = ''
   AND (github_run_id = @run_id OR (github_run_id = 0 AND run_id = @run_id))
 ORDER BY tier;

-- name: ListRerunCandidatesOfRun :many
-- ListRerunCandidates for one run, which a run is decided from: the pages that
-- brought the run up may not have held all of it. Paged from the same cursor, so a
-- reader can read it to its end rather than decide from a prefix.
SELECT lease_id, tier, COALESCE(repo, '') AS repo,
       CAST(CASE WHEN github_run_id > 0 THEN github_run_id ELSE COALESCE(run_id, 0) END
            AS BIGINT) AS run_id,
       disruption, result_at, rerun_attempt
  FROM job_history
 WHERE (github_run_id = @run_id OR (github_run_id = 0 AND run_id = @run_id))
   AND COALESCE(repo, '') = CAST(@repo AS TEXT)
   AND disruption != ''
   AND disruption IN (CAST(@cause_one AS TEXT), CAST(@cause_two AS TEXT))
   AND started_at IS NOT NULL
   AND result != '' AND result != @succeeded
   AND rerun_decision = ''
   AND github_job_id != ''
   AND (CAST(@before_result AS TEXT) = ''
        OR result_at < CAST(@before_result AS TEXT)
        OR (result_at = CAST(@before_result AS TEXT) AND lease_id < CAST(@before_lease AS TEXT)))
 ORDER BY result_at DESC, lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: AskRerunsForTier :exec
-- Record when a tier started asking, and keep the first time.
INSERT INTO rerun_tiers (tier, asked_since) VALUES (@tier, @asked_since)
ON CONFLICT (tier) DO NOTHING;

-- name: ListRerunTiers :many
-- Every tier that asks, and since when.
SELECT tier, asked_since FROM rerun_tiers ORDER BY tier;

-- name: ForgetRerunTier :exec
-- A tier that stopped asking, so asking again starts from then.
DELETE FROM rerun_tiers WHERE tier = @tier;

-- name: ReadOpenRerun :one
-- The request of one run that may have landed, newest attempt first. sql.ErrNoRows
-- is the ordinary answer: nothing is in doubt.
SELECT run_attempt, kind, github_job_id, tries, created_at
  FROM job_reruns
 WHERE repo = @repo AND run_id = @run_id AND state = 'asking'
 ORDER BY run_attempt DESC
 LIMIT 1;

-- name: ReadRerunOfAttempt :one
-- How the request about one attempt of a run ended, and what it asked for.
-- sql.ErrNoRows is an attempt not asked about; a job that finished late finds here
-- what its attempt's request came to, since the key allows that attempt no second.
SELECT state, kind FROM job_reruns
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt;

-- name: CountRunReruns :one
-- How many re-runs of one run GitHub accepted from billet, across every attempt.
-- What retry.disrupted limits. A request that failed is not one: it ran nothing.
SELECT COUNT(*) FROM job_reruns
 WHERE repo = @repo AND run_id = @run_id AND state = 'requested';

-- name: InsertRerunIntent :exec
-- Write that one attempt of a run is about to be asked about. The key refuses a
-- second request for the attempt, whatever state the first reached.
INSERT INTO job_reruns (repo, run_id, run_attempt, kind, github_job_id, state, tries, created_at)
VALUES (@repo, @run_id, @run_attempt, @kind, @github_job_id, 'asking', 1, @created_at);

-- name: AddRerunTry :exec
-- Count one more try of a request still in doubt.
UPDATE job_reruns SET tries = tries + 1
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = 'asking';

-- name: SettleRerun :exec
-- End a request. Only one in doubt: a settled request's answer stands.
UPDATE job_reruns SET state = @state, settled_at = @settled_at, detail = @detail
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = 'asking';

-- name: BindJobToRerun :exec
-- Name the attempt a job's request is about, before the request leaves, so a
-- settlement finds it after a restart that forgot which jobs were asked about.
UPDATE job_history SET rerun_attempt = @run_attempt
 WHERE lease_id = @lease_id AND rerun_decision = '';

-- name: SettleJobsOfRerun :exec
-- Record a request's outcome on every job bound to it, once.
--
-- THE RUN IS MATCHED THE WAY ListJobsOfRun READS ONE, so the two arms are each
-- served by their own index, and a row already decided keeps what it says.
UPDATE job_history SET rerun_decision = @decision, rerun_decided_at = @decided_at
 WHERE rerun_decision = '' AND rerun_attempt = @run_attempt
   AND COALESCE(repo, '') = CAST(@repo AS TEXT)
   AND (github_run_id = @run_id OR (github_run_id = 0 AND run_id = @run_id));

-- name: RecordJobRerunDecision :exec
-- Record why a job was not asked about, once. A decision is the first one made:
-- what changes later, such as a person re-running the run, is GitHub's to say.
UPDATE job_history
   SET rerun_decision = @decision, rerun_decided_at = @decided_at, rerun_attempt = @run_attempt
 WHERE lease_id = @lease_id AND rerun_decision = '';

-- name: ListRerunDecisions :many
-- How many jobs of each tier reached each decision since a moment, for a metric.
SELECT tier, rerun_decision, COUNT(*) AS jobs
  FROM job_history
 WHERE rerun_decision != '' AND rerun_decided_at >= @since
 GROUP BY tier, rerun_decision
 ORDER BY tier, rerun_decision;
