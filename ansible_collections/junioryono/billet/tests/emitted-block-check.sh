#!/bin/sh
# Generate an inventory block with the real billet, then converge the role with
# it — see emitted-block-check.yml for why this seam needs its own gate.
#
# Two binaries, deliberately. The REAL one generates the block, because the
# block is what is under test. The role is then given a no-op, the way
# host-fresh-check.sh does, so what runs is the role's input validation rather
# than a billet install on the CI runner.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root

target=${1:-localhost}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# The emission measures THIS machine and writes for the packaged units, so it
# only runs on linux. Everywhere else this gate is skipped rather than faked:
# a block generated against a substitute would not be the artefact under test.
if [ "$(uname -s)" != "Linux" ]; then
    echo "emitted-block-check: skipped on $(uname -s) — the emission is linux-only"
    exit 0
fi

billet=${BILLET_EMITTED_BLOCK_BINARY:-}
if [ -z "$billet" ]; then
    echo "Building billet to generate the block..."
    (cd "$repo_root" && go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi

# A no-op for the role. Not `command -v true`: a POSIX shell answers a builtin
# with the bare word "true", which ansible stat reads as a relative path.
noop=""
for candidate in /usr/bin/true /bin/true; do
    if [ -f "$candidate" ]; then noop=$candidate; break; fi
done
if [ -z "$noop" ]; then
    echo "emitted-block-check: no no-op binary found for the role" >&2
    exit 1
fi

echo "Generating the inventory block..."
"$billet" init --emit ansible \
    --config "$work/billet.yaml" \
    --org emitted-block-check \
    --runner-group emitted-block-check \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main' \
    >"$work/block.yml"

# The emission writes NOTHING, and a gate that let it write would not notice.
if [ -e "$work/billet.yaml" ]; then
    echo "emitted-block-check: the emission wrote $work/billet.yaml" >&2
    exit 1
fi

# The emitted block names github.private_key_path, and `billet check` reads that
# file on every host regardless of which services are enabled — so the role now
# requires a key whenever the config demands one. Supplying a throwaway one is
# what a real operator does between `billet init` and `billet github-app create`;
# this runs in check mode, so nothing is installed from it.
key=$work/app-private-key.pem
: >"$key"
chmod 600 "$key"

connection=""
inventory="$target,"
if [ "$target" = "localhost" ]; then
    connection="--connection=local"
elif [ -f "$target" ]; then
    inventory="$target"
fi

ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" ansible-playbook \
    --check \
    -i "$inventory" \
    $connection \
    -e "@$work/block.yml" \
    -e "billet_emitted_block_binary_src=$noop" \
    -e "billet_emitted_block_key_src=$key" \
    "$here/emitted-block-check.yml"

# AND THE SAME SEAM FOR THE POSTGRESQL PROFILE, which is a DIFFERENT block
# rather than a variation of the one above: `identity_dir` and `state_dir` are
# mutually exclusive at load, so a postgres generation writes keys the shorthand
# never does — and the role read only the shorthand until this gate existed.
#
# In the same script because it is the same question about the same seam, and
# splitting it would mean a second build of billet to ask half of it.
echo "Generating the PostgreSQL inventory block..."
"$billet" init --emit ansible \
    --config "$work/billet-postgres.yaml" \
    --org emitted-block-check \
    --runner-group emitted-block-check \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main' \
    --state-backend postgres \
    --state-dsn-env BILLET_STATE_DSN \
    >"$work/postgres-block.yml"

if [ -e "$work/billet-postgres.yaml" ]; then
    echo "emitted-block-check: the postgres emission wrote $work/billet-postgres.yaml" >&2
    exit 1
fi

ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" ansible-playbook \
    --check \
    -i "$inventory" \
    $connection \
    -e "@$work/postgres-block.yml" \
    -e "billet_postgres_block_binary_src=$noop" \
    -e "billet_postgres_block_key_src=$key" \
    "$here/postgres-block-check.yml"
