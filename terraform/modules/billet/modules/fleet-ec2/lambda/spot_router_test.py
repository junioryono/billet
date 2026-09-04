"""Unit tests for the spot-interruption router Lambda.

boto3/botocore are provided by the AWS Lambda runtime, not installed in CI, so
they are stubbed here before the handler is imported — the tests exercise the
routing and error-CLASSIFICATION logic, which a Terraform plan test cannot reach.
Run: python3 -m unittest terraform/modules/billet/modules/fleet-ec2/lambda/spot_router_test.py
"""

import json
import os
import re
import sys
import types
import unittest
from unittest import mock


class _ClientError(Exception):
    def __init__(self, code):
        super().__init__(code)
        self.response = {"Error": {"Code": code}}


_ec2 = mock.MagicMock()
_sqs = mock.MagicMock()

_boto3 = types.ModuleType("boto3")
_boto3.client = lambda name: {"ec2": _ec2, "sqs": _sqs}[name]
sys.modules["boto3"] = _boto3

_botocore = types.ModuleType("botocore")
_botocore_exceptions = types.ModuleType("botocore.exceptions")
_botocore_exceptions.ClientError = _ClientError
_botocore.exceptions = _botocore_exceptions
sys.modules["botocore"] = _botocore
sys.modules["botocore.exceptions"] = _botocore_exceptions

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
import spot_router  # noqa: E402

# The queue the module creates and grants this router, as the tests deploy it.
OWN_QUEUE = "billet-spot-interruptions"


def _warning(instance_id="i-abc"):
    return {
        "source": "aws.ec2",
        "detail-type": "EC2 Spot Instance Interruption Warning",
        "detail": {"instance-id": instance_id, "instance-action": "terminate"},
    }


def _tagged(node):
    tags = [] if node is None else [{"Key": spot_router.NODE_TAG, "Value": node}]
    return {"Reservations": [{"Instances": [{"Tags": tags}]}]}


class SpotRouterTest(unittest.TestCase):
    def setUp(self):
        _ec2.reset_mock(side_effect=True, return_value=True)
        _sqs.reset_mock(side_effect=True, return_value=True)
        # Every case runs with the environment the module actually deploys, so a
        # test that wants the unconfigured case has to say so.
        self._serve(OWN_QUEUE)

    def _serve(self, name):
        """Deploy this test's router as serving `name`, or nothing when it is None.

        patch.dict restores the whole environment at cleanup, so removing the key
        inside it is undone too — and cleanups run last-in-first-out, so a test
        that re-serves over setUp's value unwinds in the right order."""
        patch = mock.patch.dict(os.environ, {})
        patch.start()
        self.addCleanup(patch.stop)
        if name is None:
            os.environ.pop(spot_router.QUEUE_NAME_ENV, None)
        else:
            os.environ[spot_router.QUEUE_NAME_ENV] = name

    def test_forwards_to_the_owning_node_queue(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}

        event = _warning("i-xyz")
        spot_router.handler(event, None)

        _sqs.get_queue_url.assert_called_once_with(QueueName=OWN_QUEUE)
        _sqs.send_message.assert_called_once()
        kwargs = _sqs.send_message.call_args.kwargs
        self.assertEqual(kwargs["QueueUrl"], "https://sqs/q")
        # The forwarded body is the ORIGINAL event the node parses.
        self.assertEqual(json.loads(kwargs["MessageBody"]), event)

    def test_drops_an_untagged_instance(self):
        _ec2.describe_instances.return_value = _tagged(None)
        spot_router.handler(_warning(), None)
        _sqs.get_queue_url.assert_not_called()
        _sqs.send_message.assert_not_called()

    def test_drops_an_event_without_instance_id(self):
        spot_router.handler({"detail": {}}, None)
        _ec2.describe_instances.assert_not_called()
        _sqs.send_message.assert_not_called()

    def test_drops_a_malformed_tag_without_calling_sqs(self):
        _ec2.describe_instances.return_value = _tagged("not a legal queue name!")
        spot_router.handler(_warning(), None)
        _sqs.get_queue_url.assert_not_called()
        _sqs.send_message.assert_not_called()

    def test_drops_when_the_instance_is_already_gone(self):
        _ec2.describe_instances.side_effect = _ClientError("InvalidInstanceID.NotFound")
        spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_reraises_a_retryable_describe_error(self):
        _ec2.describe_instances.side_effect = _ClientError("Throttling")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)

    # A QUEUE THIS ROUTER DOES NOT SERVE is another deployment's, and the tag saying
    # so is the proof that lets a permanent failure be consumed silently.

    def test_drops_access_denied_for_a_queue_it_does_not_serve(self):
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_drops_a_nonexistent_queue_it_does_not_serve(self):
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.side_effect = _ClientError(
            "AWS.SimpleQueueService.NonExistentQueue")
        spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_drops_a_permanent_send_failure_to_a_queue_it_does_not_serve(self):
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/theirs"}
        _sqs.send_message.side_effect = _ClientError("AccessDenied")
        spot_router.handler(_warning(), None)  # must not raise

    # A QUEUE THIS ROUTER DOES NOT SERVE IS STILL LOOKED UP. A deployment with
    # several spot queues extends the router's grant to them, and those forward.
    # Turning the name comparison into a short-circuit would break that silently,
    # so the name may decide what a FAILURE means and nothing else.
    def test_forwards_to_a_queue_it_does_not_serve_when_the_grant_allows_it(self):
        _ec2.describe_instances.return_value = _tagged("another-billet-node")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/other"}

        spot_router.handler(_warning(), None)

        _sqs.get_queue_url.assert_called_once_with(QueueName="another-billet-node")
        self.assertEqual(_sqs.send_message.call_args.kwargs["QueueUrl"],
                         "https://sqs/other")

    # ITS OWN QUEUE. Every refusal about the queue this router is supposed to be
    # able to reach is a could-not-tell — AccessDenied is equally the answer for an
    # absent, stale or unpropagated grant — so it is re-raised for Lambda's retry
    # rather than consuming a real two-minute warning.

    def test_reraises_access_denied_to_its_own_queue(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_reraises_when_its_own_queue_does_not_exist(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.side_effect = _ClientError(
            "AWS.SimpleQueueService.NonExistentQueue")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_reraises_access_denied_sending_to_its_own_queue(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}
        _sqs.send_message.side_effect = _ClientError("AccessDenied")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)

    def test_reraises_a_retryable_queue_error(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.side_effect = _ClientError("ServiceUnavailable")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)

    def test_reraises_a_retryable_send_message_failure(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}
        _sqs.send_message.side_effect = _ClientError("Throttling")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)

    # AND WITH NO QUEUE NAME AT ALL the router cannot tell whose queue a tag names,
    # so it drops nothing: the unset environment is the safe value, not a licence to
    # fall back on the error code alone.
    def test_reraises_access_denied_when_no_queue_is_configured(self):
        self._serve(None)
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)

    def test_queue_name_regex_boundaries(self):
        self.assertTrue(spot_router._QUEUE_NAME.match("a"))
        self.assertTrue(spot_router._QUEUE_NAME.match("A_b-9"))
        self.assertTrue(spot_router._QUEUE_NAME.match("q" * 80))
        self.assertIsNone(spot_router._QUEUE_NAME.match(""))
        self.assertIsNone(spot_router._QUEUE_NAME.match("q" * 81))
        self.assertIsNone(spot_router._QUEUE_NAME.match("has.dot"))
        self.assertIsNone(spot_router._QUEUE_NAME.match("has space"))
        self.assertIsNone(spot_router._QUEUE_NAME.match("trailing\n"))


# THE HANDLER AND THE MODULE HAVE TO NAME THE SAME VARIABLE, and nothing else in
# either gate says so: a rename on one side leaves the router with no queue name,
# which is safe (it drops nothing) and wrong (every foreign warning becomes a failed
# invocation). The value has to be the created queue's own attribute, not the name
# rebuilt from var.name, so a queue renamed in one place cannot leave the other
# behind.
class SpotRouterModuleWiringTest(unittest.TestCase):
    def test_the_module_passes_the_created_queue_as_the_variable_the_handler_reads(self):
        with open(os.path.join(_HERE, "..", "spot.tf"), encoding="utf-8") as f:
            spot_tf = f.read()

        assignment = re.search(
            r"^\s*(\S+)\s*=\s*aws_sqs_queue\.interruptions\[0\]\.name\s*$",
            spot_tf,
            re.MULTILINE,
        )
        self.assertIsNotNone(
            assignment,
            "spot.tf must pass aws_sqs_queue.interruptions[0].name into the router's "
            "environment; without it the handler cannot tell a foreign queue from its "
            "own missing grant",
        )
        self.assertEqual(assignment.group(1), spot_router.QUEUE_NAME_ENV)


if __name__ == "__main__":
    unittest.main()
