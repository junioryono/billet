#!/usr/bin/env bash
# A second converge must report changed=0 on every host, or a role is not
# idempotent and this stops being configuration as code.
#
#   prove-idempotent.sh <expected-hosts-file> -- <ansible-playbook arguments...>
#
# with BILLET_CHILD_ENV_FILE naming the 0600 file of environment lines
# converge.sh validated, applied to ansible-playbook the way converge.sh applies
# them: through with-environment.py, never this shell and never an argument.
#
# STREAMED THROUGH tee, because a second converge that hangs (a drain that never
# returns, a stalled image pull) would otherwise be killed at the job timeout
# with nothing printed, on the run whose log matters most; pipefail carries
# ansible-playbook's exit status through.
#
# ONLY THE RECAP ROWS ARE READ, and read as ROWS: with result_format=yaml every
# task result is printed in full, so a command whose stdout contains "changed=1"
# must not fail the gate, and a hostname is compared as a literal string rather
# than interpolated into a pattern, where node.a would also match node-a. The
# check is POSITIVE and EXACT: the hosts in the recap are the hosts the
# playbook listed, no more and no fewer, and every one of the five counters is
# present exactly once as an integer and is zero. A missing counter is not a
# zero, because "nothing said" is how a truncated or reformatted recap reads.
set -euo pipefail

expected=$1
shift
[[ ${1:-} == -- ]] && shift

[[ -s $expected ]] || { echo "::error::the expected-hosts file $expected is missing or empty; nothing can be proved against no hosts"; exit 1; }

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
child_env_file=${BILLET_CHILD_ENV_FILE:-}
if [[ -z $child_env_file ]]; then
  child_env_file="$RUNNER_TEMP/billet-child-env-empty"
  (umask 077 && : >"$child_env_file")
fi

log="$RUNNER_TEMP/billet-converge-2.log"
python3 "$here/with-environment.py" "$child_env_file" -- ansible-playbook "$@" 2>&1 | tee "$log"

recap=$(sed -n '/PLAY RECAP/,$p' "$log" | grep -E '^[^ ]+ +: +ok=' || true)
printf '%s\n' "--- second-pass recap ---" "$recap"

verdict=$(printf '%s\n' "$recap" | awk -v expected="$expected" '
BEGIN {
  while ((getline line < expected) > 0) if (line != "") want[line] = 1
  split("changed unreachable failed rescued ignored", required, " ")
}
NF == 0 { next }
{
  # "<host> : ok=1 changed=0 ..." — the host is everything before " : ", which
  # a hostname cannot contain; the counters are key=value words after it.
  split($0, halves, / +: +/)
  host = halves[1]
  seen[host]++
  n = split(halves[2], words, / +/)
  for (i = 1; i <= n; i++) {
    if (split(words[i], kv, "=") != 2) continue
    if (kv[2] !~ /^[0-9]+$/) { printf "%s reports %s=%s, which is not a count\n", host, kv[1], kv[2]; bad = 1; continue }
    present[host, kv[1]]++
    counters[host, kv[1]] = kv[2] + 0
  }
}
END {
  for (host in seen) {
    if (!(host in want)) { printf "%s was converged but the playbook listing did not include it\n", host; bad = 1 }
  }
  for (host in want) {
    if (!(host in seen)) { printf "%s was to be converged and has no recap row\n", host; bad = 1; continue }
    if (seen[host] != 1) { printf "%s has %d recap rows\n", host, seen[host]; bad = 1; continue }
    for (r in required) {
      c = required[r]
      if (present[host, c] != 1) { printf "%s reports %s %d times in its recap row, want exactly once\n", host, c, present[host, c] + 0; bad = 1; continue }
      if (counters[host, c] > 0) {
        if (c == "changed") printf "%s reported changed=%d on the second pass\n", host, counters[host, c]
        else if (c == "unreachable") printf "%s was unreachable\n", host
        else if (c == "failed") printf "%s had failed tasks\n", host
        else if (c == "rescued") printf "%s had rescued tasks, which is a failure a rescue caught\n", host
        else printf "%s had ignored failures\n", host
        bad = 1
      }
    }
  }
  exit bad + 0
}') || {
  printf '%s\n' "$verdict"
  echo "::error::The second converge is not clean; see the reasons above. A role that changes something on every run is not idempotent."
  exit 1
}
echo "idempotent: every host the playbook listed reported a clean second pass"
