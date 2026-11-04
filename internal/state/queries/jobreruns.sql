-- Re-running a job billet disrupted (#541): what the loop asks of the ledger, and
-- what it records on the original job. ADR-023 holds the rule these serve.

-- name: ListRerunCandidates :many
-- The jobs that ended red on a lease a cause from the closed list disrupted, that
-- GitHub saw start, and that no re-run decision has been recorded for yet. Newest
-- result first and bounded, so a backlog loses its oldest, which are also the
-- ones past their window.
--
-- `disruption != ''` IS WRITTEN OUT so the planner can use the partial index
-- migration 35 made for this table's rare rows: it does not derive that from the
-- list. The three causes are parameters so the one list is alloc's.
--
-- STARTED, because `launching` admits a disruption to a lease whose guest never
-- ran a job, and a job that never ran has nothing to re-run.
--
-- THE RUN IS THE ONE THAT STARTED, as ListJobsAwaitingQueueTime reads it.
SELECT lease_id, tier, COALESCE(repo, '') AS repo,
       CAST(CASE WHEN github_run_id > 0 THEN github_run_id ELSE COALESCE(run_id, 0) END
            AS BIGINT) AS run_id,
       disruption, result_at
  FROM job_history
 WHERE disruption != ''
   AND disruption IN (CAST(@cause_one AS TEXT), CAST(@cause_two AS TEXT), CAST(@cause_three AS TEXT))
   AND started_at IS NOT NULL
   AND result != '' AND result != @succeeded
   AND result_at >= @since
   AND rerun_decision = ''
   AND github_job_id != '' AND COALESCE(repo, '') != ''
   AND (github_run_id > 0 OR COALESCE(run_id, 0) > 0)
 ORDER BY result_at DESC, lease_id DESC
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ReadOpenRerun :one
-- The request of one run that may have landed, newest attempt first. sql.ErrNoRows
-- is the ordinary answer: nothing is in doubt.
SELECT run_attempt, kind, github_job_id, tries
  FROM job_reruns
 WHERE repo = @repo AND run_id = @run_id AND state = 'asking'
 ORDER BY run_attempt DESC
 LIMIT 1;

-- name: ReadRerunOfAttempt :one
-- How the request about one attempt of a run ended. sql.ErrNoRows is an attempt
-- not asked about; a job that finished late finds here what its attempt's request
-- already came to, since the key allows that attempt no second one.
SELECT state FROM job_reruns
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
