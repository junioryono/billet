#!/bin/sh
# Converge examples/single-host-docker with a block the real billet generated.
#
# emitted-block-check proves the ROLE accepts a generated block. This proves the
# EXAMPLE does: its site.yml, its group_vars mechanism, and the enable_server
# path — which emitted-block-check does not exercise, because it converges with
# both services disabled. An example that has drifted from the role's inputs is
# something a stranger discovers; this makes CI discover it first.
#
# The example's own inventory names a host that does not exist, so connection
# details are replaced with localhost. Everything else is the shipped file.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root
example=$collections_root/ansible_collections/junioryono/billet/examples/single-host-docker

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# The emission measures THIS machine and writes for the packaged units, so it
# only runs on linux. Everywhere else this gate is skipped rather than faked.
if [ "$(uname -s)" != "Linux" ]; then
    echo "example-check: skipped on $(uname -s) — the emission is linux-only"
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
if [ -z "$noop" ]; then
    echo "example-check: no no-op binary found for the role" >&2
    exit 1
fi

# The WHOLE example, not a reconstruction of it: the shipped inventory names the
# group, site.yml selects it, and group_vars/linux.yml is found by sitting beside
# the inventory. Rebuilding any of those here would let the three drift apart and
# still pass. Only the connection is overridden, below.
cp -R "$example" "$work/example"

# THE EXAMPLE'S OWN FLOW, INCLUDING THE STEP BEFORE THIS ONE. `billet init` is
# told to carry an App identity out of the file `billet github-app create` wrote,
# and generating without that --config is the mistake this models: it emits
# app_id 0, which the role refuses, and a gate that always emitted zeros could
# not tell the two apart. So the identity here is non-zero and asserted to
# survive the emission.
cat >"$work/billet-app.yaml" <<'EOF'
github:
  org: example-check
  app_id: 4722347
  installation_id: 156647704
  private_key_path: /etc/billet/app-private-key.pem
EOF
before=$(cksum <"$work/billet-app.yaml")

# The example instructs `> group_vars/linux.yml`. Do exactly that, so a change
# to what the emission prints breaks this gate the way it would break a reader.
echo "Generating the inventory block..."
"$billet" init --emit ansible \
    --config "$work/billet-app.yaml" \
    --org example-check \
    --runner-group example-check \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main' \
    >"$work/example/group_vars/linux.yml"

# An emission MUST NOT write. --config names a file it reads, and the whole
# point of --emit is that the operator places the result themselves.
if [ "$(cksum <"$work/billet-app.yaml")" != "$before" ]; then
    echo "example-check: the emission rewrote the contents of $work/billet-app.yaml" >&2
    exit 1
fi

ANSIBLE_COLLECTIONS_PATH="$collections_root" ansible-playbook \
    -i localhost, --connection=local \
    -e "emitted_block_path=$work/example/group_vars/linux.yml" \
    -e expected_app_id=4722347 \
    -e expected_installation_id=156647704 \
    "$here/emitted-identity-check.yml"

# The example's site.yml enables the server, so the config demands a key.
key=$work/app-private-key.pem
: >"$key"
chmod 600 "$key"

log=$work/converge.log
status=0
ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" ansible-playbook \
    --check \
    -i "$work/example/inventory.yml" \
    -e "ansible_connection=local" \
    -e "billet_binary_src=$noop" \
    -e "billet_github_private_key_src=$key" \
    "$work/example/site.yml" >"$log" 2>&1 || status=$?
cat "$log"
[ "$status" -eq 0 ] || exit "$status"

# ANSIBLE-PLAYBOOK EXITS 0 WHEN THE PLAY MATCHED NO HOSTS. That is the whole
# failure this gate exists to catch — site.yml selects a group the shipped
# inventory must define — and it arrives as a warning and an empty PLAY RECAP,
# so without this the gate passes having converged nothing.
if ! sed -n '/PLAY RECAP/,$p' "$log" | grep -qE '^[^ ]+ +: +ok=[1-9]'; then
    echo "example-check: the play converged no host — does site.yml's hosts: match a group in inventory.yml?" >&2
    exit 1
fi
