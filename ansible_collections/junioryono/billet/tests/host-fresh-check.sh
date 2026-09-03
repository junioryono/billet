#!/bin/sh
# Check-mode converge of the host role against a FRESH machine — see
# host-fresh-check.yml for why. Run it against localhost (CI's throwaway
# runner) by default, or pass an inventory host as $1.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}

# $1 is an inventory FILE (for a real remote host) or a bare hostname;
# default localhost, which is what CI's throwaway runner wants.
target=${1:-localhost}
# NOT `command -v true`: a POSIX shell answers a builtin with the bare word
# "true", which ansible stat reads as a relative path and the role then
# refuses as a missing candidate.
binary=${BILLET_FRESH_CHECK_BINARY:-}
if [ -z "$binary" ]; then
    for candidate in /usr/bin/true /bin/true; do
        if [ -f "$candidate" ]; then binary=$candidate; break; fi
    done
fi

connection=""
inventory="$target,"
if [ "$target" = "localhost" ]; then
    connection="--connection=local"
elif [ -f "$target" ]; then
    inventory="$target"
fi

# The repo root FIRST for junioryono.billet, then the default locations so a
# galaxy-installed ansible.posix (which the role's network tasks import) still
# resolves — an override that names only the repo would shadow it.
ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" exec ansible-playbook \
    --check \
    -i "$inventory" \
    $connection \
    -e "billet_fresh_check_binary_src=$binary" \
    "$here/host-fresh-check.yml"
