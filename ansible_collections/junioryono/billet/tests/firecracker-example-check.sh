#!/bin/sh
# The Firecracker emission, through the role, up to the hardware.
#
# WHAT THIS CAN AND CANNOT PROVE. A Firecracker host needs /dev/kvm and a Ceph
# cluster, and CI has neither, so this cannot converge one. What it proves is the
# seam that has actually broken before: that a block `billet init --emit ansible
# --provider firecracker` prints is one the role ACCEPTS. The docker emission
# once produced a block the role refused before rendering anything, and nothing
# noticed, because the Go tests proved the block was a correct CONFIG and the
# Ansible tests fed the role a config written by hand.
#
# The converge is expected to stop at Ceph. What is asserted is that it got
# there — past every input assertion, including the provisioning-flags one the
# docker emission tripped — rather than being refused earlier.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root
example=$collections_root/ansible_collections/junioryono/billet/examples/firecracker-host

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

if [ "$(uname -s)" != "Linux" ]; then
    echo "firecracker-example-check: skipped on $(uname -s) — the emission is linux-only"
    exit 0
fi

billet=${BILLET_EMITTED_BLOCK_BINARY:-}
if [ -z "$billet" ]; then
    echo "Building billet to generate the block..."
    (cd "$repo_root" && go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi

noop=""
for candidate in /usr/bin/true /bin/true; do
    if [ -f "$candidate" ]; then noop=$candidate; break; fi
done
[ -n "$noop" ] || { echo "firecracker-example-check: no no-op binary for the role" >&2; exit 1; }

cp -R "$example" "$work/example"

cat >"$work/app.yaml" <<EOF
github:
  org: firecracker-example-check
  app_id: 4722347
  installation_id: 156647704
  private_key_path: /etc/billet/app-private-key.pem
EOF

echo "Generating the inventory block..."
"$billet" init --emit ansible --provider firecracker \
    --config "$work/app.yaml" \
    --org firecracker-example-check \
    --runner-group firecracker-example-check \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main' \
    >"$work/example/group_vars/linux.yml" 2>"$work/emit.err" || {
    echo "firecracker-example-check: the emission failed" >&2; cat "$work/emit.err" >&2; exit 1
}

key=$work/app-private-key.pem
: >"$key"
chmod 600 "$key"

# NOT expected to succeed: there is no Ceph cluster here. The exit status is
# therefore not the oracle — what the run REACHED is.
#
# THE JSON CALLBACK, because the human one is not a contract. Its layout moved
# twice inside one change: the line beneath a task header is `fatal:` on
# ansible-core 2.16 and `[ERROR]: Task failed:` on 2.19, and the command a
# module reports having run is an argv LIST on one and a string on the other.
# Both anchors passed locally and failed in CI. The json callback carries the
# task name and its result as data.
ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
ANSIBLE_STDOUT_CALLBACK=ansible.builtin.json ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook --check \
    -i "$work/example/inventory.yml" \
    -e "ansible_connection=local" \
    -e "billet_binary_src=$noop" \
    -e "billet_github_private_key_src=$key" \
    "$work/example/site.yml" >"$work/report.json" 2>"$work/stderr.log" || true

python3 "$here/firecracker_example_verdict.py" "$work/report.json" || {
    echo "--- ansible stderr ---" >&2
    tail -20 "$work/stderr.log" >&2
    exit 1
}
