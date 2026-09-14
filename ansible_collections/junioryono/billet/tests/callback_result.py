"""Read one task's message from Ansible's default callback."""

import json
import re
import sys


def callback_message(text, name, *, failed=False):
    """Require one result with a string msg, regardless of JSON line layout.

    Examples for failed=True (each refusal includes name):
    Input                                                   | Outcome
    fatal: [h]: FAILED! => {"msg": "held"}                    | "held"
    failed: [h] (item=x) => {"msg": "held"}                   | "held"
    fatal: [h]: UNREACHABLE! => {"msg": "lost"}               | "lost"
    One result above, then a newline and ...ignoring          | same message
    No failure records                                      | refuse: found 0
    Two failed items, or FAILED! plus UNREACHABLE!            | refuse: found 2
    fatal: [h]: UNKNOWN! => {"msg": "held"}                   | refuse: unclassified
    ASYNC FAILED on h: jid=1                                 | refuse: unclassified
    FAILED - RETRYING: [h]: task (1 retries left).            | refuse: unclassified
    fatal: [h]: FAILED! => not-json                          | refuse: invalid JSON
    fatal: [h]: FAILED! => {"msg": ["held"]}                  | refuse: no string msg
    """
    if failed:
        # ansible-core 2.21.2 default.py: JSON records at lines 65, 123,
        # 252-255; retry/async notices at 350-352 and 375 have no result
        # this decoder can classify. Count them before attempting a decode.
        pattern = r"^(?:fatal:|failed:|FAILED - RETRYING:|ASYNC FAILED on )"
        kind = "final fatal result"
    else:
        pattern = r"^(?:ok|changed): [^\n]*? =>\s*"
        kind = "reported result"
    matches = list(re.finditer(pattern, text, re.MULTILINE))
    if len(matches) != 1:
        sys.exit(f"{name}: expected exactly one {kind}; found {len(matches)}")
    record = matches[0]
    if failed:
        pattern = (
            r"(?:fatal: \[[^\n]+?\]: (?:FAILED|UNREACHABLE)! =>"
            r"|failed: \[[^\n]+?\] \(item=[^\n]*?\) =>)\s*"
        )
        record = re.compile(pattern).match(text, record.start())
        if record is None:
            sys.exit(f"{name}: cannot classify {kind}: {matches[0].group()}")
    try:
        result, _ = json.JSONDecoder().raw_decode(text, record.end())
    except json.JSONDecodeError as error:
        sys.exit(f"{name}: {kind} is not valid JSON: {error}")
    if not isinstance(result, dict) or not isinstance(result.get("msg"), str):
        sys.exit(f"{name}: {kind} has no string msg")
    return result["msg"]


if __name__ == "__main__":
    print(callback_message(sys.stdin.read(), sys.argv[1], failed=True))
