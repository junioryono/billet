"""Unit tests for the spot-interruption router Lambda.

boto3/botocore are provided by the AWS Lambda runtime, not installed in CI, so
they are stubbed here before the handler is imported — the tests exercise the
routing and error-CLASSIFICATION logic, which a Terraform plan test cannot reach.
Run: python3 -m unittest terraform/modules/billet/modules/fleet-ec2/lambda/spot_router_test.py
"""

import contextlib
import io
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

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
import spot_router  # noqa: E402

# The queue the module creates and grants this router, as the tests deploy it.
OWN_QUEUE = "billet-spot-interruptions"
# A second spot node's queue, which the module creates from spot_node_names and
# grants and names to the router from the same list.
SECOND_QUEUE = "build-1"


def _warning(instance_id="i-abc"):
    return {
        "source": "aws.ec2",
        "detail-type": "EC2 Spot Instance Interruption Warning",
        "detail": {"instance-id": instance_id, "instance-action": "terminate"},
    }


def _tagged(node, instance_id="i-abc"):
    tags = [] if node is None else [{"Key": spot_router.NODE_TAG, "Value": node}]
    return _described(instance_id, tags)


def _described(instance_id, tags):
    """A DescribeInstances answer for one instance carrying exactly `tags` — the
    instance is IDENTIFIED, as EC2's own answer identifies it."""
    return {"Reservations": [{"Instances": [{"InstanceId": instance_id, "Tags": tags}]}]}


class SpotRouterTest(unittest.TestCase):
    def setUp(self):
        _ec2.reset_mock(side_effect=True, return_value=True)
        _sqs.reset_mock(side_effect=True, return_value=True)
        # Every case runs with the environment the module actually deploys for a
        # deployment with one spot node, so a test that wants the unconfigured
        # case, or several queues, has to say so.
        self._serve([OWN_QUEUE])

    def _serve(self, names):
        """Deploy this test's router as serving `names`, joined the way the module
        joins them, or nothing when it is None.

        patch.dict restores the whole environment at cleanup, so removing the key
        inside it is undone too — and cleanups run last-in-first-out, so a test
        that re-serves over setUp's value unwinds in the right order."""
        patch = mock.patch.dict(os.environ, {})
        patch.start()
        self.addCleanup(patch.stop)
        if names is None:
            os.environ.pop(spot_router.QUEUE_NAMES_ENV, None)
        else:
            os.environ[spot_router.QUEUE_NAMES_ENV] = ",".join(names)

    def test_forwards_to_the_owning_node_queue(self):
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE, "i-xyz")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}

        event = _warning("i-xyz")
        spot_router.handler(event, None)

        _sqs.get_queue_url.assert_called_once_with(QueueName=OWN_QUEUE)
        _sqs.send_message.assert_called_once()
        kwargs = _sqs.send_message.call_args.kwargs
        self.assertEqual(kwargs["QueueUrl"], "https://sqs/q")
        # The forwarded body is the ORIGINAL event the node parses.
        self.assertEqual(json.loads(kwargs["MessageBody"]), event)

    def test_drops_an_instance_that_is_present_and_untagged(self):
        _ec2.describe_instances.return_value = _tagged(None)
        spot_router.handler(_warning(), None)
        _sqs.get_queue_url.assert_not_called()
        _sqs.send_message.assert_not_called()

    # AN ANSWER THAT HOLDS NEITHER PROOF IS NOT "no tag". The call names one
    # instance, so a success that does not contain it is a third state; collapsing
    # it into untagged would drop a warning for an instance that may be billet's,
    # where a retry gets either the instance or InvalidInstanceID.NotFound. The
    # message is asserted because any other RuntimeError — a MagicMock misused, a
    # rewrite raising for its own reason — would satisfy the bare type.
    def test_reraises_when_the_answer_holds_this_instance_nowhere(self):
        answers = (
            {},
            {"Reservations": []},
            {"Reservations": [{"Instances": []}]},
            _described("i-somebody-else", []),
        )
        for answer in answers:
            with self.subTest(answer=answer):
                _ec2.reset_mock(side_effect=True, return_value=True)
                _sqs.reset_mock(side_effect=True, return_value=True)
                _ec2.describe_instances.return_value = answer

                with self.assertRaisesRegex(RuntimeError, "returned no instance"):
                    spot_router.handler(_warning(), None)

                _ec2.describe_instances.assert_called_once_with(InstanceIds=["i-abc"])
                _sqs.get_queue_url.assert_not_called()
                _sqs.send_message.assert_not_called()

    # An interruption warning always carries the instance id, and the rule that
    # invokes this router delivers nothing else — so an event without one is a shape
    # this router cannot read, not a warning it has proved is not its own. Dropping
    # it would turn a changed event shape into every warning quietly disappearing.
    def test_reraises_an_event_without_an_instance_id(self):
        with self.assertRaises(RuntimeError):
            spot_router.handler({"detail": {}}, None)
        _ec2.describe_instances.assert_not_called()
        _sqs.send_message.assert_not_called()

    def test_drops_a_malformed_tag_without_calling_sqs(self):
        _ec2.describe_instances.return_value = _tagged("not a legal queue name!")
        spot_router.handler(_warning(), None)
        _sqs.get_queue_url.assert_not_called()
        _sqs.send_message.assert_not_called()

    # A TAG THAT IS PRESENT AND EMPTY IS NOT AN UNTAGGED INSTANCE. It is still
    # dropped — nothing names a queue and no retry makes one appear — but it is
    # billet's instance with unusable tagging, and the line an operator reads has to
    # say that rather than "names no billet node". Both paths drop without touching
    # SQS, so the log line is the only thing that can tell them apart.
    def test_reports_an_unusable_tag_value_as_a_tag_rather_than_as_no_tag(self):
        for tag in ({"Key": spot_router.NODE_TAG, "Value": ""},
                    {"Key": spot_router.NODE_TAG}):
            with self.subTest(tag=tag):
                _sqs.reset_mock(side_effect=True, return_value=True)
                _ec2.describe_instances.return_value = _described("i-abc", [tag])

                spoken = io.StringIO()
                with contextlib.redirect_stdout(spoken):
                    spot_router.handler(_warning(), None)

                # THE WHOLE OUTPUT, not substrings of it: both paths drop without
                # touching SQS, so the line is the only thing that tells them apart,
                # and an equality cannot be loosened later by a second line or by
                # rewording the untagged message out from under a negative assertion.
                self.assertEqual(
                    spoken.getvalue(),
                    f"i-abc {spot_router.NODE_TAG}='' is not a legal queue name; "
                    f"dropping\n")
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
    # so is the proof that lets AccessDenied or NonExistentQueue about it be consumed
    # silently. Neither code is permanent in itself — that is the whole point.

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

    def test_drops_an_access_denied_send_to_a_queue_it_does_not_serve(self):
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/theirs"}
        _sqs.send_message.side_effect = _ClientError("AccessDenied")

        spot_router.handler(_warning(), None)  # must not raise

        # ...having actually reached the send. Returning before either call would
        # satisfy "did not raise" while classifying nothing.
        _sqs.get_queue_url.assert_called_once_with(QueueName="someone-elses-queue")
        _sqs.send_message.assert_called_once()
        self.assertEqual(_sqs.send_message.call_args.kwargs["QueueUrl"],
                         "https://sqs/theirs")

    # A QUEUE THIS ROUTER DOES NOT SERVE IS STILL LOOKED UP. A queue the router is
    # granted but was not told about forwards. Turning the membership test into a
    # short-circuit would break that silently, so the served set may decide what a
    # FAILURE means and nothing else.
    def test_forwards_to_a_queue_it_does_not_serve_when_the_grant_allows_it(self):
        _ec2.describe_instances.return_value = _tagged("another-billet-node")
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/other"}

        spot_router.handler(_warning(), None)

        _sqs.get_queue_url.assert_called_once_with(QueueName="another-billet-node")
        self.assertEqual(_sqs.send_message.call_args.kwargs["QueueUrl"],
                         "https://sqs/other")

    # SEVERAL SERVED QUEUES. The module creates one queue per spot node and tells
    # the router every name, from the same list it scopes the grant to. A refusal
    # about ANY of them is the could-not-tell the single-queue cases below assert
    # for the primary — this is the window issue #66 named: a second queue that was
    # granted but not named had its AccessDenied read as foreign and its warning
    # dropped while the grant propagated.

    def test_forwards_to_a_second_served_queue(self):
        self._serve([OWN_QUEUE, SECOND_QUEUE])
        _ec2.describe_instances.return_value = _tagged(SECOND_QUEUE)
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/second"}

        spot_router.handler(_warning(), None)

        _sqs.get_queue_url.assert_called_once_with(QueueName=SECOND_QUEUE)
        self.assertEqual(_sqs.send_message.call_args.kwargs["QueueUrl"],
                         "https://sqs/second")

    def test_reraises_access_denied_to_a_second_served_queue(self):
        self._serve([OWN_QUEUE, SECOND_QUEUE])
        _ec2.describe_instances.return_value = _tagged(SECOND_QUEUE)
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_reraises_when_a_second_served_queue_does_not_exist(self):
        self._serve([OWN_QUEUE, SECOND_QUEUE])
        _ec2.describe_instances.return_value = _tagged(SECOND_QUEUE)
        _sqs.get_queue_url.side_effect = _ClientError(
            "AWS.SimpleQueueService.NonExistentQueue")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    def test_reraises_access_denied_sending_to_a_second_served_queue(self):
        self._serve([OWN_QUEUE, SECOND_QUEUE])
        _ec2.describe_instances.return_value = _tagged(SECOND_QUEUE)
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/second"}
        _sqs.send_message.side_effect = _ClientError("AccessDenied")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)
        _sqs.send_message.assert_called_once()

    # ...while a queue outside a set of several is still foreign, so widening the
    # set has not turned the classification off.
    def test_drops_access_denied_for_a_queue_outside_a_served_set_of_several(self):
        self._serve([OWN_QUEUE, SECOND_QUEUE])
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        spot_router.handler(_warning(), None)
        _sqs.send_message.assert_not_called()

    # MEMBERSHIP, NOT A SUBSTRING. "build-1" served must not make "build" read as
    # served: a rewrite that searches the raw environment string finds the shorter
    # name inside the longer one and re-raises for a queue that is foreign — safe,
    # and wrong, and invisible to every other case in this file, whose foreign
    # names share no text with a served one. (The other direction, a foreign
    # "build-10", is not a substring of the raw string and would pass such a
    # rewrite, so it proves nothing.)
    def test_a_served_names_substring_is_foreign(self):
        self._serve([OWN_QUEUE, SECOND_QUEUE])
        _ec2.describe_instances.return_value = _tagged(SECOND_QUEUE[:-2])
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        spot_router.handler(_warning(), None)
        _sqs.get_queue_url.assert_called_once_with(QueueName="build")
        _sqs.send_message.assert_not_called()

    # THE SET IS PARSED, NOT TAKEN LITERALLY: whitespace and empty entries are
    # ignored, so a hand-edited or reformatted value serves the same names.
    def test_the_served_set_ignores_whitespace_and_empty_entries(self):
        with mock.patch.dict(os.environ,
                             {spot_router.QUEUE_NAMES_ENV: f" {OWN_QUEUE} , ,{SECOND_QUEUE},"}):
            self.assertEqual(spot_router._served(), frozenset([OWN_QUEUE, SECOND_QUEUE]))

        # ...and a served name reached through the ragged form is still a
        # could-not-tell, proving the parsed set is what _drop consults.
        with mock.patch.dict(os.environ,
                             {spot_router.QUEUE_NAMES_ENV: f" {OWN_QUEUE} , ,{SECOND_QUEUE},"}):
            _ec2.describe_instances.return_value = _tagged(SECOND_QUEUE)
            _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
            with self.assertRaises(_ClientError):
                spot_router.handler(_warning(), None)

    # AN EMPTY SET DROPS NOTHING, in every spelling of empty: an unset variable is
    # asserted further down, and these are the set forms.
    def test_an_empty_served_set_drops_nothing(self):
        for raw in ("", ",", " , , "):
            with self.subTest(raw=raw):
                _sqs.reset_mock(side_effect=True, return_value=True)
                with mock.patch.dict(os.environ, {spot_router.QUEUE_NAMES_ENV: raw}):
                    self.assertEqual(spot_router._served(), frozenset())
                    _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
                    _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
                    with self.assertRaises(_ClientError):
                        spot_router.handler(_warning(), None)

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

    # AND WITH NO SERVED SET AT ALL the router cannot tell whose queue a tag names,
    # so it drops nothing: the unset environment is the safe value, not a licence to
    # fall back on the error code alone.
    def test_reraises_access_denied_when_no_queue_is_configured(self):
        self._serve(None)
        _ec2.describe_instances.return_value = _tagged("someone-elses-queue")
        _sqs.get_queue_url.side_effect = _ClientError("AccessDenied")
        with self.assertRaises(_ClientError):
            spot_router.handler(_warning(), None)

    # ...and losing the name costs only the ability to CLASSIFY a failure. A router
    # whose environment went missing still places every warning it can, because
    # refusing to forward one it could have delivered would lose it for certain.
    def test_forwards_even_when_no_queue_is_configured(self):
        self._serve(None)
        _ec2.describe_instances.return_value = _tagged(OWN_QUEUE)
        _sqs.get_queue_url.return_value = {"QueueUrl": "https://sqs/q"}

        spot_router.handler(_warning(), None)

        self.assertEqual(_sqs.send_message.call_args.kwargs["QueueUrl"], "https://sqs/q")

    def test_queue_name_regex_boundaries(self):
        self.assertTrue(spot_router._QUEUE_NAME.match("a"))
        self.assertTrue(spot_router._QUEUE_NAME.match("A_b-9"))
        self.assertTrue(spot_router._QUEUE_NAME.match("q" * 80))
        self.assertIsNone(spot_router._QUEUE_NAME.match(""))
        self.assertIsNone(spot_router._QUEUE_NAME.match("q" * 81))
        self.assertIsNone(spot_router._QUEUE_NAME.match("has.dot"))
        self.assertIsNone(spot_router._QUEUE_NAME.match("has space"))
        self.assertIsNone(spot_router._QUEUE_NAME.match("trailing\n"))


# THE HANDLER AND THE MODULE HAVE TO NAME THE SAME VARIABLE, and neither gate can
# see both: this file has no terraform and the plan tests have no Python. A rename
# on one side leaves the router with no served set, which is safe (it drops
# nothing) and wrong (a foreign queue it cannot reach becomes a failed invocation
# and an alarm instead of a drop).
class SpotRouterModuleWiringTest(unittest.TestCase):
    def test_the_module_names_the_variable_the_handler_reads(self):
        with open(os.path.join(_HERE, "..", "spot.tf"), encoding="utf-8") as f:
            spot_tf = f.read()

        # THE LITERAL IS PINNED ON BOTH SIDES, and each side's gate pins its own.
        # tests/fleet.tftest.hcl:spot_creates_the_interruption_router asserts against a
        # real plan — terraform having done the parsing — that the router function's
        # environment carries exactly this key, set to the created queues, and nothing
        # else. So the whole of the Python side's job is that the handler reads that
        # same name, which an equality says completely; a containment check over
        # spot.tf would pass a rename to any text the file already holds. The
        # PLURAL is part of the pin: the singular this replaced is a prefix of it,
        # so a handler that kept the old name would find it in spot.tf and read an
        # unset variable in production.
        self.assertEqual(spot_router.QUEUE_NAMES_ENV, "BILLET_INTERRUPTION_QUEUE_NAMES")

        # ...and this one catches spot.tf losing the variable on a machine with no
        # terraform, which is where `make lambda-test` runs. It cannot say WHERE the
        # name sits — a regex written to re-derive that only invents ways to fail on
        # valid HCL (a comment above `variables`, a one-line map, a brace inside a
        # heredoc), and the plan test already knows.
        self.assertIn(
            spot_router.QUEUE_NAMES_ENV,
            spot_tf,
            f"spot.tf names no {spot_router.QUEUE_NAMES_ENV}, so the deployed router "
            f"would read an unset variable: it would drop nothing, and a foreign "
            f"queue it cannot reach would alarm instead",
        )


if __name__ == "__main__":
    unittest.main()
