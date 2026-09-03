"""Route an EC2 Spot interruption warning to the billet node that owns the
instance.

EventBridge cannot match on an instance tag, and a Spot interruption warning
carries only the instance id and action — so a rule can deliver the warning here
but cannot pick the queue. This looks up the instance's sh.billet.node tag and
forwards the warning to the SQS queue that node polls (billet requires the queue's
name to equal the node name). An instance without the tag is not billet's, so it
is dropped rather than forwarded — which keeps a node from ever seeing a foreign
warning it would refuse and re-queue (the poison-ack churn the README warns about).

ERROR CLASSIFICATION MATTERS because the warning has a ~2-minute life: a PERMANENT
failure (the instance already reclaimed, no such queue, a queue outside this
router's grant, a tag that is not a legal queue name) is dropped, but a RETRYABLE
failure (throttling, a 5xx) is re-raised so Lambda's asynchronous retry gets
another chance rather than silently losing a real warning.
"""

import json
import re

import boto3
from botocore.exceptions import ClientError

NODE_TAG = "sh.billet.node"

# SQS queue names are up to 80 characters of alphanumerics, hyphens and
# underscores. A tag that is not one cannot name a queue, so drop it without an
# API call (which would only return an InvalidAddress error anyway).
_QUEUE_NAME = re.compile(r"\A[A-Za-z0-9_-]{1,80}\Z")

# Permanent get_queue_url failures: retrying cannot fix them. AccessDenied means
# the named queue is outside this router's grant (another deployment's), which is a
# configuration fact, not a transient fault.
_PERMANENT_QUEUE_ERRORS = {"AWS.SimpleQueueService.NonExistentQueue", "AccessDenied"}

_ec2 = boto3.client("ec2")
_sqs = boto3.client("sqs")


def handler(event, _context):
    instance_id = event.get("detail", {}).get("instance-id")
    if not instance_id:
        print("event has no instance-id; dropping")
        return

    node = _node_of(instance_id)  # raises on a retryable EC2 error
    if not node:
        print(f"{instance_id} has no {NODE_TAG} tag; not a billet instance, dropping")
        return
    if not _QUEUE_NAME.match(node):
        print(f"{instance_id} {NODE_TAG}={node!r} is not a legal queue name; dropping")
        return

    try:
        url = _sqs.get_queue_url(QueueName=node)["QueueUrl"]
    except ClientError as err:
        if _drop(f"get_queue_url({node!r})", err):
            return
        raise  # retryable (throttling / 5xx) — let Lambda retry within the window

    # Forward the ORIGINAL EventBridge event: the node parses detail-type, source
    # and detail.instance-id/instance-action from exactly this shape.
    try:
        _sqs.send_message(QueueUrl=url, MessageBody=json.dumps(event))
    except ClientError as err:
        if _drop(f"send_message to {node!r}", err):
            return
        raise

    print(f"forwarded interruption for {instance_id} to node {node!r}")


def _drop(what, err):
    """Whether an SQS ClientError is a PERMANENT failure to drop (True) rather than
    a retryable one to re-raise (False)."""
    code = err.response.get("Error", {}).get("Code", "")
    if code in _PERMANENT_QUEUE_ERRORS:
        print(f"{what} permanently failed ({code}); dropping")
        return True
    return False


def _node_of(instance_id):
    """Return the instance's sh.billet.node tag value, or None if it has none or
    the instance is already gone. Re-raises a retryable EC2 error."""
    try:
        described = _ec2.describe_instances(InstanceIds=[instance_id])
    except ClientError as err:
        code = err.response.get("Error", {}).get("Code", "")
        if code == "InvalidInstanceID.NotFound":
            # Reclaimed before this ran; there is nothing left to route.
            print(f"{instance_id} is already gone; dropping")
            return None
        raise  # throttling / service error — let Lambda retry

    for reservation in described.get("Reservations", []):
        for instance in reservation.get("Instances", []):
            for tag in instance.get("Tags", []):
                if tag.get("Key") == NODE_TAG:
                    return tag.get("Value")
    return None
