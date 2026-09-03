-- migration 32: scale_set_provenance
--
-- WHAT BILLET CREATED, so it can say what it no longer declares.
--
-- A scale set outlives the tier that made it: removing a tier from billet.yaml
-- is the ordinary way to stop offering a size, and the object it created stays
-- on the organization advertising nothing — so a job aimed at that label queues
-- rather than failing, which is billet's characteristic failure reached by an
-- ordinary config edit. Nothing could TELL an operator it was there, because
-- the config was the only index and GitHub's client cannot enumerate a runner
-- group: its list call always filters by name, so billet can ask about a scale
-- set it can name and no others.
--
-- Recording what it creates is therefore the only way to notice. It is also
-- provenance the teardown path lacks: `billet teardown` on an undeclared tier
-- checks the label billet would have given it, which is evidence but not proof
-- of who made it.
--
-- Keyed by (org, runner_group, label). The same label in two groups is two
-- scale sets — exactly why teardown reports "nothing in runner group X; if it
-- was created under a different group it is still there" — and a scale set
-- belongs to an organization, so a state directory pointed at a different one
-- must not have its records collide with the first's.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE scale_set (
			org          TEXT NOT NULL,
			runner_group TEXT NOT NULL,
			label        TEXT NOT NULL,
			scale_set_id INTEGER NOT NULL CHECK (scale_set_id > 0),
			created_at   TEXT NOT NULL,
			PRIMARY KEY (org, runner_group, label)
		) STRICT
-- +billet:end
