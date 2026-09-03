-- migration 31: admission
--
-- admissionMigration records whether the deployment is accepting new work.
--
-- A SINGLETON, because admission is one deployment-wide fact rather than a set
-- of them: `CHECK (id = 1)` makes a second row impossible rather than merely
-- unusual, and the row is inserted here so every read finds one and no caller
-- has to treat "no row" as a state.
--
-- The generation is the fence. Clearing a seal presents the generation it meant
-- to clear, so an operator's `up` cannot undo a seal a later operator created in
-- between — it fails and says so instead. Every transition increments it,
-- including reseal, so a generation names one decision.
--
-- Provenance separates a seal a lifecycle command took for its own duration
-- from one an operator took deliberately. `billet local up` clears only its own
-- kind; a maintenance seal survives a control-plane restart and needs an
-- explicit resume, because silently reopening admission is the failure the whole
-- mechanism exists to prevent.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE admission (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			mode       TEXT NOT NULL CHECK (mode IN ('open','sealed')),
			generation INTEGER NOT NULL CHECK (generation >= 0),
			provenance TEXT NOT NULL CHECK (provenance IN ('','local-down','operator')),
			reason     TEXT NOT NULL,
			actor      TEXT NOT NULL,
			changed_at TEXT NOT NULL
		) STRICT
-- +billet:end

-- +billet:statement
INSERT INTO admission (id, mode, generation, provenance, reason, actor, changed_at)
		 VALUES (1, 'open', 0, '', '', '', '')
-- +billet:end
