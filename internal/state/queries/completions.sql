-- name: PendingCompletionMessage :one
-- The message id and retirement of an obligation already on file.
--
-- Read INSIDE the same transaction as the upsert below, because the disposition
-- it decides (stale, retired, or actionable) describes the row the upsert is
-- about to act on.
SELECT message_id, retired FROM pending_completions
 WHERE tier = @tier AND request_id = @request_id;

-- name: UpsertPendingCompletion :exec
-- Durably record a result-delivery obligation.
--
-- THE CASE ARMS ARE THE REDELIVERY RULE, and they are not decoration. A
-- redelivery of the SAME message must not undo settlement that has already
-- happened: once a row is retired, or once it has been narrowed to a release
-- only, the lease fields and outcome on file win over what the delivery carries.
-- A LATER message (a higher message_id) replaces them outright, and the trailing
-- WHERE drops an EARLIER one entirely.
--
-- release_only, retired and acknowledged are max()'d for the same message so a
-- redelivery can only ever move them forward.
INSERT INTO pending_completions
	(tier, request_id, run_id, result, lease_id, lease_epoch, lease_node, outcome,
	 release_only, message_id, retired, acknowledged)
	VALUES (@tier, @request_id, @run_id, @result, @lease_id, @lease_epoch, @lease_node,
	        @outcome, @release_only, @message_id, @retired, @acknowledged)
	ON CONFLICT(tier, request_id) DO UPDATE SET
		run_id=excluded.run_id,
		result=excluded.result,
		lease_id=CASE WHEN excluded.message_id = pending_completions.message_id
			AND (pending_completions.retired = 1 OR
			     pending_completions.release_only > excluded.release_only)
			THEN pending_completions.lease_id ELSE excluded.lease_id END,
		lease_epoch=CASE WHEN excluded.message_id = pending_completions.message_id
			AND (pending_completions.retired = 1 OR
			     pending_completions.release_only > excluded.release_only)
			THEN pending_completions.lease_epoch ELSE excluded.lease_epoch END,
		lease_node=CASE WHEN excluded.message_id = pending_completions.message_id
			AND (pending_completions.retired = 1 OR
			     pending_completions.release_only > excluded.release_only)
			THEN pending_completions.lease_node ELSE excluded.lease_node END,
		outcome=CASE WHEN excluded.message_id = pending_completions.message_id
			AND (pending_completions.retired = 1 OR
			     pending_completions.release_only > excluded.release_only)
			THEN pending_completions.outcome ELSE excluded.outcome END,
		release_only=CASE
			WHEN excluded.message_id = pending_completions.message_id
			THEN CASE WHEN pending_completions.release_only > excluded.release_only
			          THEN pending_completions.release_only ELSE excluded.release_only END
			ELSE excluded.release_only END,
		message_id=excluded.message_id,
		retired=CASE
			WHEN excluded.message_id = pending_completions.message_id
			THEN CASE WHEN pending_completions.retired > excluded.retired
			          THEN pending_completions.retired ELSE excluded.retired END
			ELSE excluded.retired END,
		acknowledged=CASE
			WHEN excluded.message_id = pending_completions.message_id
			THEN CASE WHEN pending_completions.acknowledged > excluded.acknowledged
			          THEN pending_completions.acknowledged ELSE excluded.acknowledged END
			ELSE excluded.acknowledged END
	WHERE excluded.message_id >= pending_completions.message_id;

-- name: RetirePendingCompletion :exec
-- Make replay a no-op BEFORE deletion is attempted.
--
-- Scoped to the message id: a redelivery that arrives as a later message is a
-- new obligation, not one this settlement covers.
UPDATE pending_completions SET retired = 1
 WHERE tier = @tier AND request_id = @request_id AND message_id = @message_id;

-- name: DeleteAcknowledgedCompletion :exec
-- Remove an obligation that is both retired and acknowledged.
--
-- Paired with RetirePendingCompletion in one transaction: the row may only go
-- once GitHub will not redeliver it AND settlement is durable, and whichever of
-- the two happens second is what removes it.
DELETE FROM pending_completions
 WHERE tier = @tier AND request_id = @request_id AND message_id = @message_id
   AND acknowledged = 1;

-- name: AcknowledgePendingCompletion :exec
-- Record that GitHub will not redeliver this message.
UPDATE pending_completions SET acknowledged = 1
 WHERE tier = @tier AND request_id = @request_id AND message_id = @message_id;

-- name: DeleteRetiredCompletion :exec
-- The other half of the pair above, from the acknowledgement side.
DELETE FROM pending_completions
 WHERE tier = @tier AND request_id = @request_id AND message_id = @message_id
   AND retired = 1;

-- name: ListPendingCompletions :many
-- One tier's outstanding obligations.
SELECT tier, request_id, run_id, result, lease_id, lease_epoch, lease_node,
       outcome, release_only, message_id, retired, acknowledged
  FROM pending_completions WHERE tier = @tier ORDER BY request_id;
