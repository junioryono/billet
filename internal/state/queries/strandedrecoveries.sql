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
-- The launches decided stranded since a moment whose run billet has never tried to
-- recover, oldest decision first and bounded. The WHERE leads with the partial
-- index's, word for word.
SELECT lease_id, tier, COALESCE(repo, '') AS repo, CAST(COALESCE(run_id, 0) AS BIGINT) AS run_id,
       job_name, COALESCE(finished_at, '') AS finished_at, stranded_at
  FROM job_history
 WHERE stranded = 'stranded'
   AND stranded_at >= @since
   AND NOT EXISTS (SELECT 1 FROM stranded_recoveries AS r
                    WHERE r.repo = COALESCE(job_history.repo, '') AND r.run_id = job_history.run_id)
 ORDER BY stranded_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListOpenStrandedRecoveries :many
-- Every recovery between its two requests, oldest first and bounded.
SELECT repo, run_id, run_attempt, tier, lease_id, job_name, state, tries, created_at
  FROM stranded_recoveries
 WHERE state IN ('cancelling', 'rerunning')
 ORDER BY created_at, repo, run_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: BeginStrandedRecovery :execrows
-- Write that billet is about to cancel one attempt of a run, BEFORE the cancel
-- leaves. A run billet has tried to recover once, on any attempt, is never tried
-- again, and the rows changed say whether this one began.
INSERT INTO stranded_recoveries (repo, run_id, run_attempt, tier, lease_id, job_name, state, tries, created_at)
SELECT @repo, @run_id, @run_attempt, @tier, @lease_id, @job_name, 'cancelling', 1, @created_at
 WHERE NOT EXISTS (SELECT 1 FROM stranded_recoveries AS r WHERE r.repo = @repo AND r.run_id = @run_id);

-- name: MoveStrandedRecovery :execrows
-- Move a recovery from one of its two requests to the other, or count one more try
-- of the same one, only from the state it was read in.
UPDATE stranded_recoveries SET state = @to_state, tries = @tries
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = @from_state;

-- name: SettleStrandedRecovery :execrows
-- End a recovery, only from the state it was read in: a settled recovery's end
-- stands.
UPDATE stranded_recoveries SET state = @to_state, settled_at = @settled_at, detail = @detail
 WHERE repo = @repo AND run_id = @run_id AND run_attempt = @run_attempt AND state = @from_state;
