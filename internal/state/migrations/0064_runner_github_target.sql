-- migration 64: runner_github_target
--
-- A RUNNER'S CREDENTIAL IS THE OWNER IT WAS REGISTERED ON, NOT WHAT THE TIER
-- NAMES TODAY. A lease and its pool-runner binding recorded the tier label and
-- not the tier's GitHub target, so the node plane resolved the credential for a
-- runner's removal and a quarantine's recovery through the tier catalogue it was
-- started with. On a deployment serving several targets, a runner whose tier had
-- since been removed from the config resolved to a tier naming no target, and
-- the plane refused it rather than serve it through another owner's App (#93).
--
-- github_target is the target's GitHub path (`owner`, or `owner/name` for a
-- repository), the identity scale_set records already use, never its config
-- name, which an operator may rename. The plane writes it on the lease before it
-- mints the registration, so a mint whose answer was lost is still recovered on
-- the right owner, and on the binding it journals after. Empty means written
-- before this migration, or by an assembly with one credential and no path,
-- which every row from before this reads as.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE leases ADD COLUMN github_target TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE pool_runners ADD COLUMN github_target TEXT NOT NULL DEFAULT ''
-- +billet:end
