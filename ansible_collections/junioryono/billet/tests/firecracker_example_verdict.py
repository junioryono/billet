"""Read the firecracker example converge and say what it proved.

Kept out of the shell because the two facts that matter are the RESULTS of two
named tasks, and the human callback's layout is not stable enough to grep: it
differs between ansible-core versions in both the line under a task header and
the shape of a module's reported command.
"""

import json
import sys


def main(path: str) -> int:
    with open(path, encoding="utf-8") as fh:
        report = json.load(fh)

    def results_for(want: str) -> list[dict]:
        out: list[dict] = []
        for play in report.get("plays", []):
            for task in play.get("tasks", []):
                name = task.get("task", {}).get("name", "")
                # "<role> : <task>" in the json callback; the role prefix is not
                # part of what the role file calls this task.
                if name.split(" : ")[-1] == want:
                    out.extend(task.get("hosts", {}).values())
        return out

    # THE ASSERTION THE DOCKER EMISSION TRIPPED. A block whose provisioning flags
    # disagree with node.provider is refused here, before anything is rendered.
    flags = "Assert the provisioning flags match the configured provider"
    got = results_for(flags)
    if not got:
        print(
            f"firecracker-example-check: {flags!r} never ran, so the role did not reach "
            "the check that accepts or refuses a generated block",
            file=sys.stderr,
        )
        return 1

    if any(r.get("failed") or r.get("skipped") for r in got):
        print(
            "firecracker-example-check: the role did not accept the generated block — "
            "its provisioning flags do not match node.provider",
            file=sys.stderr,
        )
        return 1

    # And it went as far as the hardware rather than being refused on the way. A
    # SKIPPED Ceph inspection is not reaching it: that means Ceph was disabled,
    # which a firecracker config cannot be.
    ceph = "Inspect Ceph pools"
    got = results_for(ceph)
    if not got or all(r.get("skipped") for r in got):
        print(
            "firecracker-example-check: the Ceph inspection never ran, so something "
            "stopped the converge before the host prerequisites",
            file=sys.stderr,
        )
        return 1

    print(
        "firecracker-example-check: the role accepted the emission and ran to the Ceph "
        "cluster. Check mode skips the Firecracker download, unpack and install, so "
        "this gate does NOT cover them"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
