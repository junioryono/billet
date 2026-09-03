-- migration 42: rollout_converged_digest
--
-- rolloutConvergedDigestMigration records what proved a host converged.
--
-- DURABLE, RATHER THAN RE-DERIVED FROM THE LIVE FLEET. A host's current digest
-- answers "what is it running now"; this answers "what did this rollout accept as
-- evidence", and the two stop agreeing the moment anything upgrades that host
-- again. Without it a completed rollout cannot say which of its hosts were proved
-- against the manifest it decided on and which were taken on their version alone
-- — which is exactly the distinction this whole change exists to make visible.
--
-- EMPTY MEANS THE HOST NAMED NO MANIFEST, which is a converged host that nothing
-- proved rather than a host that failed.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE rollout_nodes ADD COLUMN converged_digest TEXT NOT NULL DEFAULT ''
-- +billet:end
