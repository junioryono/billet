#!/bin/sh
# The warp_connector role, as converges that must each end the right way.
#
# WHY A GATE OF ITS OWN: the same reason cloudflared-connector-check.sh gives.
# The role's rules are that a connector is enrolled exactly on the daemon's own
# "Registration Missing", never on a status the daemon could not answer, never
# a second time, and only after a signing key with the pinned fingerprint.
#
# What this cannot prove, stated: the apt path on a real host and the real
# daemon's answers; the reference deployment's runs prove those.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
. "$here/connector-check-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
connector_fakes "$work/bin"

good_fpr=$(sed -n 's/^billet_warp_signing_fingerprint: *//p' "$here/../roles/warp_connector/defaults/main.yml")
[ -n "$good_fpr" ] || { echo "warp-connector-check: the role's defaults name no fingerprint" >&2; exit 1; }
printf 'GOOD CLOUDFLARE KEY' >"$work/good-key.asc"
printf 'SOMEBODY ELSES KEY' >"$work/bad-key.asc"
token="WARP-SECRET-MARKER-TOKEN"

cat >"$work/play.yml" <<'EOF'
- name: warp connector
  hosts: all
  connection: local
  gather_facts: false
  vars:
    ansible_facts:
      distribution_release: noble
    billet_warp_manage_apt: "{{ lookup('env', 'BILLET_TEST_MANAGE_APT') | default('false', true) | bool }}"
    billet_warp_sysctls: false
    billet_warp_signing_key_url: "file://{{ lookup('env', 'BILLET_TEST_KEY_FILE') }}"
    billet_warp_stage_dir: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/stage"
    billet_warp_keyring_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/keyrings/cloudflare-warp-archive-keyring.gpg"
    billet_warp_cli: "{{ lookup('env', 'BILLET_TEST_WARP_CLI') }}"
  roles:
    - role: junioryono.billet.warp_connector
EOF

run() {
    name=$1; expect=$2; shift 2
    envs=""
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs="$envs $1"; shift; done
    [ "$#" -gt 0 ] && shift
    rm -rf "$work/root" "$work/state"; mkdir -p "$work/root/keyrings" "$work/state"
    : >"$work/calls"
    status=0
    # shellcheck disable=SC2086
    env PATH="$work/bin:$PATH" \
        BILLET_FAKE_CALLS="$work/calls" BILLET_FAKE_STATE="$work/state" BILLET_FAKE_GOOD_FPR="$good_fpr" \
        BILLET_TEST_ROOT="$work/root" BILLET_TEST_KEY_FILE="$work/good-key.asc" BILLET_TEST_WARP_CLI=warp-cli \
        ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        $envs \
        ansible-playbook -i "$work/inventory.ini" -e ansible_become=false "$@" "$work/play.yml" \
        >"$work/out.log" 2>&1 || status=$?
    judge "$name" "$status" "$work/out.log" "$expect" "${BILLET_TEST_STAGED:-}"
}

printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 1. No token: nothing done, no fake called.
run "no token skips with no change" pass -- -e BILLET_WARP_CONNECTOR_TOKEN_NODE_A=
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: a token-less run changed something" >&2; exit 1; }
[ ! -s "$work/calls" ] || { echo "FAIL: a token-less run called a fake: $(cat "$work/calls")" >&2; exit 1; }

# 2. Registration Missing: enrolled, connected, and the token in the one argv
#    the tool leaves no alternative to (and in no other).
run "a missing registration is enrolled" pass "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
grep -q "^warp-cli --accept-tos connector new $token\$" "$work/calls" || { echo "FAIL: the connector was not enrolled: $(cat "$work/calls")" >&2; exit 1; }
grep -q '^warp-cli --accept-tos connect$' "$work/calls" || { echo "FAIL: the connector was not connected" >&2; exit 1; }
[ "$(grep -c "$token" "$work/calls")" = 1 ] || { echo "FAIL: the token reached more than the one enrolment exec: $(grep "$token" "$work/calls")" >&2; exit 1; }
if grep -q "$token" "$work/out.log"; then echo "FAIL: the token appears in Ansible's output" >&2; exit 1; fi

# 3. A registered, connected daemon: neither enrolled nor connected again.
run "a registered daemon is left alone" pass "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" \
    BILLET_FAKE_WARP_STATUS=connected --
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a registered daemon was re-enrolled" >&2; exit 1; fi
if grep -q '^warp-cli --accept-tos connect$' "$work/calls"; then echo "FAIL: a connected daemon was told to connect" >&2; exit 1; fi
grep -q 'registration on node-a is left as it is' "$work/out.log" || { echo "FAIL: the manual-rotation note was not printed" >&2; exit 1; }

# 4. A daemon that cannot answer is never enrolled.
run "a daemon that cannot answer is not enrolled" "Error communicating with daemon" "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" \
    BILLET_FAKE_WARP_STATUS_RC=3 -- -e billet_warp_status_retries=1 -e billet_warp_connect_retries=1 -e billet_warp_retry_delay=0
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a daemon that could not answer was enrolled" >&2; exit 1; fi

# 5. A signing key with another fingerprint refuses the repository.
BILLET_TEST_STAGED=staged run "a signing key with another fingerprint is refused" "not exactly one primary key with the pinned fingerprint" \
    BILLET_TEST_MANAGE_APT=true "BILLET_TEST_KEY_FILE=$work/bad-key.asc" "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
[ ! -e "$work/root/keyrings/cloudflare-warp-archive-keyring.gpg" ] || { echo "FAIL: a refused key reached the keyring" >&2; exit 1; }
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a refused key did not stop the enrolment" >&2; exit 1; fi

# 6. Two hosts sharing one variable name are refused before any host acts.
printf 'node-a ansible_host=127.0.0.1\nnode_a ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts sharing one token variable are refused" "read their WARP connector token from the same environment variable" \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
[ ! -s "$work/calls" ] || { echo "FAIL: the collision refusal came after a fake was called" >&2; exit 1; }
printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 7. Check mode on a host with no client: exits 0, reports, enrols nothing.
run "check mode on a fresh host reports and stops" pass "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" \
    BILLET_TEST_WARP_CLI=/nonexistent/warp-cli -- --check
grep -q 'the real converge stages and verifies' "$work/out.log" || { echo "FAIL: the dry run did not report what it cannot see" >&2; exit 1; }
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a dry run enrolled the connector" >&2; exit 1; fi

echo "warp-connector-check: every case behaved as the role requires"
