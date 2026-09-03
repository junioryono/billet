-- migration 46: codebuild_registration_sweep
--
-- A STAGED CODEBUILD REGISTRATION OUTLIVES THE BUILD IT WAS STAGED FOR. The
-- runner's JIT configuration cannot travel in StartBuild, so it is written to
-- Parameter Store first, and three places remove it, each holding a proof that the
-- compute is gone. A node process that dies between staging one and reaching any
-- of them leaks exactly one parameter, and nothing on the node can ever authorise
-- deleting it: from the provider alone, "no build for this lease" and "the build
-- has not appeared yet" are the same observation. What can authorise it is the
-- LEDGER — the lease is terminal, and has been for longer than any build could
-- still be running — and only the control plane holds that. So the control plane
-- sweeps, and these columns are what it needs in order to.
--
-- THE PATH AND THE REGION COME FROM THE NODE'S REGISTRATION, beside the fleet it
-- draws on, because the control plane's own config usually carries no node block
-- at all and a second copy of the path in a second file would be the two-pins
-- problem. They are recorded on the node row rather than kept in memory so the
-- sweep still covers a host that has since been DECOMMISSIONED: its leaked
-- registrations are precisely the ones nobody is left to clean up. Empty means
-- the host registered before it could say, or is not a codebuild node; `billet
-- status` names the first case rather than reading it as swept.
--
-- credential_sweeps IS THE RECORD OF WHAT THE SWEEP DID, one row per region and
-- path, because `billet status` runs in another process and a count that lived in
-- the control plane's memory would be invisible to it. removed is the last pass,
-- removed_total accumulates, and error is the last pass's failure or empty — a
-- sweep that cannot read the ledger keeps everything and says so here.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN codebuild_jit_path TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
ALTER TABLE nodes ADD COLUMN codebuild_region TEXT NOT NULL DEFAULT ''
-- +billet:end

-- +billet:statement
CREATE TABLE credential_sweeps (
	region        TEXT NOT NULL,
	path          TEXT NOT NULL,
	swept_at      TEXT NOT NULL,
	removed       INTEGER NOT NULL CHECK (removed >= 0),
	removed_total INTEGER NOT NULL CHECK (removed_total >= 0),
	kept          INTEGER NOT NULL CHECK (kept >= 0),
	unaccounted   INTEGER NOT NULL CHECK (unaccounted >= 0),
	foreign_names INTEGER NOT NULL CHECK (foreign_names >= 0),
	error         TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (region, path)
) STRICT
-- +billet:end
