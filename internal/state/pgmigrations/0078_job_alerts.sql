-- migration 78: job_alerts
--
-- The twin of internal/state/migrations/0078_job_alerts.sql; the reasoning is there.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE alert_rules (
    name text PRIMARY KEY NOT NULL,
    first_seen_at text NOT NULL,
    muted_until text NOT NULL DEFAULT '',
    muted_by text NOT NULL DEFAULT ''
);
-- +billet:end

-- +billet:statement
CREATE TABLE alert_marks (
    rule_name text NOT NULL,
    lease_id text NOT NULL,
    alert_condition text NOT NULL,
    outcome text NOT NULL,
    decided_at text NOT NULL,
    PRIMARY KEY (rule_name, lease_id, alert_condition)
);
-- +billet:end

-- +billet:statement
CREATE INDEX alert_marks_decided_idx ON alert_marks(decided_at);
-- +billet:end

-- +billet:statement
CREATE TABLE alert_streaks (
    rule_name text NOT NULL,
    job_key text NOT NULL,
    streak bigint NOT NULL DEFAULT 0 CHECK (streak >= 0),
    last_fired_at text NOT NULL DEFAULT '',
    cooldown_until text NOT NULL DEFAULT '',
    updated_at text NOT NULL,
    PRIMARY KEY (rule_name, job_key)
);
-- +billet:end

-- +billet:statement
CREATE TABLE alert_outbox (
    id text PRIMARY KEY NOT NULL,
    rule_name text NOT NULL,
    destination text NOT NULL,
    lease_id text NOT NULL,
    alert_condition text NOT NULL,
    payload text NOT NULL,
    state text NOT NULL,
    attempts bigint NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_at text NOT NULL,
    last_error text NOT NULL DEFAULT '',
    created_at text NOT NULL,
    settled_at text NOT NULL DEFAULT ''
);
-- +billet:end

-- +billet:statement
CREATE INDEX alert_outbox_due_idx ON alert_outbox(destination, state, next_at);
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_running_idx ON job_history(started_at)
			WHERE result = '';
-- +billet:end

-- +billet:statement
CREATE INDEX job_history_result_idx ON job_history(result_at)
			WHERE result != '';
-- +billet:end
