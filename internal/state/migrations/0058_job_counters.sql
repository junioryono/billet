-- migration 58: job_counters
--
-- WHAT THE CPU'S HARDWARE COUNTERS SAW A JOB DO, beside what migration 57 keeps
-- of what it did to the host. With node.monitoring.perf the node counts each of
-- a microVM's vCPU threads with perf_event_open, guest mode included, and
-- reports the totals once with the rest of the usage summary: cycles,
-- instructions, cache references and misses, branch misses and the cycles the
-- frontend issued nothing. A reader derives IPC and misses per 1,000
-- instructions from them.
--
-- EACH COLUMN IS NULL WHERE THE EVENT WAS NOT COUNTED, never zero, because zero
-- is also a real count: a node without perf, a backend with no vCPU threads, a
-- CPU without the event (Zen 3 has no stalled-cycles-backend, which is why it is
-- not here), a group the kernel never scheduled, or a report from before this
-- migration. NULL rather than migration 57's zero-and-named, because each event
-- is its own verdict and every row written before this has none of them.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN cycles INTEGER
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN instructions INTEGER
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN cache_references INTEGER
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN cache_misses INTEGER
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN branch_misses INTEGER
-- +billet:end

-- +billet:statement
ALTER TABLE job_usage ADD COLUMN frontend_stall_cycles INTEGER
-- +billet:end
