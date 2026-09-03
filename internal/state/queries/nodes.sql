-- Registered hosts: the durable placement identity of one compute machine.
--
-- A NODE ROW OUTLIVES THE HOST'S CONNECTION. `live` is this control plane's
-- judgement and is cleared for the whole fleet whenever one starts, so nothing
-- here may read an absent or not-live host as a host that is gone. What removes
-- a machine from the set a drain expects to hear from is a decommission, and
-- `drained` is the column that records it.

-- name: ListRegisteredNodes :many
-- Every host the deployment has recorded, offline ones included.
SELECT name, provider, site, live,
       decommissioned_at, decommission_proven, decommission_actor
  FROM nodes
 ORDER BY name;

-- name: ListNodeWireVersions :many
-- What each host's registration said about its build, and what was negotiated.
--
-- highest_release is the newest release this host has EVER registered with, kept
-- beside node_release so a report can say a host is running something older than
-- it once did. It decides nothing: a rolled-back host is exactly that shape.
SELECT name, live, node_release, wire_min, wire_max, wire_version, epoch,
       node_digest, highest_release
  FROM nodes
 ORDER BY name;

-- name: ReadNodeHighestRelease :one
-- The newest release one host has registered with, for the registration that is
-- about to decide whether this one is newer still.
--
-- sql.ErrNoRows means the host was never registered, which the caller reads as
-- "nothing recorded" rather than as an error.
SELECT highest_release FROM nodes WHERE name = @name;

-- name: ReadNodeLiveness :one
-- Whether the deployment can currently reach one host.
--
-- sql.ErrNoRows means the host was never registered, which the caller reports
-- differently from a host that is simply not answering.
SELECT live FROM nodes WHERE name = @name;

-- name: CountOutstandingLeasesOnNode :one
-- How many leases still charge capacity against one host.
--
-- COALESCE(node, target_node), the way the rest of this package attributes a
-- lease: an escrow AIMED at a machine has already committed that machine's room
-- whether or not a container has started.
SELECT CAST(COUNT(*) AS BIGINT) FROM leases
 WHERE COALESCE(node, target_node) = CAST(@node AS TEXT) AND phase NOT IN ('done','failed');

-- name: DecommissionNode :exec
-- Stop expecting one host to answer.
--
-- decommission_proven RECORDS WHETHER ANYTHING PROVED IT WAS IDLE, and a forced
-- exclusion writes 0 rather than being omitted: an exclusion that cannot be told
-- from a proof is the whole membership rule defeated. `drained = 1` is what both
-- placement queries and the floor arithmetic already read.
UPDATE nodes
   SET decommissioned_at = @decommissioned_at, decommission_proven = @decommission_proven,
       decommission_actor = @decommission_actor, drained = 1, live = 0
 WHERE name = @name;

-- name: ListPlaceableNodes :many
-- The hosts a lease may be placed on at all.
--
-- ORDERED BY NAME, because Go map iteration is not and this list decides
-- placement: an unordered candidate set makes the same fleet produce different
-- answers on different runs, which is untestable and unexplainable in a log.
--
-- ONE STATEMENT FOR BOTH READERS. eligibleNodes filters it down to a tier's
-- acceptable providers in Go and liveNodes takes the whole fleet, because a
-- floor belonging to one tier is held on the hosts THAT tier could use and the
-- asking tier may not share them -- so the two differ in what they do with the
-- rows, never in which rows exist.
--
-- `drained = 0` IS REDUNDANT WITH `live = 1` IN EVERY STATE REACHABLE TODAY, and
-- that is said here rather than left for somebody to discover: a decommission
-- writes drained = 1 and live = 0 together, and a re-registration clears both
-- together, so no API path produces a live host that is drained. Measured --
-- deleting either half of this predicate fails no test in internal/alloc, while
-- deleting `live = 1` alone does. It stays because the two columns answer
-- different questions (can this host be reached, and does anybody still expect
-- it to serve), and the next path that sets liveness without going through
-- registration must not silently start placing work on an excluded host.
SELECT name, provider, site, total_vcpu, total_memory, ec2_shapes
  FROM nodes
 WHERE live = 1 AND drained = 0
 ORDER BY name;

-- name: ReadNodeIncarnation :one
-- The process one host presented at its current registration, or '' for a host
-- that never presented one.
--
-- THE DURABLE NAME OF A PROCESS, which the epoch is not: the epoch moves on
-- every registration and the same process registers again after a
-- control-plane restart. A lease records this at Bind and on entry to custody
-- or teardown, so a report can say whether the process holding it is the one
-- the host runs now. sql.ErrNoRows means the host is not in the fleet.
SELECT incarnation FROM nodes WHERE name = @name;

-- name: ReadNodeEpoch :one
-- One host's registration fence.
--
-- A REGISTRATION BUMPS IT AND NOTHING ELSE DOES, so a value that has not moved
-- proves the answer is about the same incarnation the caller was talking to.
-- sql.ErrNoRows means the host is not in the fleet, which is not the same as a
-- host whose epoch has moved and must not be reported as one.
SELECT epoch FROM nodes WHERE name = @name;

-- name: ReadNodeSize :one
-- What one host says it has.
SELECT total_vcpu, total_memory FROM nodes WHERE name = @name;

-- name: ListRemoteCostNodes :many
-- Every host whose compute is bought rather than owned.
--
-- THE PROVIDER LIST IS A LITERAL AND IT IS PINNED. sqlc.slice() is not available
-- on SQLite, so this cannot take config.RemoteProviders() as a bound list --
-- and a literal that drifts from that function is a backend whose cost stops
-- being reported at all, which reads as a fleet that costs nothing.
-- TestTheRemoteProviderListMatchesTheQueries is what keeps the two the same.
SELECT name, provider, total_vcpu, total_memory, ec2_shapes
  FROM nodes
 WHERE provider IN ('ec2','codebuild')
 ORDER BY name;

-- name: ListOutstandingRemoteShapes :many
-- What each remote host currently has running, by the shape it was charged for.
--
-- THE SHAPE, NOT THE TIER REQUEST. A remote lease is charged for the instance
-- placement actually bought, so a cost report keyed on anything else understates
-- a fallback. Same pinned provider list as ListRemoteCostNodes.
SELECT COALESCE(l.node, l.target_node) AS node, l.instance_type,
       CAST(COUNT(*) AS BIGINT) AS outstanding
  FROM leases l
  JOIN nodes n ON n.name = COALESCE(l.node, l.target_node)
 WHERE n.provider IN ('ec2','codebuild') AND l.phase NOT IN ('done','failed')
 GROUP BY COALESCE(l.node, l.target_node), l.instance_type;

-- name: UsageByNode :many
-- Every host's committed capacity, keyed the way a lease is attributed.
SELECT COALESCE(node, target_node, '') AS node,
       CAST(COALESCE(SUM(vcpu), 0) AS BIGINT) AS vcpu,
       CAST(COALESCE(SUM(memory), 0) AS BIGINT) AS memory,
       CAST(COALESCE(SUM(macos_slot), 0) AS BIGINT) AS macos_slots
  FROM leases
 WHERE phase NOT IN ('done','failed')
 GROUP BY COALESCE(node, target_node, '');

-- name: UsageOnNode :one
-- One host's committed capacity.
SELECT CAST(COALESCE(SUM(vcpu), 0) AS BIGINT) AS vcpu,
       CAST(COALESCE(SUM(memory), 0) AS BIGINT) AS memory,
       CAST(COUNT(*) AS BIGINT) AS leases
  FROM leases
 WHERE phase NOT IN ('done','failed')
   AND COALESCE(node, target_node) = CAST(@node AS TEXT);

-- name: ReadLeaseTargetSize :one
-- One lease's target host, its cost, and whether that host is reachable.
--
-- LEFT JOIN, because a target naming a host the ledger has never heard of is the
-- same situation as one it has forgotten: there is nowhere for that reservation
-- to go, and an inner join would report the lease as absent instead.
SELECT l.target_node, l.vcpu, l.memory, n.live
  FROM leases l LEFT JOIN nodes n ON n.name = l.target_node
 WHERE l.id = @id;

-- name: ReadNodeProvider :one
-- The backend one host reported at registration.
--
-- THE REGISTERED ONE, NEVER A CATALOGUE'S CLAIM. Placement asks whether a lease's
-- acceptable backends include what the machine itself said it runs; a firecracker
-- lease cannot run on a Tart host whatever the config says.
SELECT provider FROM nodes WHERE name = @name;

-- name: ReadNodeCapacity :one
-- What one host has, and the shapes it can buy.
SELECT total_vcpu, total_memory, ec2_shapes FROM nodes WHERE name = @name;

-- name: ReadNodeRegistration :one
-- The facts a re-registration has to be checked against.
SELECT provider, site, ec2_shapes, codebuild_fleet FROM nodes WHERE name = @name;

-- name: ListCodeBuildRegistrationPaths :many
-- Every codebuild host and the Parameter Store path it stages registrations under.
--
-- DECOMMISSIONED HOSTS INCLUDED, deliberately. The control plane sweeps these paths
-- for registrations a dead node never removed, and a host that has been taken out
-- of the fleet is exactly the one with nobody left to clean up after it. Rows with
-- an empty path are included too: a host that registered before it could name its
-- path is one `billet status` has to NAME as unswept rather than count as clean.
--
-- The provider is a parameter rather than a literal, so there is no second
-- spelling of the backend name to drift.
SELECT name, codebuild_region, codebuild_jit_path, decommissioned_at
  FROM nodes
 WHERE provider = @provider
 ORDER BY name;

-- name: CountLiveWorkOnNode :one
-- How much work on one host would be invalidated by a placement change.
--
-- STRICTER THAN CountOutstandingLeasesOnNode: an escrow that has already expired
-- holds no compute and is about to be reaped, so it must not block a host from
-- correcting its own registration -- but anything past `launching` has compute
-- behind it whatever its TTL says.
SELECT CAST(COUNT(*) AS BIGINT) FROM leases
 WHERE COALESCE(node, target_node) = CAST(@node AS TEXT)
   AND phase NOT IN ('done','failed')
   AND (phase IN ('launching','online','busy','custody','teardown','quarantine')
        OR expires_at > @now);

-- name: FleetClaimHolder :one
-- Another host that still claims one reserved CodeBuild fleet.
--
-- A RESERVED FLEET IS ONE SHARED POOL, so two nodes naming it each register its
-- whole capacity and the deployment promises GitHub more concurrent jobs than AWS
-- will run. Only the control plane can see that, because the two config files are
-- on two machines.
--
-- WHAT RELEASES A CLAIM IS decommission_proven = 1, NOT LIVENESS AND NOT AN
-- EXCLUSION. Liveness says nothing about remote compute -- a control plane start
-- marks every host not-live while its builds keep running -- and `--force`
-- records decommission_proven = 0 precisely because nothing could be asked. The
-- ordinary replacement needs none of this because it reuses the node NAME, which
-- the `name <> @name` clause excludes.
SELECT name FROM nodes
 WHERE codebuild_fleet = @fleet AND name <> @name
   AND (decommissioned_at = '' OR decommission_proven = 0)
 ORDER BY name
 LIMIT 1;

-- name: UpsertNodeRegistration :one
-- Register a host, or re-register one, and return its new fence.
--
-- THE EPOCH ALWAYS MOVES, which is what makes it the fleet's one causal signal: a
-- registration is the only thing that bumps it, so a value that has not changed
-- proves an answer is about the same incarnation.
--
-- A HOST THAT COMES BACK IS A MEMBER AGAIN. A decommission is a person asserting
-- a machine is gone; the machine registering is that assertion being contradicted
-- by the machine itself, and an exclusion nobody remembers would hide a live host
-- from every later drain forever. `drained` is cleared with it, because that is
-- the column placement and the floor arithmetic read.
INSERT INTO nodes
   (name, provider, site, total_vcpu, total_memory, ec2_shapes, last_seen_at, live,
    node_release, wire_min, wire_max, wire_version, node_digest, codebuild_fleet,
    codebuild_jit_path, codebuild_region, incarnation, highest_release, epoch)
VALUES (@name, @provider, @site, @total_vcpu, @total_memory, @ec2_shapes, @last_seen_at, 1,
        @node_release, @wire_min, @wire_max, @wire_version, @node_digest,
        @codebuild_fleet, @codebuild_jit_path, @codebuild_region, @incarnation,
        @highest_release, 1)
ON CONFLICT (name) DO UPDATE SET
   provider     = excluded.provider,
   site         = excluded.site,
   total_vcpu   = excluded.total_vcpu,
   total_memory = excluded.total_memory,
   ec2_shapes   = excluded.ec2_shapes,
   last_seen_at = excluded.last_seen_at,
   live         = 1,
   node_release = excluded.node_release,
   wire_min     = excluded.wire_min,
   wire_max     = excluded.wire_max,
   wire_version = excluded.wire_version,
   node_digest  = excluded.node_digest,
   codebuild_fleet = excluded.codebuild_fleet,
   codebuild_jit_path = excluded.codebuild_jit_path,
   codebuild_region   = excluded.codebuild_region,
   incarnation  = excluded.incarnation,
   highest_release = excluded.highest_release,
   epoch        = nodes.epoch + 1,
   decommissioned_at   = '',
   decommission_proven = 0,
   decommission_actor  = '',
   drained             = 0
RETURNING epoch;

-- name: MarkNodeNotLive :exec
-- Record that the control plane has given up on one host.
--
-- FENCED ON THE EPOCH. Registration commits to the ledger BEFORE it takes the
-- plane's mutex, and expiry holds that mutex while dropping the old entry -- so a
-- host that restarts quickly could commit its new registration and then be marked
-- dead by the expiry of the incarnation it replaced. A no-op once the epoch has
-- moved, which is the point.
UPDATE nodes SET live = 0 WHERE name = @name AND epoch = @epoch;

-- name: WithdrawNode :execrows
-- Record that one host said it is leaving, and take it out of placement.
--
-- FENCED ON THE EPOCH AND THE INCARNATION, and both are needed. The epoch is the
-- registration fence -- a withdrawal that arrives after the host has registered
-- again matches nothing -- and the incarnation is the durable name of the
-- PROCESS, so a superseded process that still holds the certificate cannot take
-- its replacement out of the fleet. Zero rows is the caller's answer that the
-- fence moved.
--
-- ONLY `live`. A withdrawal is not a decommission (drained, decommissioned_at
-- and the proof columns are untouched), releases no lease, and leaves the
-- host's inventory and barrier run alone -- the next registration discards
-- those, exactly as it does after silence. What changes is that ListPlaceableNodes
-- stops offering the host at once instead of after the silence window.
UPDATE nodes SET live = 0
 WHERE name = @name AND epoch = @epoch AND incarnation = @incarnation;

-- name: ForgetEveryNode :exec
-- Mark the whole fleet unreachable, which is what a control-plane start knows.
--
-- IT REMOVES NOTHING. A node row is the durable placement identity of a machine,
-- and a plane that has just started has no judgement about any host -- not that
-- the host is gone.
UPDATE nodes SET live = 0;

-- name: DeleteEveryNodeInventory :exec
-- And forget what they last said they were running.
--
-- A REPORT RECEIVED BY A PREVIOUS PROCESS SAYS NOTHING ABOUT NOW, and the node
-- epoch does NOT move on a plane restart -- so without this the record would
-- survive with a matching epoch and read as current to everything that consults
-- it.
DELETE FROM node_inventory;
