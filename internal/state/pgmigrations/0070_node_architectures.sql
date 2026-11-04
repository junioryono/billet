-- migration 70: node_architectures
--
-- THE ARCHITECTURES A NODE'S PROVIDER CAN LAUNCH (ADR-022), as the node reported
-- them at its last registration: the host's own for a provider that runs work on
-- it, a CodeBuild node's environment's, and an EC2 node's shapes', which the
-- ec2_shapes column also carries one by one. A sorted comma list (config.JoinArchs)
-- read back through config.SplitArchs, which refuses a name nothing wrote.
--
-- EMPTY IS "REPORTS NONE", not "any": every row from before this migration, and
-- every node registered over a wire before VersionArchitectures. Placement serves
-- such a node only tiers that name no architecture, which is how every node was
-- placed before a tier could name one.
--
-- Everything between the markers below is PUBLISHED BYTES; the prose is not.
-- Reformat one tab and every ledger that applied this migration refuses to open.

-- +billet:statement
ALTER TABLE nodes ADD COLUMN architectures text NOT NULL DEFAULT '';
-- +billet:end
