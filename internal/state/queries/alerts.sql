-- Alerts (#537): which jobs `server.alerts` looks at, what it has decided, and what
-- it owes a destination. Migration 76 holds why each table is shaped as it is.

-- name: ListRunningJobsForAlerts :many
-- The jobs with no result yet that started since a moment, oldest first and bounded,
-- with what a rule is judged against. The run is the one that started, as
-- ListJobsAwaitingQueueTime reads it. finished_at says the lease is archived though
-- GitHub has said nothing: a lease a disruption archived is here, so the disruption
-- is told at once, and its finish says it is not running. One archived longer ago than
-- archived_since is left out: GitHub concludes a lost runner's job within minutes, and
-- one it never concluded must not hold the read. They are as many as the fleet's
-- runners and the leases waiting for GitHub's word.
SELECT lease_id, tier, COALESCE(node, '') AS node, COALESCE(repo, '') AS repo, workflow_ref, job_name,
       CAST(CASE WHEN github_run_id > 0 THEN github_run_id ELSE COALESCE(run_id, 0) END
            AS BIGINT) AS run_id,
       result, result_at, COALESCE(started_at, '') AS started_at, github_queued_at, disruption, rerun_decision,
       head_branch, COALESCE(finished_at, '') AS finished_at
  FROM job_history
 WHERE result = '' AND started_at IS NOT NULL AND started_at >= @since
   AND (COALESCE(finished_at, '') = '' OR finished_at >= @archived_since)
 ORDER BY started_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ListEndedJobsForAlerts :many
-- The jobs whose result arrived after a cursor and since a moment, oldest first and
-- bounded, so a pass reads on from where the last one decided everything and a
-- backlog is reached in order rather than hidden behind newer results. Only a job
-- that started since started_since, the window a running job is watched for, so a
-- job is judged in one window and a judgement outlives every window it is read in. A
-- job GitHub cancelled before it started is not here: an assignment nobody ran is
-- cancelled and reassigned by GitHub itself (assignment-protocol.md), and that is
-- nobody's failure.
SELECT lease_id, tier, COALESCE(node, '') AS node, COALESCE(repo, '') AS repo, workflow_ref, job_name,
       CAST(CASE WHEN github_run_id > 0 THEN github_run_id ELSE COALESCE(run_id, 0) END
            AS BIGINT) AS run_id,
       result, result_at, COALESCE(started_at, '') AS started_at, github_queued_at, disruption, rerun_decision,
       head_branch
  FROM job_history
 WHERE result != '' AND result_at >= @since AND started_at IS NOT NULL AND started_at >= @started_since
   AND (result_at > @after_result OR (result_at = @after_result AND lease_id > @after_lease))
 ORDER BY result_at, lease_id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: InsertAlertRule :exec
-- Record when this deployment first saw a rule, and only then.
INSERT INTO alert_rules (name, first_seen_at) VALUES (@name, @first_seen_at)
ON CONFLICT (name) DO NOTHING;

-- name: ListAlertRules :many
-- Every rule this deployment has seen, with its mute.
SELECT name, first_seen_at, muted_until, muted_by FROM alert_rules ORDER BY name;

-- name: ReadAlertRuleMute :one
-- Until when one rule is muted, read inside the decision that honours it.
SELECT muted_until FROM alert_rules WHERE name = @name;

-- name: MuteAlertRule :exec
-- Mute a rule until a moment, whether or not a controller has seen it yet: the
-- operator names a rule from the config, and a mute must not depend on which came
-- first.
INSERT INTO alert_rules (name, first_seen_at, muted_until, muted_by)
VALUES (@name, @first_seen_at, @muted_until, @muted_by)
ON CONFLICT (name) DO UPDATE SET muted_until = excluded.muted_until, muted_by = excluded.muted_by;

-- name: UnmuteAlertRule :execrows
-- Lift a rule's mute; the count says whether there was one.
UPDATE alert_rules SET muted_until = '', muted_by = '' WHERE name = @name AND muted_until != '';

-- name: ListAlertMarks :many
-- What has been judged since a moment, read once when the loop starts so a restart
-- does not spend a transaction on each job it already judged.
SELECT rule_name, lease_id, alert_condition FROM alert_marks WHERE decided_at >= @since;

-- name: InsertAlertMark :execrows
-- Say a rule has judged a job on a condition. No row means it already had, and the
-- transaction then changes nothing else.
INSERT INTO alert_marks (rule_name, lease_id, alert_condition, outcome, decided_at)
VALUES (@rule_name, @lease_id, @alert_condition, @outcome, @decided_at)
ON CONFLICT (rule_name, lease_id, alert_condition) DO NOTHING;

-- name: ReadAlertStreak :one
-- One job's streak under one rule, and when its cooldown ends. sql.ErrNoRows is a
-- job the rule has not counted.
SELECT streak, cooldown_until FROM alert_streaks WHERE rule_name = @rule_name AND job_key = @job_key;

-- name: WriteAlertStreak :exec
-- Set one job's streak and, when it fired, its firing and cooldown.
INSERT INTO alert_streaks (rule_name, job_key, streak, last_fired_at, cooldown_until, updated_at)
VALUES (@rule_name, @job_key, @streak, @fired_at, @cooldown_until, @updated_at)
ON CONFLICT (rule_name, job_key) DO UPDATE SET
  streak = excluded.streak,
  last_fired_at = CASE WHEN excluded.last_fired_at != '' THEN excluded.last_fired_at
                       ELSE alert_streaks.last_fired_at END,
  cooldown_until = CASE WHEN excluded.last_fired_at != '' THEN excluded.cooldown_until
                        ELSE alert_streaks.cooldown_until END,
  updated_at = excluded.updated_at;

-- name: ListAlertStreaks :many
-- The streaks an operator reads, the most recent first, bounded.
SELECT rule_name, job_key, streak, last_fired_at, cooldown_until
  FROM alert_streaks
 ORDER BY updated_at DESC, rule_name, job_key
 LIMIT CAST(@max_rows AS BIGINT);

-- name: InsertAlertNotice :exec
-- Owe a destination one notice.
INSERT INTO alert_outbox (id, rule_name, destination, lease_id, alert_condition, payload, state, next_at, created_at)
VALUES (@id, @rule_name, @destination, @lease_id, @alert_condition, @payload, 'pending', @next_at, @created_at);

-- name: ListDueAlertNotices :many
-- What one destination is owed now, oldest first and bounded.
SELECT id, rule_name, lease_id, alert_condition, payload, attempts
  FROM alert_outbox
 WHERE destination = @destination AND state = 'pending' AND next_at <= @now
 ORDER BY created_at, id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: RetryAlertNotice :exec
-- A try that may succeed if made again.
UPDATE alert_outbox SET attempts = attempts + 1, next_at = @next_at, last_error = @last_error
 WHERE id = @id AND state = 'pending';

-- name: SettleAlertNotice :exec
-- End a notice, delivered or given up on. Only one still owed: an answer stands.
UPDATE alert_outbox
   SET state = @state, attempts = attempts + 1, last_error = @last_error, settled_at = @settled_at
 WHERE id = @id AND state = 'pending';

-- name: CountAlertNotices :many
-- How many notices each destination has in each state.
SELECT destination, state, COUNT(*) AS notices FROM alert_outbox GROUP BY destination, state
 ORDER BY destination, state;

-- name: ListFailedAlertNotices :many
-- The notices that were given up on, the most recent first, bounded: why a destination
-- is not hearing anything.
SELECT rule_name, destination, lease_id, alert_condition, last_error, settled_at
  FROM alert_outbox
 WHERE state = 'failed'
 ORDER BY settled_at DESC, id
 LIMIT CAST(@max_rows AS BIGINT);

-- name: PruneAlertMarks :exec
-- Forget judgements older than any job the loop still reads.
DELETE FROM alert_marks WHERE decided_at < @before;

-- name: PruneAlertNotices :exec
-- Forget settled notices. One still owed is kept: it has not been given up on.
DELETE FROM alert_outbox WHERE state != 'pending' AND settled_at < @before;

-- name: PruneAlertStreaks :exec
-- Forget a job nobody has run in a long while; a streak of that age says nothing. A
-- cooldown still running is kept, however long it was asked to be.
DELETE FROM alert_streaks WHERE updated_at < @before AND cooldown_until < @now;

-- name: ListPendingAlertDestinations :many
-- Every destination a notice is still owed to, so one the config no longer names can
-- be told so rather than owe for ever.
SELECT DISTINCT destination FROM alert_outbox WHERE state = 'pending' ORDER BY destination;

-- name: FailPendingAlertNotices :execrows
-- Give up on every notice still owed to one destination, with why.
UPDATE alert_outbox SET state = 'failed', last_error = @last_error, settled_at = @settled_at
 WHERE destination = @destination AND state = 'pending';
