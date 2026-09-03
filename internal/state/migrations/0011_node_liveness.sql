-- migration 11: node_liveness
--
-- WHETHER A HOST IS REACHABLE IS NOW A FACT THE LEDGER NEEDS, because capacity
-- is counted here and a machine that is gone must stop backing advertisements.
--
-- SEPARATE FROM `drained`, which is a different state: draining is a host that
-- is finishing its work and taking no more, and it is still there. This is the
-- plane's judgement about whether it is there at all.
--
-- DEFAULTS TO 0, so a ledger written by an older billet trusts nothing until
-- each node registers again — which is the same conservative start a restart
-- gets, and the correct one: liveness is the plane's judgement and a plane that
-- has just started has not formed one.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN live INTEGER NOT NULL DEFAULT 0 CHECK (live IN (0, 1))
-- +billet:end
