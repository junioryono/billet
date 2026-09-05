-- The capacity ledger's central row.
--
-- A LEASE IS THE UNIT OF CAPACITY, and every rule about not promising a machine
-- billet does not have is expressed as a predicate on this table. Two of them
-- recur in almost every statement and are worth knowing before reading any of
-- them:
--
--   COALESCE(node, target_node)   `node` is where the work actually went and
--                                 `target_node` is where billet decided it
--                                 would go. Capacity is charged on the
--                                 coalesce, because a reservation aimed at a
--                                 machine has already spent that machine's
--                                 room -- counting only bound leases let a tier
--                                 escrow against the same host repeatedly in
--                                 the window before its first launch.
--
--   AND epoch = @epoch            The fence. A holder declared dead and replaced
--                                 must not keep writing to a lease somebody else
--                                 now owns, so every write that acts on a row a
--                                 caller read presents the epoch it read.

-- name: ReadLeaseEpoch :one
-- One lease's current fencing token.
--
-- sql.ErrNoRows means the lease is gone, which callers report as
-- ErrLeaseNotFound rather than as a read failure.
SELECT epoch FROM leases WHERE id = @id;

-- name: InsertLease :exec
-- Escrow one slot.
--
-- node IS NULL AND target_node IS NOT. The two answer different questions: the
-- target is where billet DECIDED the work goes, the node is where it actually
-- went. Capacity is charged on COALESCE(node, target_node) precisely so a
-- reservation aimed at a machine spends that machine's room immediately --
-- counting only bound leases let a tier escrow against the same host repeatedly
-- in the window before its first launch, which is the deployment-level overcommit
-- moved down to the machine.
--
-- THE REQUESTED AND CHARGED VECTORS ARE BOTH RECORDED. A remote lease is charged
-- for the SHAPE placement bought rather than the smaller tier request, and a
-- fallback resizes the charge; the immutable request is what a later shape is
-- checked against.
--
-- THE PRICE IS THE SHAPE'S PRICE AT THE MOMENT IT IS CHARGED, in millionths of
-- a dollar, and the site is the placed host's registered site. Both are written
-- here rather than read back later because a node may re-register with a new
-- catalogue while this lease is open, and the history a terminalization copies
-- has to say what the deployment bought, not what it would pay today. Zero is a
-- host-backed lease, which buys nothing.
INSERT INTO leases
   (id, tier, node, target_node, macos_slot, guest_os, providers, phase, vcpu, memory,
    requested_vcpu, requested_memory, instance_type, site, price_micros_per_hour,
    epoch, created_at, heartbeat_at, expires_at)
VALUES (@id, @tier, NULL, @target_node, @macos_slot, @guest_os, @providers, @phase,
        @vcpu, @memory, @requested_vcpu, @requested_memory, @instance_type, @site,
        @price_micros_per_hour, 0, @created_at, @heartbeat_at, @expires_at);

-- name: AssignLease :exec
-- Bind an escrowed lease to a GitHub job and refresh its TTL.
UPDATE leases
   SET phase = @phase, run_id = @run_id, request_id = @request_id,
       heartbeat_at = @heartbeat_at, expires_at = @expires_at
 WHERE id = @id AND epoch = @epoch;

-- name: BindLease :exec
-- Record which host the work actually went to, and the backend it runs on.
--
-- THE PROVIDER IS THE HOST'S REGISTERED ONE, read in this transaction rather than
-- taken from a catalogue: the registration is what the machine itself reported.
--
-- holder_incarnation IS THAT HOST'S INCARNATION, read in the same transaction:
-- the process that is launching this compute. A report compares it with the
-- node's current incarnation to say whether the process that was given the
-- work is still the one the deployment talks to. It decides nothing.
UPDATE leases SET node = @node, chosen_provider = @chosen_provider,
       holder_incarnation = @holder_incarnation
 WHERE id = @id AND epoch = @epoch;

-- name: ResizeLease :exec
-- Authorise a remote fallback shape, atomically with the budget check above it.
--
-- A LATER FALLBACK IS A NEW PURCHASE DECISION, so its larger resource vector must
-- fit before the launch request is allowed onto the wire -- never reconciled
-- afterwards.
--
-- THE PRICE MOVES WITH THE SHAPE: a fallback is a different purchase at a
-- different rate, and the history records what was bought.
UPDATE leases SET vcpu = @vcpu, memory = @memory, instance_type = @instance_type,
       price_micros_per_hour = @price_micros_per_hour
 WHERE id = @id AND epoch = @epoch;

-- name: HeartbeatLease :exec
-- Push a lease's expiry out.
UPDATE leases SET heartbeat_at = @heartbeat_at, expires_at = @expires_at
 WHERE id = @id AND epoch = @epoch;

-- name: MarkLeaseFailure :exec
-- Record why a still-open lease is destined to fail.
--
-- SEPARATE FROM RELEASING IT, because the fact often arrives before the compute
-- is gone and capacity must stay charged throughout that interval.
UPDATE leases SET failure_reason = @failure_reason WHERE id = @id AND epoch = @epoch;

-- name: BackfillLeaseFailureReason :exec
-- The lease-row half of BackfillFailureReason: a failed row with no reason.
UPDATE leases SET failure_reason = @failure_reason
 WHERE id = @id AND phase = 'failed' AND failure_reason = '';

-- name: MarkLeaseDeregistered :exec
-- Record that GitHub's runner registration for this lease has been removed.
--
-- DELIBERATELY UNFENCED. Deregistration is a monotonic fact about GitHub, not
-- about who holds the lease: once RemoveRunner has succeeded the runner is gone
-- whatever the epoch. A quarantined lease has only terminal successors and a reap
-- never relaunches on the same row -- new capacity is always a fresh id -- so no
-- live runner can occupy a row this flag has set. Fencing it would let a reap
-- landing between RemoveRunner and this mark strand a gone runner as counted
-- forever.
UPDATE leases SET deregistered = 1 WHERE id = @id;

-- name: SetLeasePhase :exec
-- Move a lease to a phase the caller has already validated.
UPDATE leases SET phase = @phase WHERE id = @id AND epoch = @epoch;

-- name: HoldLease :exec
-- Move a lease into a phase that holds compute, and keep its TTL alive.
--
-- held_at IS WHEN THE OBLIGATION STARTED, which is what an operator report ages
-- from. The TTL still moves because a held lease is still being tended.
--
-- holder_incarnation IS REWRITTEN ON THE WAY INTO CUSTODY OR TEARDOWN, because
-- the process taking the obligation may not be the one that launched the
-- compute -- a restart adopts what it finds -- and the caller passes the value
-- it read for every other phase so the column is never blanked.
UPDATE leases
   SET phase = @phase, held_at = @held_at, heartbeat_at = @heartbeat_at,
       expires_at = @expires_at, holder_incarnation = @holder_incarnation
 WHERE id = @id AND epoch = @epoch;

-- name: RefreshLeaseHolder :exec
-- Re-record which process holds a lease already in a held phase.
--
-- FOR A REPORT OF A HOLD THE LEASE IS ALREADY IN. A restart adopts a teardown
-- or custody it finds and reports the same phase; the idempotent report writes
-- nothing else, and without this the process that died would stay the durable
-- holder while its replacement renews and tends the compute. Fenced on the
-- epoch like every other write, and a no-op when nothing changed.
UPDATE leases SET holder_incarnation = @holder_incarnation
 WHERE id = @id AND epoch = @epoch AND holder_incarnation <> @holder_incarnation;

-- name: ReclaimLease :exec
-- Terminalize or quarantine an expired lease, bumping the fence.
--
-- THE EPOCH BUMP IS THE POINT. A holder that comes back -- a paused process, a
-- healed partition -- finds its writes refused rather than silently operating on
-- a lease somebody else now owns.
--
-- held_at IS SET ONLY ON THE WAY INTO QUARANTINE, and only if it is empty: that
-- is when the proof obligation starts, and a lease reaped twice must not have its
-- age reset.
UPDATE leases
   SET phase = @phase, epoch = epoch + 1,
       held_at = CASE WHEN CAST(@phase AS TEXT) = CAST(@quarantine AS TEXT)
                       AND held_at = '' THEN CAST(@held_at AS TEXT) ELSE held_at END
 WHERE id = @id AND epoch = @epoch;

-- name: ListExpiredLeases :many
-- The leases whose holders stopped heartbeating.
--
-- QUARANTINE IS EXCLUDED because it is already past this line: its compute is
-- unconfirmed and its capacity stays charged until something proves otherwise, so
-- reaping it again would be the reaper deciding a question only evidence settles.
--
-- EVERYTHING THE ARCHIVE COPIES IS SELECTED, chosen_provider included. The
-- reaper terminalizes an expired escrow outright and archives it from this row,
-- and a projection that left the provider out archived every reaped lease as
-- having run on nothing.
SELECT id, tier, node, target_node, macos_slot, chosen_provider, phase, vcpu, memory,
       requested_vcpu, requested_memory, instance_type, site, price_micros_per_hour,
       image_cache, cache_generation, actions_cache, held_at, force_release,
       holder_incarnation, failure_reason, disruption, disrupted_at, epoch, run_id,
       request_id
  FROM leases
 WHERE phase NOT IN ('done','failed','quarantine') AND expires_at <= @cutoff
 ORDER BY expires_at
 LIMIT CAST(@max_rows AS BIGINT);

-- name: ReadLeaseCharge :one
-- What one lease has charged the fleet since it was escrowed: the host it is
-- charged to, the shape, and when the charge began.
--
-- FROM ESCROW, NOT ASSIGNMENT. A lease is charged the moment it is inserted,
-- while it is still a discovery slot nobody has been given, and job_history
-- opens its row only at assignment; a reader auditing what a host carried at
-- an instant has to start the interval here. COALESCE(node, target_node) is
-- the host the arithmetic charges, as everywhere else.
SELECT COALESCE(node, target_node, '') AS host, vcpu, memory, created_at
  FROM leases WHERE id = @id;

-- name: ReadLease :one
-- One lease, whatever phase it is in.
SELECT id, tier, node, target_node, macos_slot, guest_os, providers, chosen_provider,
       phase, vcpu, memory, requested_vcpu, requested_memory, instance_type, site,
       price_micros_per_hour, image_cache, cache_generation, actions_cache,
       held_at, force_release, holder_incarnation, failure_reason, disruption,
       disrupted_at, epoch, run_id, request_id
  FROM leases WHERE id = @id;

-- name: ReadLeaseJob :one
-- The tier and job identity one lease carries.
SELECT tier, run_id, request_id FROM leases WHERE id = @id;

-- name: ReadLeaseClosure :one
-- Whether one lease has finished, and when the ledger closed it.
--
-- THE ONE QUESTION THAT MAY AUTHORISE DELETING A STAGED REGISTRATION. A CodeBuild
-- runner's JIT configuration outlives its build in Parameter Store, and from the
-- provider alone "no build for this lease" and "the build has not appeared yet"
-- are the same observation. What separates them is this row: a terminal phase,
-- closed longer ago than any build could still be running. sql.ErrNoRows is a
-- lease the ledger has never heard of, which the caller reports and never acts on.
--
-- finished_at comes from job_history, which every terminalization writes; the
-- COALESCE is for a row that predates the column, which reads as "closed at an
-- unknown time" and is therefore never old enough.
SELECT l.phase, CAST(COALESCE(h.finished_at, '') AS TEXT) AS finished_at
  FROM leases l
  LEFT JOIN job_history h ON h.lease_id = l.id
 WHERE l.id = @id;

-- name: TotalUsage :one
-- What the whole deployment has committed.
SELECT CAST(COALESCE(SUM(vcpu), 0) AS BIGINT) AS vcpu,
       CAST(COALESCE(SUM(memory), 0) AS BIGINT) AS memory,
       CAST(COUNT(*) AS BIGINT) AS leases
  FROM leases WHERE phase NOT IN ('done','failed');

-- name: CountActiveRunnerLeases :one
-- How many runners GitHub could still route a job to, in one tier.
--
-- KEYED ON deregistered RATHER THAN ON PHASE, because the question is whether the
-- GitHub registration is gone rather than what billet's compute lifecycle is
-- doing: a teardown still retrying its destroy is deregistered and must stop
-- counting, while a teardown whose runner was never deregistered must keep
-- counting or a replacement is launched against a runner GitHub can still
-- schedule.
SELECT CAST(COUNT(*) AS BIGINT) FROM leases
 WHERE tier = @tier AND phase NOT IN ('capacity','done','failed') AND deregistered = 0;

-- name: ListServiceableRunnerLeaseIDs :many
-- The leases in one tier whose compute a restart should re-adopt.
--
-- RETIRING AND RETIRED POOL MEMBERS ARE EXCLUDED: their registration is being
-- removed or is gone, so re-adopting them would hold capacity for a runner
-- nothing can route to.
SELECT leases.id FROM leases
 WHERE leases.tier = @tier
   AND leases.phase IN ('launching','online','busy','custody')
   AND NOT EXISTS (SELECT 1 FROM pool_runners
                    WHERE pool_runners.lease_id = leases.id
                      AND pool_runners.status IN ('retiring','retired'))
 ORDER BY leases.id;

-- name: ListLeaseIDsOnNode :many
-- The leases one host is running compute for.
SELECT id FROM leases
 WHERE node = @node
   AND phase IN (@launching, @online, @busy, @custody, @teardown);

-- name: CountOpenPerTier :many
-- How much each tier already holds, for the floor arithmetic.
--
-- A LEASE ON A HOST THAT IS GONE COUNTS FOR NOTHING, which is what the LEFT JOIN
-- and the liveness test express: a floor is about capacity a tier can actually
-- use. An UNPLACED lease still counts, because escrow has promised it.
--
-- THAT UNPLACED CLAUSE IS UNREACHABLE THROUGH TODAY'S API, and that is said here
-- rather than left for somebody to discover: every reservation names a machine
-- now -- Reserve refuses when placement finds no host, so insertLease always has
-- a target -- and only a row written before that was true has an empty one.
-- Measured: deleting the clause fails no test in internal/alloc. It stays because
-- such rows exist in deployments upgraded from before, and dropping them from a
-- tier's committed count understates what that tier already holds.
SELECT l.tier, CAST(COUNT(*) AS BIGINT) AS open_leases
  FROM leases l
  LEFT JOIN nodes n ON n.name = COALESCE(l.node, l.target_node)
 WHERE l.phase NOT IN ('done','failed')
   AND (COALESCE(l.node, l.target_node, '') = ''
        OR (n.live = 1 AND n.drained = 0))
 GROUP BY l.tier;

-- name: CountOpenInTier :one
-- Everything one tier holds, live host or not.
SELECT CAST(COUNT(*) AS BIGINT) FROM leases
 WHERE tier = @tier AND phase NOT IN ('done','failed');
