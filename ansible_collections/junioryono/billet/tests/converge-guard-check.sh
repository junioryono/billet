#!/usr/bin/env bash
# Prove the role refuses a converge driven from a billet-managed runner.
#
# WHY A GATE OF ITS OWN. Every other suite here runs with no RUNNER_NAME in the
# environment, so none of them reaches this branch: delete the guard and they all
# stay green. That is the same hole key-policy-check.sh and release-fetch-check.sh
# were written to close, one branch over.
#
# The failure it guards against is not a failed converge. Restarting the node
# drains it, a second SIGTERM destroys the jobs still running, and GitHub does not
# requeue a job whose runner vanished -- so the play destroys itself part-way
# through a durable upgrade transaction, and the operator sees a cancelled job.
#
# THE GUARD ALONE, VIA tasks_from. The first version imported the WHOLE role,
# which cannot converge without a real billet_config -- so every "this case is
# allowed" check failed on unrelated validation, and the `if ! run` idiom let it
# reach the ok line regardless. Four of five cases proved nothing. Exercising just
# the guard is what lets an allowed case genuinely SUCCEED, which is the only way
# those four mean anything.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
collection_root=$(cd "$here/../../../.." && pwd)
role_tasks="$here/../roles/host/tasks"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- the guard runs first ----------------------------------------------------
#
# A REFUSAL IS ONLY USEFUL BEFORE THE TRANSACTION IT PROTECTS HAS STARTED, and
# nothing below can observe that ordering: the cases prove the guard fires, not
# that it fires first. A task inserted above it would leave them all green.
first_task=$(grep -n '^- name:' "$role_tasks/main.yml" | head -1 | cut -d: -f1)
guard_task=$(grep -n '^- name: Refuse a converge that would destroy the job running it' \
  "$role_tasks/main.yml" | head -1 | cut -d: -f1)

if [ -z "$guard_task" ] || [ "$first_task" != "$guard_task" ]; then
  echo "FAIL: the converge guard is not the first task in main.yml (first task at line ${first_task:-none}, guard at line ${guard_task:-none}); a task above it runs before the refusal, on a host the converge may be about to drain" >&2
  exit 1
fi
echo "ok   the guard is the first task in the role"

cat >"$work/inventory.ini" <<'INV'
[billet_hosts]
localhost ansible_connection=local
INV

cat >"$work/play.yml" <<'PLAY'
---
- name: Exercise the converge guard
  hosts: billet_hosts
  gather_facts: false
  tasks:
    - name: Run only the guard
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: converge-guard
PLAY

# RUNNER_NAME is read on the CONTROLLER, which is where a deploy job's environment
# actually lives, so it is set here rather than in the inventory.
#
# The collections path keeps the INSTALLED collections findable: this collection
# is under collection_root, and CI galaxy-installs others into ~/.ansible.
run() {
  env RUNNER_NAME="$1" \
    ANSIBLE_COLLECTIONS_PATH="$collection_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ansible-playbook -i "$work/inventory.ini" "$work/play.yml" \
    --connection=local "${@:2}" >"$work/out.txt" 2>&1
}

refused() { grep -q "runner billet itself manages" "$work/out.txt"; }

fail() {
  echo "FAIL: $1" >&2
  sed -n '1,60p' "$work/out.txt" >&2
  exit 1
}

# EVERY NON-ZERO RESULT IS A FAILURE, and the message only sharpens the
# diagnosis. Making the message the CONDITION is what made the first version
# vacuous: an unrelated error left the grep returning 1, `set -e` exempted it as
# the left operand of &&, and the case reported ok anyway.
allowed() {
  what=$1
  shift

  if run "$@"; then
    echo "ok   $what"

    return
  fi

  if refused; then
    fail "$what was REFUSED by the guard"
  fi

  fail "$what failed for an unrelated reason, so this case proves nothing"
}

# 1. A billet-managed runner is refused, and for its OWN reason.
if run "billet-lease-abc123"; then
  fail "a converge driven from billet-lease-abc123 was NOT refused; a deploy job would drain the node it is running on and destroy itself"
fi
refused ||
  fail "the play failed, but not with the guard's refusal -- something else broke, and this case would otherwise have passed for the wrong reason"
echo "ok   a billet-managed runner is refused"

# 2. The override works. An operator who knows their topology must not be stuck,
#    and a guard with no escape hatch is one people route around by deleting it.
allowed "the documented override lifts the refusal" \
  "billet-lease-abc123" -e billet_allow_converge_from_billet_runner=true

# 3. A PLAIN runner is permitted. The direction that matters most: a guard that
#    refused every runner would block the deploy-runner setup the documentation
#    recommends, and would be found only by someone who had already built it.
allowed "an ordinary runner is not refused" "gh-deploy-runner-1"

# 4. A workstation converge -- no RUNNER_NAME at all -- is permitted.
allowed "a workstation converge is not refused" ""

# 5. A name merely CONTAINING the prefix is permitted: ci-billet-deploy is not
#    compute billet launched, and `billet-` has to anchor at the start.
allowed "a name merely containing billet- is not refused" "ci-billet-deploy"

echo "converge guard: all cases pass"
