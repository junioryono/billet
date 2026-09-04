"""Route an EC2 Spot interruption warning to the billet node that owns the
instance.

EventBridge cannot match on an instance tag, and a Spot interruption warning
carries only the instance id and action — so a rule can deliver the warning here
but cannot pick the queue. This looks up the instance's sh.billet.node tag and
forwards the warning to the SQS queue that node polls (billet requires the queue's
name to equal the node name). An instance without the tag is not billet's, so it
is dropped rather than forwarded — which keeps a node from ever seeing a foreign
warning it would refuse and re-queue (the poison-ack churn the README warns about).

ERROR CLASSIFICATION MATTERS because the warning has a ~2-minute life, so the only
thing that may consume one SILENTLY is a positive proof it is not this router's to
place: an instance that is present and carries no tag, an instance EC2 says is
already gone, a tag that cannot name a queue, or a tag naming a queue outside the
set this router serves (BILLET_INTERRUPTION_QUEUE_NAMES, set by the module to
every queue it created, comma-separated) THAT SQS then answers AccessDenied or
NonExistentQueue for. That last one takes both halves: a queue this router is
granted but was not told about still forwards. Everything else is a could-not-tell
and is re-raised, so Lambda retries and the failure appears on the function's
Errors metric — where the module's alarm reports it — rather than a real warning
disappearing.

AccessDenied in particular is what AWS answers BOTH for a queue outside this
router's grant AND for a grant that is absent, stale, or has not propagated yet,
which is exactly the state a fresh apply, a new spot_node_names entry or a policy
edit leaves it in. Reading it as the first alone dropped a real warning at the
moment the node most needed it; the served set is what tells the two apart, and
the module derives it from the same list it scopes the grant to.
"""

import json
import os
import re

import boto3
from botocore.exceptions import ClientError

NODE_TAG = "sh.billet.node"

# THE QUEUES THIS ROUTER SERVES, set by the module to every queue it created,
# comma-separated (a queue name cannot contain a comma). Unset or empty is "cannot
# tell whose queue this is", which is the safe value: it drops nothing.
QUEUE_NAMES_ENV = "BILLET_INTERRUPTION_QUEUE_NAMES"

# SQS queue names are up to 80 characters of alphanumerics, hyphens and
# underscores. A tag that is not one cannot name a queue, so drop it without an
# API call (which would only return an InvalidAddress error anyway).
_QUEUE_NAME = re.compile(r"\A[A-Za-z0-9_-]{1,80}\Z")

# The two codes that end a warning for a queue this router does NOT serve: there
# is nowhere to place it and nothing saying the queue is reachable. Not "permanent"
# — a fresh grant's AccessDenied clears when it propagates — which is exactly why
# they may never be read this way about a queue in the served set, where both are
# a could-not-tell.
_FOREIGN_QUEUE_ERRORS = {"AWS.SimpleQueueService.NonExistentQueue", "AccessDenied"}

_ec2 = boto3.client("ec2")
_sqs = boto3.client("sqs")


def handler(event, _context):
    instance_id = event.get("detail", {}).get("instance-id")
    if not instance_id:
        # NOT a drop. Every interruption warning carries the instance id, and the
        # rule that invokes this delivers nothing else, so an event without one is a
        # shape this router cannot read rather than a warning it can prove is not
        # its own. Raising makes an EventBridge or AWS schema change an alarm
        # instead of every warning in the account quietly disappearing.
        raise RuntimeError("event carries no detail.instance-id; cannot route it")

    node = _node_of(instance_id)  # raises unless it can say which of the two it is
    if node is None:
        # None is the two proofs _node_of can return: untagged, or already gone
        # (where it has said so itself). A tag that is PRESENT and unusable is not
        # one of them and falls through to the queue-name check below, which names
        # the tag rather than reporting a tagged instance as untagged.
        print(f"{instance_id} names no billet node; nothing to route, dropping")
        return
    if not _QUEUE_NAME.match(node):
        print(f"{instance_id} {NODE_TAG}={node!r} is not a legal queue name; dropping")
        return

    # A TAG OUTSIDE THE SERVED SET IS STILL LOOKED UP, never short-circuited on the
    # name: a queue this router is granted but was not told about forwards
    # successfully. The set only decides what a FAILURE means.
    try:
        url = _sqs.get_queue_url(QueueName=node)["QueueUrl"]
    except ClientError as err:
        if _drop(f"get_queue_url({node!r})", err, node):
            return
        raise  # a could-not-tell — let Lambda retry rather than lose the warning

    # Forward the ORIGINAL EventBridge event: the node parses detail-type, source
    # and detail.instance-id/instance-action from exactly this shape.
    try:
        _sqs.send_message(QueueUrl=url, MessageBody=json.dumps(event))
    except ClientError as err:
        if _drop(f"send_message to {node!r}", err, node):
            return
        raise

    print(f"forwarded interruption for {instance_id} to node {node!r}")


def _drop(what, err, node):
    """Whether an SQS ClientError may be dropped (True) rather than re-raised, which
    takes a POSITIVE proof the warning is not this router's to place: the tag names a
    queue outside the set it serves, AND SQS answered one of the two codes that end
    it for such a queue. A served queue's refusals and an unconfigured served set
    are could-not-tells, and re-raising one costs a retry where dropping it costs a
    real two-minute warning."""
    code = err.response.get("Error", {}).get("Code", "")
    served = _served()
    if served and node not in served and code in _FOREIGN_QUEUE_ERRORS:
        print(f"{what} failed ({code}) for {node!r}, which is not one of the queues this "
              f"router serves ({', '.join(sorted(served))}); dropping")
        return True

    print(f"{what} failed ({code}); this router cannot place the warning and cannot "
          f"prove the queue is not its own, so Lambda will retry it")
    return False


def _served():
    """The set of queue names this router serves, from the module-set environment.
    MEMBERSHIP, not a substring match: "build-1" served must not make "build" look
    served. Whitespace and empty entries are ignored, so an empty variable is the
    empty set — the value that drops nothing."""
    raw = os.environ.get(QUEUE_NAMES_ENV, "")
    return frozenset(name.strip() for name in raw.split(",") if name.strip())


def _node_of(instance_id):
    """Return the instance's sh.billet.node tag value, or None on one of the two
    proofs that there is nothing of billet's to route: the instance is PRESENT and
    carries no such tag, or EC2 says it no longer exists. An answer that holds
    neither — a retryable error, or a success that does not contain the instance
    this call named — is a could-not-tell and raises.

    A tag that is present with an empty or missing value comes back as "", not None:
    it is unusable, but the instance IS billet's, and the caller says so."""
    try:
        described = _ec2.describe_instances(InstanceIds=[instance_id])
    except ClientError as err:
        code = err.response.get("Error", {}).get("Code", "")
        if code == "InvalidInstanceID.NotFound":
            # Reclaimed before this ran; there is nothing left to route.
            print(f"{instance_id} is already gone; dropping")
            return None
        raise  # throttling / service error — let Lambda retry

    # The instance is IDENTIFIED rather than taken as whatever came first. The call
    # named one instance and AWS answers with that one, but "the answer's first
    # entry" and "the instance this warning is about" are two different claims, and
    # the drop below is a proof about the second.
    for reservation in described.get("Reservations", []):
        for instance in reservation.get("Instances", []):
            if instance.get("InstanceId") != instance_id:
                continue
            for tag in instance.get("Tags", []):
                if tag.get("Key") == NODE_TAG:
                    return tag.get("Value") or ""
            return None  # present, and carrying no billet tag: not billet's

    # An answer without this instance is a THIRD state, and collapsing it into "no
    # tag" would drop a warning for an instance that may well be billet's. A retry
    # either finds the instance or is told it is gone, and both of those are answers.
    raise RuntimeError(
        f"describe_instances({instance_id}) returned no instance and no error; "
        f"cannot tell whether it is billet's")
