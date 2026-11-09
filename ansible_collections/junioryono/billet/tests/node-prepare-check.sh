#!/bin/sh
# Real converges of a node's host prepared for `billet join` to finish
# (billet_node_prepare_only, ADR-030) — see node-prepare-check.yml for what each
# play proves. Unlike host-fresh-check, these are NOT dry runs: the role installs
# its packages, the service account and the units, so this runs only on a
# throwaway machine (CI's runner) that has no billet yet, and refuses any other.
#
# The binary is installed where the package installs it, /usr/bin/billet, the way
# a pool host's boot prelude or image leaves it: the mode refuses a binary source.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root

if [ "$(uname -s)" != "Linux" ]; then
    echo "node-prepare-check: skipped on $(uname -s) — the host role converges Linux"
    exit 0
fi

# A MACHINE WITH BILLET ON IT IS NOT THROWAWAY. The plays converge it for real and
# leave billet installed, so a run anywhere but a fresh machine is refused before
# anything is changed.
for path in /usr/bin/billet /etc/billet /var/lib/billet; do
    if [ -e "$path" ]; then
        echo "node-prepare-check: $path exists; this converges the machine for real, so it runs only on one with no billet" >&2
        exit 1
    fi
done
if id billet >/dev/null 2>&1; then
    echo "node-prepare-check: a billet account exists; this runs only on a machine with no billet" >&2
    exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

billet=${BILLET_NODE_PREPARE_BINARY:-}
if [ -z "$billet" ]; then
    echo "Building billet..."
    (cd "$repo_root" && CGO_ENABLED=0 go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi

sudo install -m 0755 -o root -g root "$billet" /usr/bin/billet

ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" exec ansible-playbook \
    -i localhost, \
    --connection=local \
    -e "billet_converge_guard_holder=node-prepare-check" \
    -e "billet_node_prepare_check_src=$billet" \
    "$here/node-prepare-check.yml"
