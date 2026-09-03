-- Durable negative scheduler identities.
--
-- ZERO IS NEVER A SCHEDULER IDENTITY. GitHub's direct JobAssigned path carries
-- runnerRequestId 0, and its desired-count signal names a quantity rather than a
-- job at all -- so billet mints its own id for both. Negative, so it can never
-- collide with one of GitHub's own positive request ids, and allocated under the
-- immediate writer transaction so a redelivery or a restart recovers the SAME
-- number rather than minting a second identity for one job.
--
-- HASHING THE JOB ID WAS THE ALTERNATIVE AND IT IS WORSE: it would make hash
-- collisions a correctness property of the scheduler.
--
-- THE MINIMUM IS TAKEN ACROSS BOTH TABLES, which is what keeps the two families
-- of identity in one number space. Reading only its own table would let a job and
-- a pool slot be handed the same id.

-- name: InsertJobIdentity :exec
-- Mint the identity for one direct-assignment job, if it has none.
--
-- DO NOTHING rather than DO UPDATE: a job that already has an identity must keep
-- it, because that number is what an in-flight message is keyed on.
INSERT INTO job_identities (job_id, internal_id)
SELECT @job_id, COALESCE(MIN(internal_id), 0) - 1
FROM (SELECT internal_id FROM job_identities
      UNION ALL SELECT internal_id FROM pool_slot_identities)
WHERE true
ON CONFLICT(job_id) DO NOTHING;

-- name: ReadJobIdentity :one
-- The identity one job holds.
--
-- READ AFTER THE INSERT RATHER THAN RETURNED BY IT, because the insert may have
-- done nothing: the identity that matters is whatever the row holds now, which
-- is not necessarily the number this call would have minted.
SELECT internal_id FROM job_identities WHERE job_id = @job_id;

-- name: InsertPoolSlotIdentity :exec
-- Mint the identity for one escrowed lease becoming a physical pool member.
--
-- KEYED ON THE LEASE, because GitHub's desired-count signal names no job: the
-- lease is the only stable identity available on redelivery and after a restart.
INSERT INTO pool_slot_identities (lease_id, internal_id)
SELECT @lease_id, COALESCE(MIN(internal_id), 0) - 1
FROM (SELECT internal_id FROM job_identities
      UNION ALL SELECT internal_id FROM pool_slot_identities)
WHERE true
ON CONFLICT(lease_id) DO NOTHING;

-- name: ReadPoolSlotIdentity :one
-- The identity one lease's pool slot holds.
SELECT internal_id FROM pool_slot_identities WHERE lease_id = @lease_id;
