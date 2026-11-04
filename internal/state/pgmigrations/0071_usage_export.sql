-- migration 71: usage_export, for PostgreSQL
--
-- The twin of migrations/0071_usage_export.sql.
--
-- It carries that file's statements with SQLite's spellings translated and
-- nothing else changed, so the two read side by side. The reasoning lives
-- there rather than being duplicated here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every PostgreSQL ledger that applied this migration
-- refuses to open.

-- +billet:statement
ALTER TABLE job_history ADD COLUMN github_target text NOT NULL DEFAULT '';
-- +billet:end

-- +billet:statement
ALTER TABLE job_history ADD COLUMN usage_seq bigint NOT NULL DEFAULT 0 CHECK (usage_seq >= 0);
-- +billet:end

-- +billet:statement
CREATE TABLE usage_sequence (
    id bigint PRIMARY KEY CHECK (id = 1),
    last bigint NOT NULL CHECK (last >= 0)
);
-- +billet:end

-- +billet:statement
UPDATE job_history
   SET usage_seq = numbered.n
  FROM (SELECT lease_id, ROW_NUMBER() OVER (ORDER BY finished_at, lease_id) AS n
          FROM job_history
         WHERE finished_at IS NOT NULL AND finished_at != '') AS numbered
 WHERE numbered.lease_id = job_history.lease_id;
-- +billet:end

-- +billet:statement
INSERT INTO usage_sequence (id, last)
SELECT 1, top.n FROM (SELECT MAX(usage_seq) AS n FROM job_history) AS top WHERE top.n > 0;
-- +billet:end

-- +billet:statement
CREATE UNIQUE INDEX job_history_usage_seq ON job_history (usage_seq) WHERE usage_seq > 0;
-- +billet:end
