-- Recovering a job GitHub stopped offering (#911), for a tier that sets
-- retry.stranded: what the pass asks of the ledger. Migration 85 holds why a
-- recovery is a row, and the order of its two requests.

-- name: AskStrandedForTier :exec
-- Record when a tier started asking, and keep the first time.
INSERT INTO stranded_tiers (tier, asked_since) VALUES (@tier, @asked_since)
ON CONFLICT (tier) DO NOTHING;

-- name: ListStrandedTiers :many
-- Every tier that asks, and since when.
SELECT tier, asked_since FROM stranded_tiers ORDER BY tier;

-- name: ForgetStrandedTier :exec
-- A tier that stopped asking, so asking again starts from then.
DELETE FROM stranded_tiers WHERE tier = @tier;

-- name: ListStrandedToRecover :many
-- The launches decided stranded since a moment, of a tier that asks, whose lease
-- ended after the tier started asking and whose run billet has never tried to
-- recover: oldest decision first from just past a cursor, (after_decided,
-- after_lease), the last row of the page before, an empty after_decided the start,
-- and bounded. What it leaves out is ineligible for good, so a page is only of what a
-- pass may take; the cursor goes round it. The WHERE leads with the partial index's,
-- word for word.
SELECT h.lease_id, h.tier, COALESCE(h.repo, '') AS repo, CAST(COALESCE(h.run_id, 0) AS BIGINT) AS run_id,
       h.job_name, COALESCE(h.finished_at, '') AS finished_at, h.stranded_at
  FROM job_history AS h
  JOIN stranded_tiers AS t ON t.tier = h.tier
 WHERE h.stranded = 'stranded'
   AND h.stranded_at >= @since
   AND COALESCE(h.finished_at, '') >= t.asked_since
   AND (h.stranded_at > @after_decided OR (h.stranded_at = @after_decided AND h.lease_id > @after_lease))
   AND NOT EXISTS (SELECT 1 FROM stranded_recoveries AS r
                    WHERE r.repo = COALESCE(h.repo, '') AND r.run_id = h.run_id)
 ORDER BY h.stranded_at, h.lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListOpenStrandedRecoveries :many
-- The recoveries between their two requests, oldest first from just past a cursor,
-- (after_created, after_repo, after_run), the last row of the page before, an empty
-- after_created the start, and bounded: a pass reads a page and the cursor goes
-- round, so none waits behind a page of others for ever.
SELECT repo, run_id, run_attempt, tier, lease_id, job_name, ended_at, state, tries, refused, created_at
  FROM stranded_recoveries
 WHERE state IN ('cancelling', 'rerunning')
   AND (created_at > @after_created
        OR (created_at = @after_created
            AND (repo > @after_repo OR (repo = @after_repo AND run_id > @after_run))))
 ORDER BY created_at, repo, run_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: CountOpenStrandedRecoveriesOfRun :one
-- Whether a run has a recovery between its two requests: a re-run of one of its
-- disrupted jobs waits for it, since the recovery's re-run of the run's failed jobs
-- runs that job too, and one that took the next attempt first would leave the job
-- billet cancelled without its re-run.
SELECT CAST(COUNT(*) AS BIGINT) AS open
  FROM stranded_recoveries
 WHERE repo = @repo AND run_id = @run_id AND state IN ('cancelling', 'rerunning');

-- name: BeginStrandedRecovery :execrows
-- Write that billet is about to cancel one attempt of a run, BEFORE the cancel
-- leaves. A run billet has tried to recover once, on any attempt, is never tried
-- again, and the rows changed say whether this one began. The transaction that
-- writes it has asked CountLaterLeasesOfJob first.
INSERT INTO stranded_recoveries (repo, run_id, run_attempt, tier, lease_id, job_name, ended_at, state, tries,
                                 created_at)
SELECT @repo, @run_id, @run_attempt, @tier, @lease_id, @job_name, @ended_at, 'cancelling', 1, @created_at
 WHERE NOT EXISTS (SELECT 1 FROM stranded_recoveries AS r WHERE r.repo = @repo AND r.run_id = @run_id);

-- name: MoveStrandedRecovery :execrows
-- Move a recovery from one of its two requests to the other, or count one more try
-- of the same one, only from the state it was read in.
UPDATE stranded_recoveries SET state = @to_state, tries = @tries
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = @from_state;

-- name: RefuseStrandedCancel :execrows
-- Record that GitHub definitely refused the cancel most recently sent, the one tries
-- counts. refused is how far an unbroken run of refused sends reaches from the first,
-- so it moves only from the send before to this one: written twice it changes
-- nothing, and a refusal after a send that may have landed is not taken for one.
UPDATE stranded_recoveries SET refused = tries
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = 'cancelling'
   AND tries = @tries AND refused = tries - 1;

-- name: SettleStrandedRecovery :execrows
-- End a recovery, only from the state it was read in: a settled recovery's end
-- stands.
UPDATE stranded_recoveries SET state = @to_state, settled_at = @settled_at, detail = @detail
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = @from_state;

-- name: CountStrandedRecoveries :many
-- How many recoveries of each tier are in each state, for a metric: every one still
-- under way, however old, and every one that settled since a moment. A recovery
-- expires only a day after it began, so a window on its beginning would drop it as
-- it ends.
SELECT tier, state, COUNT(*) AS recoveries
  FROM stranded_recoveries
 WHERE state IN ('cancelling', 'rerunning') OR settled_at >= @since
 GROUP BY tier, state
 ORDER BY tier, state;
