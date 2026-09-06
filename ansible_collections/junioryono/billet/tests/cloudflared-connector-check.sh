#!/bin/sh
# The cloudflared_connector role, as converges that must each end the right way.
#
# WHY A GATE OF ITS OWN. Nothing else in the suite sets a connector token, so
# every other gate takes the "no token, nothing done" branch and each of the
# role's refusals could be deleted with everything green. The refusals are the
# role: a token for another tunnel, a rotation over the play's own transport, a
# signing key that is not the pinned one, a variable name two hosts share.
#
# WITHOUT ROOT OR A NETWORK. The role's paths are inputs and the tests point
# them at a temporary tree; `-e ansible_become=false` outranks the tasks' become
# keyword; the apt path is switched off except where a refusal must precede it;
# and the fakes on PATH (connector-check-lib.sh) record every call so the gate
# can prove what was and was not run, and that the token reached none of them
# in argv.
#
# WHAT THIS CANNOT PROVE, stated: the apt path on a real host (the repository,
# the package, the real key), which the reference deployment's check and
# converge runs prove and docs/reference/records/ci-converge.md records.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
. "$here/connector-check-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
connector_fakes "$work/bin"

good_fpr=$(sed -n 's/^billet_cloudflared_signing_fingerprint: *//p' "$here/../roles/cloudflared_connector/defaults/main.yml")
[ -n "$good_fpr" ] || { echo "cloudflared-connector-check: the role's defaults name no fingerprint" >&2; exit 1; }
printf 'GOOD CLOUDFLARE KEY' >"$work/good-key.gpg"
printf 'SOMEBODY ELSES KEY' >"$work/bad-key.gpg"

# Tokens are base64({"a": account, "s": secret, "t": tunnel}). The secret is a
# marker the gate greps the fakes' argv for.
tunnel_a=11111111-1111-1111-1111-111111111111
tunnel_b=22222222-2222-2222-2222-222222222222
mk_token() { printf '{"a":"acct","s":"SECRET-%s-MARKER","t":"%s"}' "$2" "$1" | base64 | tr -d '\n'; }
token_a=$(mk_token "$tunnel_a" one)
token_a2=$(mk_token "$tunnel_a" two)
token_b=$(mk_token "$tunnel_b" three)

cat >"$work/play.yml" <<'EOF'
- name: cloudflared connector
  hosts: all
  connection: local
  gather_facts: false
  vars:
    billet_cloudflared_manage_apt: "{{ lookup('env', 'BILLET_TEST_MANAGE_APT') | default('false', true) | bool }}"
    billet_cloudflared_signing_key_url: "file://{{ lookup('env', 'BILLET_TEST_KEY_FILE') }}"
    billet_cloudflared_config_dir: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/etc/cloudflared"
    billet_cloudflared_unit_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/etc/systemd/system/cloudflared.service"
    billet_cloudflared_stage_dir: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/stage"
    billet_cloudflared_keyring_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/keyrings/cloudflare-main.gpg"
    billet_cloudflared_binary: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/bin/cloudflared"
  roles:
    - role: junioryono.billet.cloudflared_connector
EOF

# run <name> <expect> [env NAME=value ...] -- [ansible args ...]
#
# The temporary root is recreated unless BILLET_TEST_KEEP_ROOT=1 is among the
# environment settings, which is how a second converge sees the first's files.
run() {
    name=$1; expect=$2; shift 2
    envs=""
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs="$envs $1"; shift; done
    [ "$#" -gt 0 ] && shift
    keep=false
    case " $envs " in *" BILLET_TEST_KEEP_ROOT=1 "*) keep=true ;; esac
    if [ "$keep" = false ]; then
        rm -rf "$work/root" "$work/state"; mkdir -p "$work/root/etc/systemd/system" "$work/root/bin" "$work/root/keyrings"
        # The fake binary at the role's binary path, so the "already installed"
        # branch is what runs (a fresh host is the check-mode case below).
        cp "$work/bin/cloudflared" "$work/root/bin/cloudflared"
    fi
    : >"$work/calls"; mkdir -p "$work/state"
    status=0
    # shellcheck disable=SC2086
    env PATH="$work/bin:$PATH" \
        BILLET_FAKE_CALLS="$work/calls" BILLET_FAKE_STATE="$work/state" BILLET_FAKE_GOOD_FPR="$good_fpr" \
        BILLET_TEST_ROOT="$work/root" BILLET_TEST_KEY_FILE="$work/good-key.gpg" \
        ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        $envs \
        ansible-playbook -i "$work/inventory.ini" -e ansible_become=false "$@" "$work/play.yml" \
        >"$work/out.log" 2>&1 || status=$?
    judge "$name" "$status" "$work/out.log" "$expect" "${BILLET_TEST_STAGED:-}"
}

no_secret_in_argv() {
    if grep -q 'SECRET-' "$work/calls"; then
        echo "FAIL $1: a token reached a command's argv: $(grep 'SECRET-' "$work/calls")" >&2; exit 1
    fi
}

printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 1. No token: nothing done, no fake called, changed=0.
run "no token skips with no change" pass -- -e BILLET_CLOUDFLARED_TOKEN_NODE_A=
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: a token-less run changed something" >&2; exit 1; }
[ ! -s "$work/calls" ] || { echo "FAIL: a token-less run called a fake: $(cat "$work/calls")" >&2; exit 1; }

# 2. A token for another tunnel is refused before any fake is called.
run "a token for another tunnel is refused" "advertises routes this host cannot serve" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_b" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ ! -s "$work/calls" ] || { echo "FAIL: the wrong-tunnel refusal came after a fake was called: $(cat "$work/calls")" >&2; exit 1; }

# 3. A malformed token fails before anything is written.
run "a malformed token fails before any write" "is not a base64 token" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=not-base64-json" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ ! -e "$work/root/etc/cloudflared/token" ] || { echo "FAIL: a malformed token was written" >&2; exit 1; }

# 4. The right token on a fresh root: the token file 0600, the unit rendered,
#    the service started, the registration awaited, and the secret in no argv.
run "the right token installs the connector" pass \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
tok=$work/root/etc/cloudflared/token
[ -f "$tok" ] || { echo "FAIL: no token file was written" >&2; exit 1; }
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: the token file does not hold the token" >&2; exit 1; }
mode=$(stat -f '%Lp' "$tok" 2>/dev/null || stat -c '%a' "$tok")
[ "$mode" = 600 ] || { echo "FAIL: the token file is mode $mode, want 600" >&2; exit 1; }
unit=$work/root/etc/systemd/system/cloudflared.service
grep -q -- "--token-file $work/root/etc/cloudflared/token" "$unit" || { echo "FAIL: the unit does not run from the token file" >&2; exit 1; }
grep -q '^systemctl .*daemon-reload' "$work/calls" || { echo "FAIL: systemd was not reloaded after the unit was written: $(cat "$work/calls")" >&2; exit 1; }
grep -q '^journalctl' "$work/calls" || { echo "FAIL: the registration was not awaited after a start" >&2; exit 1; }
no_secret_in_argv "the right token installs the connector"
cp -R "$work/root" "$work/root-installed"; cp -R "$work/state" "$work/state-installed"

# 5. The same token again: nothing changes and nothing waits.
rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
run "an unchanged token changes nothing" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: an unchanged token reported changed=$(recap_changed_of "$work/out.log")" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
if grep -q '^journalctl' "$work/calls"; then echo "FAIL: the registration wait ran on an unchanged host" >&2; exit 1; fi
if grep -qE '^systemctl .*(restart|daemon-reload)' "$work/calls"; then echo "FAIL: an unchanged token restarted or reloaded: $(cat "$work/calls")" >&2; exit 1; fi

# 6. A rotated token with nothing said about the transport: could-not-tell, refused.
rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
run "a rotation with an unknown transport is refused" "nothing says whether this play reaches the host" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: a refused rotation rewrote the token" >&2; exit 1; }

# 7. A rotated token with the transport declared true: refused.
rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
run "a rotation over the play's own transport is refused" "reaches the host through that connector" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=true

# 8. The transport derived from a routed address equal to ansible_host: refused.
rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
run "a rotation is refused when ansible_host is the routed address" "reaches the host through that connector" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_routed_address=127.0.0.1

# 9. The routed address declared and ansible_host elsewhere: the rotation applies,
#    the service restarts, the registration is awaited, no secret in argv.
rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
run "a rotation applies when the play reaches the host another way" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_routed_address=10.9.9.9
[ "$(cat "$tok")" = "$token_a2" ] || { echo "FAIL: the rotation did not rewrite the token" >&2; exit 1; }
grep -q '^systemctl .*restart' "$work/calls" || { echo "FAIL: the rotation did not restart the connector: $(cat "$work/calls")" >&2; exit 1; }
grep -q '^journalctl' "$work/calls" || { echo "FAIL: the registration was not awaited after a restart" >&2; exit 1; }
no_secret_in_argv "a rotation applies when the play reaches the host another way"

# 10. An explicit false applies the rotation whatever the address says.
rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
run "an explicit transport false applies the rotation" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=false -e billet_cloudflared_routed_address=127.0.0.1
[ "$(cat "$tok")" = "$token_a2" ] || { echo "FAIL: an explicit false did not apply the rotation" >&2; exit 1; }

# 11. The apt path with a key that is not the pinned one: refused before the
#     keyring copy, the repository and the package.
BILLET_TEST_STAGED=staged run "a signing key with another fingerprint is refused" "not exactly one primary key with the pinned fingerprint" \
    BILLET_TEST_MANAGE_APT=true "BILLET_TEST_KEY_FILE=$work/bad-key.gpg" "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" --
[ ! -e "$work/root/keyrings/cloudflare-main.gpg" ] || { echo "FAIL: a refused key reached the keyring" >&2; exit 1; }
grep -q '^gpg .*--show-keys' "$work/calls" || { echo "FAIL: the key was never read by gpg" >&2; exit 1; }

# 12. Two hosts mapping to one variable name are refused before any host acts.
printf 'node-a ansible_host=127.0.0.1\nnode.a ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts sharing one token variable are refused" "read their cloudflared token from the same environment variable" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" --
[ ! -s "$work/calls" ] || { echo "FAIL: the collision refusal came after a fake was called" >&2; exit 1; }
printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 13. Check mode on a fresh root (no binary): exits 0, reports the stop, writes nothing.
rm -rf "$work/root"; mkdir -p "$work/root/etc/systemd/system" "$work/root/bin" "$work/root/keyrings"
run "check mode on a fresh host reports and stops" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- --check -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
grep -q 'the real converge stages and verifies' "$work/out.log" || { echo "FAIL: the dry run did not report what it cannot see" >&2; exit 1; }
[ ! -e "$work/root/etc/cloudflared/token" ] || { echo "FAIL: a dry run wrote the token" >&2; exit 1; }

echo "cloudflared-connector-check: every case behaved as the role requires"
