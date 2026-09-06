#!/bin/sh
# The warp_connector role, as converges that must each end the right way.
#
# WHY A GATE OF ITS OWN: the same reason cloudflared-connector-check.sh gives.
# The role's rules are that a connector is enrolled exactly on the daemon's own
# "Registration Missing", never on a status the daemon could not answer or did
# not give, never a second time, and only after a signing key with the pinned
# fingerprint.
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
  serial: "{{ lookup('env', 'BILLET_TEST_SERIAL') | default('100%', true) }}"
  vars:
    ansible_facts:
      distribution_release: noble
    billet_warp_manage_apt: "{{ lookup('env', 'BILLET_TEST_MANAGE_APT') | default('false', true) | bool }}"
    billet_warp_prerequisites: []
    billet_warp_sysctls: false
    billet_warp_signing_key_url: "file://{{ lookup('env', 'BILLET_TEST_KEY_FILE') }}"
    billet_warp_stage_dir: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/stage"
    billet_warp_keyring_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/keyrings/cloudflare-warp-archive-keyring.gpg"
    billet_warp_legacy_key_path: "{{ lookup('env', 'BILLET_TEST_LEGACY_KEY') }}"
    billet_warp_cli: "{{ lookup('env', 'BILLET_TEST_WARP_CLI') }}"
  roles:
    - role: junioryono.billet.warp_connector
EOF

# The same play with the variable name set on the role invocation, a scope
# hostvars does not see: the play-wide list shows distinct fallbacks while
# every host reads the one name, and only the per-host consistency check
# catches it.
sed -e 's/^    - role: junioryono.billet.warp_connector$/    - role: junioryono.billet.warp_connector\n      vars:\n        billet_warp_connector_token_env: BILLET_WARP_CONNECTOR_TOKEN_NODE_A/' "$work/play.yml" >"$work/play-rolevar.yml"

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
        BILLET_TEST_LEGACY_KEY="$work/legacy/cloudflare-warp.asc" \
        ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        $envs \
        ansible-playbook -i "$work/inventory.ini" -e ansible_become=false --diff -v "$@" "$work/${BILLET_TEST_PLAY:-play}.yml" \
        >"$work/out.log" 2>&1 || status=$?
    judge "$name" "$status" "$work/out.log" "$expect" "${BILLET_TEST_STAGED:-}"
    # A PREFIX ASSIGNMENT ON A FUNCTION CALL PERSISTS AFTER THE CALL in some
    # shells (measured: the serial case ran the previous case's play and the
    # staged allowance leaked into every later refusal), so a case's settings
    # are cleared here and each case states its own.
    BILLET_TEST_STAGED=; BILLET_TEST_PLAY=
}

printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 1. No token: nothing done, no fake called.
run "no token skips with no change" pass -- -e BILLET_WARP_CONNECTOR_TOKEN_NODE_A=
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: a token-less run changed something" >&2; exit 1; }
[ ! -s "$work/calls" ] || { echo "FAIL: a token-less run called a fake: $(cat "$work/calls")" >&2; exit 1; }

# 2. Registration Missing: enrolled, connected, and the token in the one argv
#    the tool leaves no alternative to (and in no other, and not in the output
#    of a run with --diff -v).
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
run "a daemon that cannot answer is not enrolled" "printed no \`Status update:\` line this role reads" "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" \
    BILLET_FAKE_WARP_STATUS_RC=3 -- -e billet_warp_status_retries=1 -e billet_warp_connect_retries=1 -e billet_warp_retry_delay=0
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a daemon that could not answer was enrolled" >&2; exit 1; fi
grep -q 'Error communicating with daemon' "$work/out.log" || { echo "FAIL: the refusal did not quote the daemon's error" >&2; exit 1; }

# 5. A daemon that exits 0 with no status line is not read as any state.
run "a daemon that names no state is not enrolled" "printed no \`Status update:\` line this role reads" "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" \
    BILLET_FAKE_WARP_STATUS=silent -- -e billet_warp_status_retries=1 -e billet_warp_retry_delay=0
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a silent daemon was enrolled" >&2; exit 1; fi
if grep -q '^warp-cli --accept-tos connect$' "$work/calls"; then echo "FAIL: a silent daemon was told to connect" >&2; exit 1; fi

# 6. A signing key with another fingerprint refuses the repository; the legacy
#    world-writable key is removed at the path the role was GIVEN, not at a
#    hard-coded /tmp path on whatever machine runs this gate.
mkdir -p "$work/legacy"; printf 'OLD KEY' >"$work/legacy/cloudflare-warp.asc"
BILLET_TEST_STAGED=staged run "a signing key with another fingerprint is refused" "not exactly one primary key with the pinned fingerprint" \
    BILLET_TEST_MANAGE_APT=true "BILLET_TEST_KEY_FILE=$work/bad-key.asc" "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
[ ! -e "$work/root/keyrings/cloudflare-warp-archive-keyring.gpg" ] || { echo "FAIL: a refused key reached the keyring" >&2; exit 1; }
if grep -q -- '--dearmor' "$work/calls"; then echo "FAIL: a refused key was dearmored" >&2; exit 1; fi
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a refused key did not stop the enrolment" >&2; exit 1; fi
[ ! -e "$work/legacy/cloudflare-warp.asc" ] || { echo "FAIL: the legacy key at the given path was not removed" >&2; exit 1; }

# What is NOT run here, on purpose: the apt path past the keyring. The
# repository module writes under /etc/apt with no path seam, so a case that
# ran it would touch the real machine wherever it happened to have
# python3-debian; the ordering (verify, then dearmor, then repository) is
# proved by the refusal above, whose fake gpg would have written the keyring
# had the role dearmored first.

# 7. Two hosts sharing one variable name are refused before any host acts.
printf 'node-a ansible_host=127.0.0.1\nnode_a ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts sharing one token variable are refused" "read their WARP connector token from the same environment variable" \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
[ ! -s "$work/calls" ] || { echo "FAIL: the collision refusal came after a fake was called" >&2; exit 1; }

# 8. The same through an inventory override of the variable name.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1 billet_warp_connector_token_env=BILLET_WARP_CONNECTOR_TOKEN_NODE_A\n' >"$work/inventory.ini"
run "two hosts sharing one token variable by override are refused" "read their WARP connector token from the same environment variable" \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
[ ! -s "$work/calls" ] || { echo "FAIL: the override collision refusal came after a fake was called" >&2; exit 1; }

# 8b. A name shared through a scope every host sees (-e) is refused too.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts sharing one token variable through -e are refused" "WARP connector token" \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" -- -e billet_warp_connector_token_env=BILLET_WARP_CONNECTOR_TOKEN_NODE_A
[ ! -s "$work/calls" ] || { echo "FAIL: the -e collision refusal came after a fake was called" >&2; exit 1; }

# A name set on the role invocation itself, which the play-wide check cannot
# see: refused by the per-host check, before any fake is called.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1\n' >"$work/inventory.ini"
BILLET_TEST_STAGED=staged BILLET_TEST_PLAY=play-rolevar run "a name the play-wide check cannot see is refused" "a variable the play-wide collision check could not see" \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" --
# node-a's own name IS the computed one, so it converges; node-b, which reads
# node-a's variable, is the host refused. The staged allowance is for node-a's
# writes; node-b's row must be unchanged.
sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^node-b +: +ok=[0-9]+ +changed=0 .*failed=1' || { echo "FAIL: node-b was not the host refused, unchanged" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^node-a +: .*failed=0' || { echo "FAIL: node-a, whose name is its own, was refused too" >&2; exit 1; }

# 9. Two distinct hosts under serial: 1: the collision check reads the
#    inventory, so the first batch does not fail on a fact the second has not
#    recorded; the first enrols, the second finds the daemon connected.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts converge one batch at a time" pass BILLET_TEST_SERIAL=1 \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" "BILLET_WARP_CONNECTOR_TOKEN_NODE_B=$token" --
[ "$(sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -cE '^node-[ab] +: +ok=[1-9]')" = 2 ] || { echo "FAIL: both hosts did not converge under serial" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
[ "$(grep -c 'connector new' "$work/calls")" = 1 ] || { echo "FAIL: the shared daemon was enrolled other than once: $(cat "$work/calls")" >&2; exit 1; }
printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 10. Check mode on a host with no client: exits 0, reports, enrols nothing.
run "check mode on a fresh host reports and stops" pass "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" \
    BILLET_TEST_WARP_CLI=/nonexistent/warp-cli -- --check
grep -q 'the real converge stages and verifies' "$work/out.log" || { echo "FAIL: the dry run did not report what it cannot see" >&2; exit 1; }
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a dry run enrolled the connector" >&2; exit 1; fi

# 11. Check mode on a host with the client but no staged key: the apt path
#     reports what it cannot verify and stops rather than failing on a file the
#     dry run never fetched; the registration is still read and nothing enrolled.
run "check mode without a staged key reports and stops" pass BILLET_TEST_MANAGE_APT=true \
    "BILLET_WARP_CONNECTOR_TOKEN_NODE_A=$token" -- --check
grep -q 'fetches and verifies it before trusting the repository' "$work/out.log" || { echo "FAIL: the dry run did not report the unverified key" >&2; exit 1; }
if grep -q '^gpg' "$work/calls"; then echo "FAIL: a dry run read a key it never fetched" >&2; exit 1; fi
if grep -q 'connector new' "$work/calls"; then echo "FAIL: a dry run enrolled the connector" >&2; exit 1; fi

echo "warp-connector-check: every case behaved as the role requires"
