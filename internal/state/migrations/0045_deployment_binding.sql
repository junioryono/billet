-- migration 45: deployment_binding
--
-- WHICH DEPLOYMENT THESE ROWS BELONG TO.
--
-- The deployment identity is a file in the identity directory, and while the
-- ledger was a file beside it the pairing needed nothing to enforce it: one
-- directory, one identity, one ledger. A PostgreSQL ledger is reachable from
-- anywhere and recorded nothing about whose it was, so two hosts could share one
-- capacity record under two identities — each control plane admitting nodes
-- against its own authority while charging capacity into one ledger, with
-- nothing anywhere naming the disagreement.
--
-- A SINGLETON, like admission and the controller claim, because this is one
-- deployment-wide fact rather than a set. It is written by the first claim that
-- finds it absent and never replaced: the legitimate operation a rebind would
-- serve is relabelling a deployment, which every other command in billet already
-- refuses.
--
-- IT IS NOT A SECRET AND IT IS NOT A CREDENTIAL. The identity is the label
-- billet puts on compute so it can find it again; recording it here lets a
-- controller prove the ledger it is about to schedule against is the one its
-- authority belongs to.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
CREATE TABLE deployment_binding (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	deployment_id TEXT NOT NULL CHECK (deployment_id <> ''),
	bound_at      TEXT NOT NULL
) STRICT
-- +billet:end
