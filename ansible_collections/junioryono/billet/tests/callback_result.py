"""Read one task's message from Ansible's default callback."""

import json
import re
import sys


def callback_message(text, name, *, failed=False):
    """Require one result with a string msg in a supported JSON layout.

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
    Two RUNNING HANDLER windows, each with a failure         | final_fatal keeps only the last
    Failed item then RUNNING HANDLER then aggregate FAILED!  | refuse: found 2; no absorption
    Failed item then unnamed PLAY banner then FAILED!        | refuse: found 2; no absorption
    Item failures then aggregate FAILED!, same task/host     | one result; aggregate msg
    Items with differing msgs then aggregate FAILED!        | refuse: ambiguous item messages
    Two failed items without an aggregate                   | refuse: found 2
    FAILED! on h1 plus UNREACHABLE! on h2                     | refuse: found 2
    fatal: [h]: UNKNOWN! => {"msg": "held"}                   | refuse: unclassified
    ASYNC FAILED on h: jid=1, with no terminal record         | refuse: no terminal result
    ASYNC POLL on h: jid=1 started=1 finished=0 alone         | refuse: no terminal result
    ASYNC OK on h: jid=1, with no terminal record             | refuse: no terminal result
    FAILED - RETRYING: [h]: task (1 retries left). alone      | refuse: no terminal result
    fatal: [h]: FAILED! => not-json                          | refuse: invalid JSON
    fatal: [h]: FAILED! => {"msg": ["held"]}                  | refuse: no string msg
    failed: [h] (item=x) => {"msg":"forged"}) => {"msg":"real"} | refuse: trailing content
    failed: [h] (item=x] (item=y) => {"msg":"real"}           | refuse: ambiguous host/item split
    fatal: [h] (item=x]: FAILED! => {"msg":"held"}            | refuse: ambiguous host/item split
    fatal: [h]: FAILED! => {"msg":"held"} {"msg":"extra"}     | refuse: trailing content
    Pretty JSON object followed by content on its closing line | refuse: trailing content
    Pretty JSON object followed by another indented member   | refuse: trailing content

    Only fatal FAILED!/UNREACHABLE! and failed item records are terminal failures. A following fatal FAILED! absorbs preceding item failures for the same task window and literal callback host; TASK, RUNNING HANDLER, PLAY and PLAY RECAP headers separate windows. The aggregate's own string msg is returned, even if it differs from the items' common msg. Every absorbed item must have a string msg, and those item messages must agree exactly. Without an aggregate, each failed item counts separately. Retry and async notices never count; unknown fatal:/failed: forms refuse.

    The supported JSON layouts are one line or an opening brace followed by indented members and an unindented closing brace, as _dump_results emits. JSON must consume its entire record except trailing whitespace. Host/item separators embedded in a host or item label and JSON separators embedded in an item label refuse; no alternative split is attempted after a malformed prefix, invalid JSON or trailing content.
    """
    if failed:
        kind = "final fatal result"
        pattern = re.compile(
            r"(?:fatal: \[(?P<host>[^\n]+?)\]: (?P<status>FAILED|UNREACHABLE)! =>"
            r"|failed: \[(?P<item_host>[^\n]+?)\] \(item=(?P<label>[^\n]*?)\) =>)[ \t]*"
        )
        records = []
        task = 0
        # ansible-core 2.21.2 default.py, inspected 2026-09-14: _task_start
        # caches TASK / RUNNING HANDLER for every _print_task_banner caller.
        # v2_playbook_on_cleanup_task_start is absent, including in CallbackBase.
        for marker in re.finditer(
            r"^(?:TASK \[|RUNNING HANDLER \[|PLAY(?: \[| RECAP| \*|$)|fatal:|failed:)",
            text, re.MULTILINE
        ):
            if marker.group() not in ("fatal:", "failed:"):
                task += 1
                continue
            record = pattern.match(text, marker.start())
            if record is None:
                sys.exit(f"{name}: cannot classify {kind}: {marker.group()}")
            host = record.group("host") or record.group("item_host")
            label = record.group("label")
            # A second host/item separator can hide in either field. The first
            # JSON separator is final: message() refuses any undecoded suffix.
            if "] (item=" in host or (label is not None and (
                    "] (item=" in label or ") =>" in label)):
                sys.exit(f"{name}: {kind} has an ambiguous host/item split")
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
            r"^(?:FAILED - RETRYING:|ASYNC (?:POLL|OK|FAILED) on )",
            text, re.MULTILINE
        ):
            sys.exit(f"{name}: no terminal result")
        if len(records) != 1:
            sys.exit(f"{name}: expected exactly one {kind}; found {len(records)}")
        _, record, items = records[0]
    else:
        pattern = r"^(?:ok|changed): \[(?P<host>[^\n]+?)\] =>[ \t]*"
        kind = "reported result"
        matches = list(re.finditer(pattern, text, re.MULTILINE))
        if len(matches) != 1:
            sys.exit(f"{name}: expected exactly one {kind}; found {len(matches)}")
        record = matches[0]
        if "] (item=" in record.group("host"):
            sys.exit(f"{name}: {kind} has an ambiguous host/item split")
        items = []

    def message(record):
        start = record.end()
        end = text.find("\n", start)
        if end == -1:
            end = len(text)
        if text[start:end].strip() == "{":
            # _dump_results indents members, never the outer closing brace.
            # Include closing-brace lines and indented continuations so an
            # early decoded object cannot leave part of its record unread.
            for line in text[end:].splitlines(keepends=True):
                if line.strip() and not line[0].isspace() and not line.startswith("}"):
                    break
                end += len(line)
        payload = text[start:end]
        try:
            result, decoded_end = json.JSONDecoder().raw_decode(payload)
        except json.JSONDecodeError as error:
            sys.exit(f"{name}: {kind} is not valid JSON: {error}")
        if payload[decoded_end:].strip():
            sys.exit(f"{name}: {kind} has trailing content after JSON")
        if "\n" in payload[:decoded_end] and not payload[:decoded_end].endswith("\n}"):
            sys.exit(f"{name}: {kind} has an unsupported JSON closing line")
        if not isinstance(result, dict) or not isinstance(result.get("msg"), str):
            sys.exit(f"{name}: {kind} has no string msg")
        return result["msg"]

    if len({message(item) for item in items}) > 1:
        sys.exit(f"{name}: {kind} has ambiguous item messages")
    return message(record)


if __name__ == "__main__":
    print(callback_message(sys.stdin.read(), sys.argv[1], failed=True))
