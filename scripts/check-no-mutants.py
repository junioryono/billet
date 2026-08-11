"""Refuse a commit while a mutation run's leftovers are still on disk.

The mutation harness copies a file to `<file>.bak`, edits the original, runs one
test, and moves the backup back. Interrupt it — a timeout, a kill, a crash — and
the ORIGINAL is left holding a mutant while the pristine copy sits beside it
under a name nothing looks at.

This has happened. The stranded mutant deleted the very guard the commit was
about: it compiled, most of the suite passed, and the commit message would have
described behaviour the code no longer had. A gate that ran earlier proves
nothing, because the mutation landed after it.

`make check` cannot notice, since the mutated tree is a perfectly valid tree. The
only durable evidence is the leftover backup, so that is what this looks for.

Exit non-zero if any `*.bak` sits beside a tracked Go file; print how to restore
it, which is always `mv <file>.bak <file>` — the backup is the pristine copy.

Usage: python3 scripts/check-no-mutants.py
"""

import os
import subprocess
import sys

ROOT = subprocess.run(["git", "rev-parse", "--show-toplevel"],
                      capture_output=True, text=True, check=True).stdout.strip()

stranded = []

for dirpath, dirnames, filenames in os.walk(ROOT):
    dirnames[:] = [d for d in dirnames if d not in (".git", "bin", "node_modules")]

    for name in filenames:
        if not name.endswith(".bak"):
            continue

        backup = os.path.join(dirpath, name)
        original = backup[: -len(".bak")]

        if os.path.exists(original):
            stranded.append((os.path.relpath(backup, ROOT),
                             os.path.relpath(original, ROOT)))

if not stranded:
    print("no stranded mutation backups")
    sys.exit(0)

print("A mutation run did not finish. These originals are holding MUTANTS:\n")

for backup, original in stranded:
    print(f"    mv {backup} {original}")

print("\nThe .bak is the pristine copy. Restore before doing anything else — an")
print("earlier green gate says nothing, because the mutation landed after it.")

sys.exit(1)
