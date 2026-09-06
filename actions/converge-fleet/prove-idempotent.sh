#!/usr/bin/env bash
# A second converge must report changed=0 on every host, or a role is not
# idempotent and this stops being configuration as code.
#
#   prove-idempotent.sh <expected-hosts-file> -- <ansible-playbook arguments...>
#
# STREAMED THROUGH tee, because a second converge that hangs (a drain that never
# returns, a stalled image pull) would otherwise be killed at the job timeout
# with nothing printed, on the run whose log matters most; pipefail carries
# ansible-playbook's exit status through.
#
# ONLY THE RECAP ROWS ARE READ, and read as ROWS: with result_format=yaml every
# task result is printed in full, so a command whose stdout contains "changed=1"
# must not fail the gate, and a hostname is compared as a literal string rather
# than interpolated into a pattern, where node.a would also match node-a. Every
# counter is compared as an integer. The check is POSITIVE: each host the
# playbook listed must appear with a clean row, so a play that quietly matched
# nothing cannot pass as green; rescued and ignored count too, because a task
# that failed under ignore_errors or was caught by a rescue still says failed=0.
set -euo pipefail

expected=$1
shift
[[ ${1:-} == -- ]] && shift

log="$RUNNER_TEMP/billet-converge-2.log"
ansible-playbook "$@" 2>&1 | tee "$log"

recap=$(sed -n '/PLAY RECAP/,$p' "$log" | grep -E '^[^ ]+ +: +ok=' || true)
printf '%s\n' "--- second-pass recap ---" "$recap"

verdict=$(printf '%s\n' "$recap" | awk -v expected="$expected" '
BEGIN {
  while ((getline line < expected) > 0) if (line != "") want[line] = 1
}
{
  # "<host> : ok=1 changed=0 ..." — the host is everything before " : ", which
  # a hostname cannot contain; the counters are key=value words after it.
  split($0, halves, / +: +/)
  host = halves[1]
  seen[host]++
  n = split(halves[2], words, / +/)
  for (i = 1; i <= n; i++) {
    split(words[i], kv, "=")
    counters[host, kv[1]] = kv[2] + 0
  }
}
END {
  bad = 0
  for (host in seen) {
    if (counters[host, "changed"] > 0) { printf "%s reported changed=%d on the second pass\n", host, counters[host, "changed"]; bad = 1 }
  }
  for (host in want) {
    if (!(host in seen)) { printf "%s was to be converged and has no recap row\n", host; bad = 1; continue }
    if (seen[host] != 1) { printf "%s has %d recap rows\n", host, seen[host]; bad = 1 }
    if (counters[host, "unreachable"] > 0) { printf "%s was unreachable\n", host; bad = 1 }
    if (counters[host, "failed"] > 0) { printf "%s had failed tasks\n", host; bad = 1 }
    if (counters[host, "rescued"] > 0) { printf "%s had rescued tasks, which is a failure a rescue caught\n", host; bad = 1 }
    if (counters[host, "ignored"] > 0) { printf "%s had ignored failures\n", host; bad = 1 }
  }
  exit bad
}') || {
  printf '%s\n' "$verdict"
  echo "::error::The second converge is not clean; see the reasons above. A role that changes something on every run is not idempotent."
  exit 1
}
echo "idempotent: every host the playbook listed reported a clean second pass"
