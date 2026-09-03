#!/bin/sh
# The billet_version fetch path, as converges that must each end the right way,
# plus the one thing check mode cannot see: whether the URL it would fetch is
# the URL the release actually publishes.
#
# WITHOUT THIS the whole of tasks/binary.yml can be deleted and every other
# suite stays green — none of them names a version, so none of them reaches it.
#
# The rules under test:
#   billet_version alone      — fetch, verify, install
#   billet_version 'latest'   — refused; a moving target is not a pinned install
#   both inputs               — billet_binary_src wins, and says so
#   neither, nothing installed— refused for having no binary at all
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

if [ "$(uname -s)" != "Linux" ]; then
    echo "release-fetch-check: skipped on $(uname -s) — the emission is linux-only"
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
[ -n "$noop" ] || { echo "release-fetch-check: no no-op binary for the role" >&2; exit 1; }

cat >"$work/app.yaml" <<EOF
github:
  org: release-fetch-check
  app_id: 4722347
  installation_id: 156647704
  private_key_path: /etc/billet/app-private-key.pem
EOF
"$billet" init --emit ansible \
    --config "$work/app.yaml" \
    --org release-fetch-check \
    --runner-group release-fetch-check \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main' \
    >"$work/gv.yml" 2>"$work/emit.err" || {
    echo "release-fetch-check: the emission failed" >&2; cat "$work/emit.err" >&2; exit 1
}

key=$work/app-private-key.pem
: >"$key"
chmod 600 "$key"

cat >"$work/site.yml" <<'EOF'
- name: release fetch
  hosts: all
  become: true
  gather_facts: true
  roles:
    - role: junioryono.billet.host
      billet_enable_server: false
      billet_enable_node: false
EOF

run_case() {
    name=$1; expect=$2
    shift 2

    [ -n "$expect" ] || { echo "release-fetch-check: $name: empty expectation" >&2; exit 1; }

    status=0
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        ansible-playbook --check -i localhost, --connection=local \
        -e "@$work/gv.yml" -e "billet_github_private_key_src=$key" \
        "$@" "$work/site.yml" >"$work/out.log" 2>&1 || status=$?

    if [ "$expect" = pass ]; then
        [ "$status" -eq 0 ] || { echo "FAIL $name: expected success"; tail -20 "$work/out.log" >&2; exit 1; }
        sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^[^ ]+ +: +ok=[1-9]' || {
            echo "FAIL $name: the play converged no host" >&2; exit 1; }
    else
        [ "$status" -ne 0 ] || { echo "FAIL $name: expected a refusal, it succeeded" >&2; exit 1; }
        grep -q 'FAILED!' "$work/out.log" || {
            echo "FAIL $name: the run failed without any task failing" >&2
            tail -5 "$work/out.log" >&2; exit 1; }
        grep -Fq -- "$expect" "$work/out.log" || {
            echo "FAIL $name: refused, but not for the expected reason ($expect)" >&2
            sed -n '/fatal/p' "$work/out.log" >&2; exit 1; }
    fi
    echo "ok   $name"
}

# The notice is a debug line on a successful run, so it is grepped rather than
# expected as a refusal.
run_override_case() {
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        ansible-playbook --check -i localhost, --connection=local \
        -e "@$work/gv.yml" -e "billet_github_private_key_src=$key" \
        -e "billet_version=v0.3.26" -e "billet_binary_src=$noop" \
        "$work/site.yml" >"$work/out.log" 2>&1 || {
        echo "FAIL both inputs: expected success" >&2; tail -20 "$work/out.log" >&2; exit 1; }

    grep -Fq "is NOT being fetched" "$work/out.log" || {
        echo "FAIL both inputs: billet_binary_src won silently — the operator's" \
             "pinned version was ignored with nothing said" >&2; exit 1; }

    if ! grep -A1 'TASK .*Resolve the release architecture' "$work/out.log" \
         | grep -q '^skipping:'; then
        echo "FAIL both inputs: binary.yml ran even though a binary was supplied" \
             "— a host given its binary must not reach the network at all" >&2
        grep -A1 'TASK .*Resolve the release architecture' "$work/out.log" >&2
        exit 1
    fi

    echo "ok   both inputs, billet_binary_src wins and reports it"
}

run_case "version 'latest' refused"  "billet_version must name one release" -e billet_version=latest
run_case "no binary and no version"  "Name the binary with billet_binary_src" -e billet_binary_src=
run_case "a supplied binary alone"   pass -e "billet_binary_src=$noop"
run_case "a named version alone"     pass -e billet_version=v0.3.26 -e billet_binary_src=
run_override_case

planted_version=v0.0.0-release-fetch-planted
planted_candidate=/var/cache/billet-provision/billet-$planted_version

if [ -e "$planted_candidate" ]; then
    echo "release-fetch-check: $planted_candidate already exists; refusing to" \
         "overwrite it. Remove it and run again." >&2
    exit 1
fi

trap 'sudo rm -f "$planted_candidate"; rm -rf "$work"' EXIT INT TERM

sudo mkdir -p /var/cache/billet-provision
sudo cp "$noop" "$planted_candidate"
# Owned by somebody who is not root: on a staging directory left writable by
# drift, this is what an unprivileged user plants to be installed unverified.
planter=$(id -un 1000 2>/dev/null || echo nobody)
sudo chown "$planter" "$planted_candidate"

run_case "a candidate root did not stage" "must be a regular file owned by root" \
    -e "billet_version=$planted_version" -e billet_binary_src=

sudo rm -f "$planted_candidate"
trap 'sudo rm -f "$staged_candidate"; rm -rf "$work"' EXIT INT TERM

# FACTS PERSIST FOR THE HOST ACROSS PLAYS. A second play — or the role included
# twice with different vars — must decide from ITS OWN inputs. Left to carry
# over, the earlier play's candidate is still there, and a reader asking only
# whether one exists finds one this invocation never selected: play one's path,
# journal-copied with remote_src taken from play two's now-different input.
#
# A candidate has to be STAGED for this to bite: check mode fetches nothing, so
# without one already present play one selects nothing on the target and there
# is no stale value to carry. Planting one is what makes play one set it.
staged_dir=/var/cache/billet-provision
staged_version=v0.0.0-release-fetch-check
staged_candidate=$staged_dir/billet-$staged_version

if [ -e "$staged_candidate" ]; then
    echo "release-fetch-check: $staged_candidate already exists; refusing to" \
         "overwrite it. Remove it and run again." >&2
    exit 1
fi

trap 'sudo rm -f "$staged_candidate"; rm -rf "$work"' EXIT INT TERM

sudo mkdir -p "$staged_dir"
sudo cp "$noop" "$staged_candidate"
sudo chown root:root "$staged_candidate"
sudo chmod 0755 "$staged_candidate"

cat >"$work/two-plays.yml" <<EOF
- name: play one names a version and finds it staged
  hosts: all
  become: true
  gather_facts: true
  vars:
    billet_version: "{{ staged_version }}"
    billet_binary_src: ""
  roles:
    - role: junioryono.billet.host
      billet_enable_server: false
      billet_enable_node: false
  post_tasks:
    - name: Prove play one selected the staged target candidate
      ansible.builtin.assert:
        that:
          - billet_candidate_on_target | bool
          - billet_candidate_selected | bool
        fail_msg: >-
          play one did not select a target candidate, so play two proves
          nothing about what survives it.

- name: play two supplies a binary instead
  hosts: all
  become: true
  gather_facts: false
  vars:
    billet_version: ""
    billet_binary_src: "{{ noop_binary }}"
  roles:
    - role: junioryono.billet.host
      billet_enable_server: false
      billet_enable_node: false
  post_tasks:
    - name: Prove play two kept nothing of play one's
      ansible.builtin.assert:
        that:
          - not billet_candidate_on_target | bool
          - billet_candidate_selected | bool
          - billet_candidate_source == noop_binary
        fail_msg: >-
          play two named no version and still holds play one's target candidate
          ({{ billet_candidate_source }}, on_target={{ billet_candidate_on_target }}).
          A converge must decide from its own inputs.
EOF

status=0
ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook --check -i localhost, --connection=local \
    -e "@$work/gv.yml" -e "billet_github_private_key_src=$key" \
    -e "noop_binary=$noop" -e "staged_version=$staged_version" \
    "$work/two-plays.yml" >"$work/two.log" 2>&1 || status=$?

if [ "$status" -ne 0 ]; then
    echo "FAIL a candidate must not survive into the next play" >&2
    sed -n '/fatal/,+8p' "$work/two.log" >&2
    exit 1
fi
echo "ok   a candidate does not survive into the next play"

# WHAT CHECK MODE CANNOT SEE. get_url does not fetch in check mode, so every
# case above passes against a URL that 404s. This asks the release origin what
# it actually publishes and rebuilds the name binary.yml would construct from
# it. Fetched from 'latest' so it never goes stale, and only checksums.txt,
# which is a kilobyte rather than the archive's ten megabytes.
base=https://github.com/junioryono/billet/releases/latest/download
if ! curl -fsSL --max-time 30 --retry 4 --retry-all-errors --retry-delay 3 \
        "$base/checksums.txt" -o "$work/checksums.txt" 2>/dev/null; then
    echo "SKIP release naming — could not reach $base/checksums.txt." \
         "The URL binary.yml builds is therefore UNVERIFIED by this run."
    exit 0
fi

version=$(sed -n 's/.*  billet_\(.*\)_linux_amd64\.tar\.gz$/\1/p' "$work/checksums.txt" | head -n1)
if [ -z "$version" ]; then
    echo "FAIL release naming: no billet_<version>_linux_amd64.tar.gz in checksums.txt —" \
         "the archive name binary.yml constructs is not what the release publishes" >&2
    head -5 "$work/checksums.txt" >&2
    exit 1
fi

ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
    ansible-playbook --check -i localhost, --connection=local \
    -e "@$work/gv.yml" -e "billet_github_private_key_src=$key" \
    -e "billet_version=v$version" -e billet_binary_src= \
    "$work/site.yml" >"$work/url.log" 2>&1 || {
    echo "FAIL release naming: the dry run that reports the URL failed" >&2
    tail -20 "$work/url.log" >&2; exit 1; }

url=$(sed -n 's/.*would-fetch=\([^ "]*\).*/\1/p' "$work/url.log" | head -n1)
if [ -z "$url" ]; then
    echo "FAIL release naming: the dry run did not report a URL to fetch" >&2
    exit 1
fi

archive=${url##*/}
if ! grep -Fq -- "  $archive" "$work/checksums.txt"; then
    echo "FAIL release naming: the role would fetch $url," \
         "which v$version does not publish" >&2
    grep -oE 'billet_[^ ]*\.tar\.gz' "$work/checksums.txt" | sort -u >&2
    exit 1
fi

echo "ok   the URL the role builds ($archive) is published by v$version"
echo "release-fetch-check: every case behaved as the rules require"
