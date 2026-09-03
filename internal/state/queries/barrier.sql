-- The compute barrier: a durable request that every host in the fleet be ASKED
-- what it is running, and the fenced answers.
--
-- WHY THE LEDGER CANNOT ANSWER THIS ON ITS OWN. alloc.Quiescence reads leases,
-- and the class this exists for is compute whose lease has already gone: a
-- listener that loses a running lease keeps an in-memory obligation to destroy
-- what it launched, and a launch whose lease was reclaimed can create compute it
-- then fails to destroy. Neither is in the ledger and neither survives a restart,
-- but both leave an instance carrying billet's own name on the host's provider.
--
-- WHAT MAKES AN ANSWER CAUSAL IS TWO FENCES, and every statement here carries
-- both. `node_epoch` is the registration in force -- a reconnect moves it, so a
-- proof about a previous incarnation stops counting. `dispatch_generation` is
-- advanced durably BEFORE a launch enters that host's queue, so an answer that
-- crossed a launch matches nothing rather than being believed.
--
-- WHAT IS STORED IS A CONTINUOUS RUN, NOT A SNAPSHOT. An error, a non-empty list
-- or either fence moving DELETES the row rather than ageing it, and the proof is
-- `observed_at - empty_since` -- two observations spanning the grace, never one
-- observation plus elapsed wall clock. A host that answered empty once and then
-- disconnected must not age into proved.

-- name: BumpDispatchGeneration :one
-- Advance one host's launch-dispatch fence and return the new value.
--
-- IT COUNTS LAUNCHES, NOT COMMANDS. A destroy, sweep, tend or inventory cannot
-- create compute, so charging them would void proofs for no reason.
--
-- sql.ErrNoRows means the host has no row -- and a host with no row cannot be
-- proved clear either, since every clearance walks the node table, so there is no
-- fence to advance and nothing about it that could later be believed.
UPDATE nodes SET dispatch_generation = dispatch_generation + 1
 WHERE name = @name
 RETURNING dispatch_generation;

-- name: ReadNodeBarrierFence :one
-- The fence a barrier observation of this host must carry, plus liveness.
--
-- LIVENESS IS NOT PART OF THE FENCE and travels here only so one read answers the
-- whole question: a host billet cannot reach still holds whatever it holds.
SELECT epoch, dispatch_generation, wire_version, live FROM nodes WHERE name = @name;

-- name: ReadNodeFence :one
-- Just the two fences, for the paths that have already decided about liveness.
SELECT epoch, dispatch_generation FROM nodes WHERE name = @name;

-- name: ReadComputeBarrier :one
-- The durable barrier request, if there is one.
SELECT barrier_id, admission_generation, requested_at, requested_by
  FROM compute_barrier WHERE id = 1;

-- name: UpsertComputeBarrier :exec
-- Record the one barrier request this deployment may have outstanding.
--
-- A SINGLETON BY CONSTRUCTION (id = 1, CHECKed by the schema): two barriers would
-- each be collecting observations the other's generation invalidates.
INSERT INTO compute_barrier
     (id, barrier_id, admission_generation, requested_at, requested_by)
VALUES (1, @barrier_id, @admission_generation, @requested_at, @requested_by)
ON CONFLICT(id) DO UPDATE SET
     barrier_id           = excluded.barrier_id,
     admission_generation = excluded.admission_generation,
     requested_at         = excluded.requested_at,
     requested_by         = excluded.requested_by;

-- name: DeleteComputeBarrier :exec
-- Remove a request that can no longer mean anything.
DELETE FROM compute_barrier WHERE id = 1;

-- name: DeleteEveryBarrierRun :exec
-- Discard what every host had proved.
--
-- REACHED FROM THREE PLACES AND EACH ONE MATTERS. A superseded barrier's
-- observations go with it, because they were taken under a generation somebody
-- has since moved and admission was open in between. A dropped barrier takes its
-- own. And an arrival that cannot be ATTRIBUTED to a host -- a loopback
-- registration whose body would not decode -- invalidates everything, because "I
-- could not tell who arrived" must not read as "nothing changed".
DELETE FROM compute_barrier_nodes;

-- name: DeleteBarrierRun :exec
-- Discard one host's continuous-empty run.
DELETE FROM compute_barrier_nodes WHERE node = @node;

-- name: BarrierRunExists :one
-- Whether one host has a run at all.
--
-- READ BEFORE THE WRITE, and that is not a micro-optimisation: the invalidation
-- runs on EVERY registration, ahead of the revocation check, so a credential an
-- operator has taken back can call it as fast as it can open connections. An
-- unconditional write transaction would let it reserve SQLite's single writer
-- slot over and over and starve every scheduling decision in the process.
SELECT EXISTS(SELECT 1 FROM compute_barrier_nodes WHERE node = @node);

-- name: AnyBarrierRunExists :one
-- Whether the fleet has any run at all, for the same reason.
SELECT EXISTS(SELECT 1 FROM compute_barrier_nodes);

-- name: RecordBarrierRun :exec
-- Extend one host's continuous-empty run, or start a new one.
--
-- THE RUN'S START IS PRESERVED, AND ONLY WHERE EVERY PART OF ITS IDENTITY
-- MATCHES. Taking excluded.empty_since unconditionally would restart the clock on
-- every sample, so a host could never cross the grace no matter how long it
-- stayed empty; taking the stored one unconditionally would carry a run across a
-- reconnect or a launch. The CASE is what makes it a run rather than either.
INSERT INTO compute_barrier_nodes
     (node, barrier_id, node_epoch, dispatch_generation, empty_since, observed_at)
VALUES (@node, @barrier_id, @node_epoch, @dispatch_generation, @now, @now)
ON CONFLICT(node) DO UPDATE SET
     barrier_id          = excluded.barrier_id,
     node_epoch          = excluded.node_epoch,
     dispatch_generation = excluded.dispatch_generation,
     empty_since = CASE
         WHEN compute_barrier_nodes.barrier_id          = excluded.barrier_id
          AND compute_barrier_nodes.node_epoch          = excluded.node_epoch
          AND compute_barrier_nodes.dispatch_generation = excluded.dispatch_generation
         THEN compute_barrier_nodes.empty_since
         ELSE excluded.empty_since
     END,
     observed_at = excluded.observed_at;

-- name: ListBarrierRuns :many
-- Every run recorded under one barrier.
SELECT node, node_epoch, dispatch_generation, empty_since, observed_at
  FROM compute_barrier_nodes WHERE barrier_id = @barrier_id;

-- name: ReadBarrierRun :one
-- One host's run under one barrier.
SELECT node_epoch, dispatch_generation, empty_since, observed_at
  FROM compute_barrier_nodes WHERE node = @node AND barrier_id = @barrier_id;

-- name: ListFleetClearance :many
-- Every host, with what a clearance needs to judge it.
SELECT name, live, epoch, dispatch_generation, wire_version,
       decommissioned_at, decommission_proven, decommission_actor
  FROM nodes
 ORDER BY name;

-- name: ListHostsReportingCompute :many
-- The hosts whose own last word, under the registration in force, was that they
-- were running something.
--
-- TELEMETRY THAT CAN NEVER CLEAR AND MAY ALWAYS BLOCK. The node lists its
-- provider and THEN posts, so this can never be a proof -- but a host SAYING it
-- is running something is a fact, and the two coexist precisely when a create the
-- provider accepted has not yet been listed: a run completes around it and no
-- fence moves.
SELECT i.node
  FROM node_inventory i
  JOIN nodes n ON n.name = i.node
 WHERE i.node_epoch = n.epoch AND i.running > 0;

-- name: HostReportsCompute :one
-- The same question about one host.
--
-- ASKED BY provedTx AS WELL AS BY THE FLEET REPORT, so the two cannot disagree
-- about one host at one instant. Without it a machine actively reporting compute
-- could be decommissioned as PROVEN -- permanently out of the expected set, on a
-- run whose samples simply predated the instance becoming visible.
SELECT EXISTS(
  SELECT 1 FROM node_inventory i
    JOIN nodes n ON n.name = i.node
   WHERE i.node = @node AND i.node_epoch = n.epoch AND i.running > 0);
