"""Read one task's message from Ansible's default callback."""

import json
import re
import sys


def callback_message(text, name, *, failed=False):
    """Require one result with a string msg, regardless of JSON line layout."""
    if failed:
        pattern = r"^(?:fatal|failed): [^\n]*?FAILED! =>\s*"
        kind = "final fatal result"
    else:
        pattern = r"^(?:ok|changed): [^\n]*? =>\s*"
        kind = "reported result"
    matches = list(re.finditer(pattern, text, re.MULTILINE))
    if len(matches) != 1:
        sys.exit(f"{name}: expected exactly one {kind}; found {len(matches)}")
    try:
        result, _ = json.JSONDecoder().raw_decode(text, matches[0].end())
    except json.JSONDecodeError as error:
        sys.exit(f"{name}: {kind} is not valid JSON: {error}")
    if not isinstance(result, dict) or not isinstance(result.get("msg"), str):
        sys.exit(f"{name}: {kind} has no string msg")
    return result["msg"]


if __name__ == "__main__":
    print(callback_message(sys.stdin.read(), sys.argv[1], failed=True))
