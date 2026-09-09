#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Ask a billet executable for the converge guard's status, bounded, and judge
the answer under the status protocol's schema.

WHY A MODULE AND NOT A COMMAND TASK. The role's preparation asks an executable
(the managed binary, or the verified candidate the guard records) for
`converge-guard status --json`, and everything after depends on what came
back. A `command` task with `failed_when: false` followed by Jinja over the
output cannot judge that output without templating errors standing in for
diagnostics (`from_json` on text that is not JSON is a template failure, not
a refusal naming what was read), cannot bound the run on a Mac (which has no
`timeout`), and cannot tell "unknown command" (the pre-R answer) from a
failure it must refuse. This module runs the executable under its own bound,
reads the answer in three stages (the envelope, then a record the command
could not read, then the healthy record's members) and answers ONE of three
outcomes with the member it stopped at, so the role's judgement is one
assertion over a word.

THE OUTCOMES: `capable` (a well-formed answer; `report` carries it, and a
record the command could not read is a well-formed answer with `record_error`
set and the other members empty, which is what the command prints there);
`pre_r` (the executable answered "unknown command": a release before the
guard existed); `refused` (anything else: a failure, a timeout, an answer the
schema does not admit, or an `active` that disagrees with what the caller
already saw), with `problem` naming the stage and the member. A refusal is
never absence and never the pre-R protocol.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r"""
---
module: guard_status
short_description: Ask a billet executable for the converge guard's status and judge the answer
description:
  - Runs C(<executable> converge-guard status --json) under a bound, and reads the
    answer under the status protocol's schema in three stages. Answers one of
    C(capable), C(pre_r) or C(refused), with the problem named.
options:
  executable:
    description: The billet executable to ask.
    type: path
    required: true
  timeout:
    description: Seconds the executable may take before it is killed.
    type: int
    default: 60
  expect:
    description:
      - What the caller already knows. C(directory) means the claim was seen as
        a directory, so an answer of none, host-upgrade or legacy-role is a
        disagreement; C(capability) means only the answer's shape matters.
    type: str
    choices: [any, directory, capability]
    default: any
author:
  - junioryono
"""

EXAMPLES = r"""
- name: Judge the claim's status
  junioryono.billet.guard_status:
    executable: /usr/bin/billet
    expect: directory
  register: billet_exclusion_status
"""

RETURN = r"""
outcome:
  description: capable, pre_r or refused.
  type: str
  returned: always
problem:
  description: Why the answer was refused, naming the stage and the member; empty otherwise.
  type: str
  returned: always
report:
  description: The parsed answer, when the outcome is capable.
  type: dict
  returned: when capable
"""

import json
import os
import re
import subprocess

from ansible.module_utils.basic import AnsibleModule

ACTIVE_WORDS = ("none", "host-upgrade", "legacy-role", "converge-guard", "unpublished-guard", "unknown")
DIRECTORY_WORDS = ("converge-guard", "unpublished-guard", "unknown")
MAX_HOLDER_BYTES = 200
MAX_STDOUT = 1 << 20

_RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")
_SHA256 = re.compile(r"^[0-9a-f]{64}$")
_UNKNOWN_COMMAND = re.compile(r"unknown (converge-guard )?command")


class Refused(Exception):
    """An answer the protocol does not admit; the message names the stage and the member."""


def run(executable, timeout):
    """Run the status command under the bound; answer (rc, stdout, stderr,
    timed_out). A hung executable is killed at the bound."""
    try:
        proc = subprocess.run(
            [executable, "converge-guard", "status", "--json"],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=timeout, check=False,
        )
    except subprocess.TimeoutExpired:
        return None, b"", b"", True
    return proc.returncode, proc.stdout[:MAX_STDOUT], proc.stderr[:MAX_STDOUT], False


def is_name(holder):
    if not holder or len(holder.encode("utf-8")) > MAX_HOLDER_BYTES:
        return False
    for c in holder:
        if c.isspace() or ord(c) < 0x20 or ord(c) == 0x7F or c == "/" or c == "�":
            return False
    return True


def judge(rc, stdout, stderr, timed_out, expect):
    """The three stages over a raw answer; returns the parsed report or raises Refused."""
    # THE ENVELOPE.
    if timed_out:
        raise Refused("envelope: the executable did not answer within the bound")
    text = stdout.decode("utf-8", "replace")
    err = stderr.decode("utf-8", "replace")
    if rc != 0:
        raise Refused("envelope: the executable exited %d: %s" % (rc, (err or text).strip()[-500:]))
    try:
        report = json.loads(text)
    except ValueError as exc:
        raise Refused("envelope: the answer is not JSON (%s)" % exc)
    if not isinstance(report, dict):
        raise Refused("envelope: the answer is not a JSON object")
    active = report.get("active")
    if not isinstance(active, str):
        raise Refused("envelope: active is missing or not a string")
    if active not in ACTIVE_WORDS:
        raise Refused("envelope: active is %r, which is not a word this role knows" % active)
    if "why" in report and not isinstance(report["why"], str):
        raise Refused("envelope: why is not a string")
    if active == "unknown" and not report.get("why"):
        raise Refused("envelope: why is missing or empty under active unknown")
    if expect == "directory" and active not in DIRECTORY_WORDS:
        raise Refused("envelope: active is %r, which disagrees with the directory the role saw at the claim" % active)
    if active != "converge-guard":
        return report

    # A RECORD THE COMMAND COULD NOT READ, before the healthy schema is asked for.
    guard = report.get("guard")
    if not isinstance(guard, dict):
        raise Refused("guard: missing or not an object under active converge-guard")
    if "record_error" in guard:
        if not isinstance(guard["record_error"], str) or not guard["record_error"]:
            raise Refused("guard: record_error is not a non-empty string")
        return report

    # THE HEALTHY RECORD'S MEMBERS.
    holder = guard.get("holder")
    if not isinstance(holder, str):
        raise Refused("guard: holder is missing or not a string")
    if not is_name(holder):
        raise Refused("guard: holder %r is not a name (empty, whitespace, a control character, a slash, or over %d bytes)"
                      % (holder, MAX_HOLDER_BYTES))
    claimed = guard.get("claimed_at")
    if not isinstance(claimed, str) or not _RFC3339.match(claimed):
        raise Refused("guard: claimed_at is not an RFC 3339 time")
    if not isinstance(guard.get("hostname"), str):
        raise Refused("guard: hostname is missing or not a string")
    if not isinstance(guard.get("recovery_pointer"), bool):
        raise Refused("guard: recovery_pointer is missing or not a boolean")
    exe = guard.get("release_executable")
    if not isinstance(exe, str) or not exe.startswith("/"):
        raise Refused("guard: release_executable is not an absolute path")
    digest = guard.get("release_executable_sha256")
    if not isinstance(digest, str) or not _SHA256.match(digest):
        raise Refused("guard: release_executable_sha256 is not 64 lowercase hex digits")
    verified = guard.get("release_executable_verified", None)
    if "release_executable_verified" not in guard:
        raise Refused("guard: release_executable_verified is missing")
    if verified is True or verified is False:
        return report
    if isinstance(verified, dict) and list(verified) == ["unknown"] and isinstance(verified["unknown"], str) and verified["unknown"]:
        return report
    raise Refused("guard: release_executable_verified is neither true, false nor an object whose one member is a non-empty string unknown")


def classify(executable, timeout, expect):
    rc, stdout, stderr, timed_out = run(executable, timeout)
    if not timed_out and rc not in (0, None):
        combined = (stdout + b"\n" + stderr).decode("utf-8", "replace")
        if _UNKNOWN_COMMAND.search(combined):
            return "pre_r", "", None
    try:
        report = judge(rc, stdout, stderr, timed_out, expect)
    except Refused as exc:
        return "refused", str(exc), None
    return "capable", "", report


def main():
    module = AnsibleModule(
        argument_spec=dict(
            executable=dict(type="path", required=True),
            timeout=dict(type="int", default=60),
            expect=dict(type="str", default="any", choices=["any", "directory", "capability"]),
        ),
        supports_check_mode=True,
    )
    executable = module.params["executable"]
    if not os.path.isabs(executable):
        module.fail_json(msg="executable %s is not an absolute path" % executable)
        return
    outcome, problem, report = classify(executable, module.params["timeout"], module.params["expect"])
    result = dict(changed=False, outcome=outcome, problem=problem)
    if report is not None:
        result["report"] = report
    module.exit_json(**result)


if __name__ == "__main__":
    main()
