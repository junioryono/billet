"""Unit tests for the spot-interruption router Lambda.

boto3/botocore are provided by the AWS Lambda runtime, not installed in CI, so
they are stubbed here before the handler is imported — the tests exercise the
routing and error-CLASSIFICATION logic, which a Terraform plan test cannot reach.
Run: python3 -m unittest terraform/modules/billet/modules/fleet-ec2/lambda/spot_router_test.py
"""

import json
import os
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

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import spot_router  # noqa: E402


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

    def test_forwards_to_the_owning_node_queue(self):
        _ec2.describe_instances.return_value = _tagged("billet-spot-interruptions")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}

        event = _warning("i-xyz")
        spot_router.handler(event, None)

        _sqs.get_queue_url.assert_called_once_with(QueueName="billet-spot-interruptions")
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

    def test_drops_when_the_queue_does_not_exist(self):
        _ec2.describe_instances.return_value = _tagged("billet-spot-interruptions")
        _sqs.get_queue_url.side_effect = _ClientError("AWS.SimpleQueueService.NonExistentQueue")
        spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_drops_on_access_denied_to_the_queue(self):
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_reraises_a_retryable_queue_error(self):
        _ec2.describe_instances.return_value = _tagged("billet-spot-interruptions")
        _sqs.get_queue_url.side_effect = _ClientError("ServiceUnavailable")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)


    def test_drops_on_permanent_send_message_failure(self):
        _ec2.describe_instances.return_value = _tagged("billet-spot-interruptions")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}
        _sqs.send_message.side_effect = _ClientError("AccessDenied")
        spot_router.handler(_warning(), None)  # must not raise

    def test_reraises_a_retryable_send_message_failure(self):
        _ec2.describe_instances.return_value = _tagged("billet-spot-interruptions")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}
        _sqs.send_message.side_effect = _ClientError("Throttling")
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


if __name__ == "__main__":
    unittest.main()
