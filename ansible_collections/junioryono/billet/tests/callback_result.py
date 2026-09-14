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
    No failure records or notices                           | refuse: found 0
    One retry notice then fatal: [h]: FAILED!                | one result
    One async notice then fatal: [h]: FAILED!                | one result
    Later successful retrying task after a failure           | final_fatal keeps failure window
    Item failures then aggregate FAILED!, same task/host     | one result; aggregate msg
    Items with differing msgs then aggregate FAILED!        | refuse: ambiguous item messages
    Two failed items without an aggregate                   | refuse: found 2
    FAILED! on h1 plus UNREACHABLE! on h2                     | refuse: found 2
    fatal: [h]: UNKNOWN! => {"msg": "held"}                   | refuse: unclassified
    ASYNC FAILED on h: jid=1, with no terminal record         | refuse: no terminal result
    FAILED - RETRYING: [h]: task (1 retries left). alone      | refuse: no terminal result
    fatal: [h]: FAILED! => not-json                          | refuse: invalid JSON
    fatal: [h]: FAILED! => {"msg": ["held"]}                  | refuse: no string msg

    Only fatal FAILED!/UNREACHABLE! and failed item records are terminal failures. A following fatal FAILED! absorbs preceding item failures for the same task window and literal callback host; TASK, PLAY and PLAY RECAP headers separate windows. The aggregate's own string msg is returned, even if it differs from the items' common msg. Every absorbed item must have a string msg, and those item messages must agree exactly. Without an aggregate, each failed item counts separately. Retry and async notices never count; unknown fatal:/failed: forms refuse.
    """
    if failed:
        kind = "final fatal result"
        pattern = re.compile(
            r"(?:fatal: \[(?P<host>[^\n]+?)\]: (?P<status>FAILED|UNREACHABLE)! =>"
            r"|failed: \[(?P<item_host>[^\n]+?)\] \(item=[^\n]*?\) =>)\s*"
        )
        records = []
        task = 0
        # Source inspection, ansible-core 2.21.2 (2026-09-14): retry/async
        # callbacks report progress; failed debug loops also emit an aggregate.
        for marker in re.finditer(
            r"^(?:TASK \[|PLAY \[|PLAY RECAP|fatal:|failed:)", text, re.MULTILINE
        ):
            if marker.group() not in ("fatal:", "failed:"):
                task += 1
                continue
            record = pattern.match(text, marker.start())
            if record is None:
                sys.exit(f"{name}: cannot classify {kind}: {marker.group()}")
            items = []
            if record.group("status") == "FAILED":
                retained = []
                for previous_task, previous, details in records:
                    if (previous_task == task
                            and previous.group("item_host") == record.group("host")):
                        items.append(previous)
                    else:
                        retained.append((previous_task, previous, details))
                records = retained
            records.append((task, record, items))
        if not records and re.search(
            r"^(?:FAILED - RETRYING:|ASYNC FAILED on )", text, re.MULTILINE
        ):
            sys.exit(f"{name}: no terminal result")
        if len(records) != 1:
            sys.exit(f"{name}: expected exactly one {kind}; found {len(records)}")
        _, record, items = records[0]
    else:
        pattern = r"^(?:ok|changed): [^\n]*? =>\s*"
        kind = "reported result"
        matches = list(re.finditer(pattern, text, re.MULTILINE))
        if len(matches) != 1:
            sys.exit(f"{name}: expected exactly one {kind}; found {len(matches)}")
        record = matches[0]
        items = []

    def message(record):
        try:
            result, _ = json.JSONDecoder().raw_decode(text, record.end())
        except json.JSONDecodeError as error:
            sys.exit(f"{name}: {kind} is not valid JSON: {error}")
        if not isinstance(result, dict) or not isinstance(result.get("msg"), str):
            sys.exit(f"{name}: {kind} has no string msg")
        return result["msg"]

    if len({message(item) for item in items}) > 1:
        sys.exit(f"{name}: {kind} has ambiguous item messages")
    return message(record)


if __name__ == "__main__":
    print(callback_message(sys.stdin.read(), sys.argv[1], failed=True))
