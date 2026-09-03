-- Pool runners: one GitHub registration backed by one compute lease.
--
-- A SCALE-SET RUNNER IS A POOL MEMBER, NOT THE JOB THAT CAUSED SCALE-UP, and
-- this table is where that distinction is made durable. launch_request_id is the
-- request billet launched for; actual_request_id, run_id and job_id are the job
-- GitHub's own JobStarted says the runner actually consumed. Those two are
-- routinely different, and the second set is authoritative about the compute.
--
-- THE PROJECTIONS INCLUDE updated_at, which nothing reads, and that is
-- deliberate: selecting exactly the table's columns in exactly its order makes
-- sqlc return the model struct, so all three reads share one type and one
-- mapping function rather than three Row types that must be kept identical.

-- name: InsertPoolRunner :exec
-- Record one pool member.
--
-- THE STATUS IS A PARAMETER because a member is journaled at three different
-- points: idle when billet registers what it launched, busy when recovery finds
-- a legacy registration GitHub says is working, and retiring when recovery is
-- claiming one for teardown and no row existed. The table's CHECK is what keeps
-- the vocabulary closed.
INSERT INTO pool_runners
     (lease_id, tier, launch_request_id, runner_id, runner_name, status, updated_at)
VALUES (@lease_id, @tier, @launch_request_id, @runner_id, @runner_name, @status,
        @updated_at);

-- name: ReadPoolRunnerByLease :one
-- The pool member backed by one lease.
--
-- THE COMPUTE AUTHORITY WHEN NO MESSAGE CARRIES A RUNNER NAME. Read inside the
-- write transaction by every mutation below, against the row that write acts on.
SELECT lease_id, tier, launch_request_id, runner_id, runner_name, status,
       actual_request_id, run_id, job_id, source_acknowledged, updated_at
  FROM pool_runners WHERE lease_id = @lease_id;

-- name: ReadPoolRunnerByName :one
-- GitHub's runner identity resolved to billet's compute lease.
SELECT lease_id, tier, launch_request_id, runner_id, runner_name, status,
       actual_request_id, run_id, job_id, source_acknowledged, updated_at
  FROM pool_runners WHERE runner_name = @runner_name;

-- name: ListPoolRunnersInTier :many
-- Every durable member of one tier's GitHub runner pool.
SELECT lease_id, tier, launch_request_id, runner_id, runner_name, status,
       actual_request_id, run_id, job_id, source_acknowledged, updated_at
  FROM pool_runners WHERE tier = @tier ORDER BY updated_at, lease_id;

-- name: ReadPoolRunnerSettlementByRequest :one
-- What settling one launch request needs to know.
--
-- ONE STATEMENT FOR BOTH SETTLEMENT PATHS. A completion settles the physical
-- runner and an acknowledgement records that GitHub cannot redeliver; each reads
-- two of these three columns, and splitting them into two queries would be two
-- statements that must agree about how a request identifies a member.
SELECT lease_id, status, source_acknowledged FROM pool_runners
 WHERE tier = @tier AND launch_request_id = @launch_request_id;

-- name: BindPoolRunnerJob :exec
-- Fill in the job a recovered busy member turned out to be running.
--
-- IT DOES NOT TOUCH status OR runner_id, and the reason is narrower than it
-- looks: the caller reads the row and reaches this in ONE transaction, so a
-- status written here could not differ from the one it just read. What the
-- narrow projection buys is that the statement cannot be reused, later, on a
-- path that has not established the member is busy -- measured, adding
-- `status = 'busy'` here changes no observable behaviour today.
UPDATE pool_runners
   SET actual_request_id = @actual_request_id, run_id = @run_id, job_id = @job_id,
       updated_at = @updated_at
 WHERE lease_id = @lease_id;

-- name: StartPoolRunner :exec
-- Bind an idle member to the job GitHub gave it.
UPDATE pool_runners
   SET runner_id = @runner_id, status = 'busy', actual_request_id = @actual_request_id,
       run_id = @run_id, job_id = @job_id, updated_at = @updated_at
 WHERE lease_id = @lease_id;

-- name: MarkPoolRunnerBusy :exec
-- Journal a recovered legacy registration as working, without a job identity.
--
-- THE EMPTY JOB FIELDS ARE A RESERVATION FOR THIS EXACT PHYSICAL RUNNER, not a
-- competing binding, which is why they are left alone rather than zeroed: a
-- delayed JobStarted fills them through BindPoolRunnerJob.
UPDATE pool_runners
   SET runner_id = @runner_id, status = 'busy', updated_at = @updated_at
 WHERE lease_id = @lease_id;

-- name: MarkPoolRunnerRetiring :exec
-- Claim a member for teardown, having already decided it may be claimed.
UPDATE pool_runners SET status = 'retiring', updated_at = @updated_at
 WHERE lease_id = @lease_id;

-- name: ClaimPoolRunnerForRetirement :execresult
-- Claim a member for teardown, refusing one that is already past it.
--
-- THE STATUS FILTER IS THE CLAIM, so this is :execresult: zero rows affected is
-- how the caller learns the member was already retiring, retired, or gone, and
-- it then reads the row to say which. An :exec could not tell those apart.
UPDATE pool_runners SET status = 'retiring', updated_at = @updated_at
 WHERE lease_id = @lease_id AND status IN ('idle','busy');

-- name: MarkPoolRunnerRetired :exec
-- Record that compute settled while GitHub may still redeliver the completion.
--
-- THE ROW SURVIVES, which is the point: a redelivery must resolve to the same
-- compute even after teardown and capacity release have both completed.
UPDATE pool_runners SET status = 'retired', updated_at = @updated_at
 WHERE lease_id = @lease_id;

-- name: AcknowledgePoolRunnerSource :exec
-- Record that GitHub cannot redeliver the completion that used this member.
UPDATE pool_runners SET source_acknowledged = 1, updated_at = @updated_at
 WHERE lease_id = @lease_id;

-- name: DeletePoolRunner :exec
-- Forget the settlement metadata, after compute and capacity are both gone.
DELETE FROM pool_runners WHERE lease_id = @lease_id;
