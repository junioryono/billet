-- migration 78: job_alerts
--
-- WHAT `server.alerts` HAS DECIDED AND WHAT IT STILL OWES (#537). The ledger holds it
-- rather than the process, so a restart neither repeats a notice nor forgets one, and
-- a standby that takes over delivers what its predecessor owed.
--
-- alert_rules is one row per rule name: when this deployment first saw it, because a
-- rule added today is about jobs from now on and not the day's history, and its
-- mute, which `billet alerts mute` writes with the control plane running.
--
-- alert_marks says a rule has judged a job on a condition, and the key is the rule: a
-- job counts once per rule and condition however many passes and controllers see it.
-- Written in the transaction that moves the streak, so the two cannot come apart.
--
-- alert_streaks is N-in-a-row and the cooldown, per rule and per job of a workflow of
-- a repository. It is counted by what a person calls one job, not by a lease.
--
-- alert_outbox is what is owed to a destination, written in the same transaction as
-- the decision and delivered afterwards, so a destination that hangs holds no write.
-- The payload is rendered at the decision, what the rule saw then. last_error is
-- the class of a failure or the status a destination answered, never its address,
-- which is a credential, and never what it wrote.
--
-- Vocabularies are closed in Go rather than by a CHECK, for migration 35's reason. The
-- id is generated in Go: the two engines have no autoincrement in common.
--
-- The two partial indexes on job_history serve the evaluation, which every pass reads
-- the jobs still running and the jobs that recently ended, and job_history is
-- unbounded while those are few.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE alert_rules (
    name TEXT PRIMARY KEY NOT NULL,
    first_seen_at TEXT NOT NULL,
    muted_until TEXT NOT NULL DEFAULT '',
    muted_by TEXT NOT NULL DEFAULT ''
) STRICT
-- +billet:end

-- +billet:statement
CREATE TABLE alert_marks (
    rule_name TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    alert_condition TEXT NOT NULL,
    outcome TEXT NOT NULL,
    decided_at TEXT NOT NULL,
    PRIMARY KEY (rule_name, lease_id, alert_condition)
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX alert_marks_decided_idx ON alert_marks(decided_at)
-- +billet:end

-- +billet:statement
CREATE TABLE alert_streaks (
    rule_name TEXT NOT NULL,
    job_key TEXT NOT NULL,
    streak INTEGER NOT NULL DEFAULT 0 CHECK (streak >= 0),
    last_fired_at TEXT NOT NULL DEFAULT '',
    cooldown_until TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (rule_name, job_key)
) STRICT
-- +billet:end

-- +billet:statement
CREATE TABLE alert_outbox (
    id TEXT PRIMARY KEY NOT NULL,
    rule_name TEXT NOT NULL,
    destination TEXT NOT NULL,
    lease_id TEXT NOT NULL,
    alert_condition TEXT NOT NULL,
    payload TEXT NOT NULL,
    state TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    settled_at TEXT NOT NULL DEFAULT ''
) STRICT
-- +billet:end

-- +billet:statement
CREATE INDEX alert_outbox_due_idx ON alert_outbox(destination, state, next_at)
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_running_idx ON job_history(started_at)
			WHERE result = ''
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_result_idx ON job_history(result_at)
			WHERE result != ''
-- +billet:end
