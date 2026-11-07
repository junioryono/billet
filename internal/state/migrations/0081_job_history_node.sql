-- migration 81: job_history_node
--
-- WHEN A POOL HOST LAST RAN A JOB (ADR-030, #553). The pool shrinks only by drain, and
-- only a host idle for longer than the operator's `server.pool.idle_after` is proposed
-- for one, so the scaler reads, at every step, the end of the newest job each joined
-- host ran. Without an index that is a scan of the whole job history per host per step,
-- and the history keeps every job for its retention; with one it is a lookup of the
-- largest finished_at under one node.

-- +billet:statement
CREATE INDEX job_history_node_finished_idx ON job_history (node, finished_at)
-- +billet:end
