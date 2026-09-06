#!/bin/sh
# The shipped fleet playbook: its shape, and the one refusal that has to fire
# before it changes anything.
#
# WHY A GATE OF ITS OWN. The host role refuses a converge driven from a runner
# billet manages as its FIRST task, because a converge drains the node and a
# deploy job on that node destroys itself. The fleet playbook runs ssh_access
# before the host role, so the role's own refusal would fire after a key had
# been installed and sshd hardened; the playbook therefore repeats the guard as
# the first task of every play. Nothing else proves that ordering: the role's
# gate proves the role, and a syntax check proves nothing about order.
#
# Two halves: the playbook's shape, read from the file, and a run of it with
# RUNNER_NAME set, proving the refusal leaves no key behind.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
playbook=$here/../playbooks/fleet.yml

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# ANSIBLE'S OWN INTERPRETER, which has PyYAML; the first python3 on PATH need
# not (hybrid-emission-check.sh makes the same choice for the same reason).
python=""
if command -v ansible >/dev/null 2>&1; then
    python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
fi
if [ -z "$python" ] || [ ! -x "$python" ]; then
    python=$(command -v python3) || { echo "fleet-playbook-check: no python3" >&2; exit 1; }
fi

# --- the shape ---------------------------------------------------------------
"$python" - "$playbook" <<'EOF'
import sys, yaml

plays = yaml.safe_load(open(sys.argv[1]))
want = {
    "control_plane": ["junioryono.billet.ssh_access", "junioryono.billet.host", "junioryono.billet.cloudflared_connector"],
    "linux": ["junioryono.billet.ssh_access", "junioryono.billet.host", "junioryono.billet.warp_connector",
              "junioryono.billet.cloudflared_connector", "junioryono.billet.development_host"],
    "macos": ["junioryono.billet.development_host"],
}
if [p["hosts"] for p in plays] != list(want):
    sys.exit(f"fleet-playbook-check: plays target {[p['hosts'] for p in plays]}, want {list(want)} in that order (the control plane first)")
for play in plays:
    if "become" in play:
        sys.exit(f"fleet-playbook-check: the {play['hosts']} play sets become; development_host would run as root")
    if not play.get("gather_facts", False):
        sys.exit(f"fleet-playbook-check: the {play['hosts']} play does not gather facts; the roles read them")
    pre = play.get("pre_tasks") or []
    first = pre[0] if pre else {}
    guard = first.get("ansible.builtin.include_role") or first.get("include_role") or {}
    if guard.get("name") != "junioryono.billet.host" or guard.get("tasks_from") != "converge-guard":
        sys.exit(f"fleet-playbook-check: the {play['hosts']} play's first pre_task is not the converge guard")
    roles = [r["role"] if isinstance(r, dict) else r for r in play.get("roles", [])]
    if roles != want[play["hosts"]]:
        sys.exit(f"fleet-playbook-check: the {play['hosts']} play runs {roles}, want {want[play['hosts']]}")
    for r in play["roles"]:
        if isinstance(r, dict) and r["role"] == "junioryono.billet.development_host":
            if "billet_development_enabled" not in str(r.get("when", "")):
                sys.exit(f"fleet-playbook-check: development_host in the {play['hosts']} play is not gated on billet_development_enabled")
print("ok   the playbook has the shape the collection documents")
EOF

# --- the guard fires before ssh_access ----------------------------------------
#
# One local host in each Linux group, a key configured for both, and a
# billet-managed RUNNER_NAME: each play, run alone with -l, must be refused by
# the guard, and the key file must not exist afterwards. Alone, because a play
# whose every host failed ends the playbook, so one run would prove the first
# play's guard and nothing about the second's.
cat >"$work/inventory.yml" <<EOF
all:
  vars:
    ansible_connection: local
    ansible_user: "$(id -un)"
    billet_ssh_authorized_keys:
      - key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA fleet@ci"
    billet_ssh_authorized_keys_path: "$work/authorized_keys"
    billet_ssh_sshd_config_dir: "$work/sshd_config.d"
    billet_ssh_validate_command: "true"
    billet_ssh_manage_service: false
    billet_ssh_hostname_from_inventory: false
    billet_ssh_unattended_upgrades: false
  children:
    control_plane:
      hosts:
        cp-1: {}
    linux:
      hosts:
        node-1: {}
EOF
mkdir -p "$work/sshd_config.d"

for host in cp-1 node-1; do
    status=0
    env RUNNER_NAME=billet-lease-abc123 \
        ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        ansible-playbook -i "$work/inventory.yml" -l "$host" -e ansible_become=false "$playbook" >"$work/out.log" 2>&1 || status=$?

    if [ "$status" -eq 0 ]; then
        echo "FAIL: $host: a converge from a billet-managed runner was not refused" >&2; exit 1
    fi
    if ! grep -q "runner billet itself manages" "$work/out.log"; then
        echo "FAIL: $host: the run failed, but not with the guard's refusal" >&2; sed -n '1,60p' "$work/out.log" >&2; exit 1
    fi
    if [ -e "$work/authorized_keys" ] || [ -n "$(ls -A "$work/sshd_config.d")" ]; then
        echo "FAIL: $host: the guard fired, but ssh_access had already changed the host" >&2; exit 1
    fi
    # The default callback prints a header per task naming its role, so a
    # header for any ssh_access task means the play reached the role.
    if grep -q '^TASK \[junioryono.billet.ssh_access' "$work/out.log"; then
        echo "FAIL: $host: an ssh_access task ran before the guard" >&2; exit 1
    fi
    # The guard itself did run: its own header is what proves the refusal came
    # from the play's pre_task and not from a missing role or a parse error.
    if ! grep -q '^TASK \[Refuse a converge that would destroy the job running it\]' "$work/out.log"; then
        echo "FAIL: $host: the pre_task guard header is missing; the refusal came from somewhere else" >&2; exit 1
    fi
    echo "ok   $host: the guard refuses before ssh_access changes anything"
done

echo "fleet-playbook-check: the shipped playbook is shaped and ordered as documented"
