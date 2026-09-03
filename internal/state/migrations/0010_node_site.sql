-- migration 10: node_site
--
-- A NODE IS SOMEWHERE, and until there was a word for where, a cache had no
-- address and every host looked equally close to every other one.
--
-- Empty is "unsited", which is what every existing row is and what a
-- single-machine deployment stays. It is a real value rather than a missing one:
-- one place is still one place, and a deployment that never names it should not
-- have to.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN site TEXT NOT NULL DEFAULT ''
-- +billet:end
