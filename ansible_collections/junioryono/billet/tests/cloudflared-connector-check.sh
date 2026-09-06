#!/bin/sh
# The cloudflared_connector role, as converges that must each end the right way.
#
# WHY A GATE OF ITS OWN. Nothing else in the suite sets a connector token, so
# every other gate takes the "no token, nothing done" branch and each of the
# role's refusals could be deleted with everything green. The refusals are the
# role: a token for another tunnel, a change that would restart the connector
# the play may be riding, a signing key that is not the pinned one, a variable
# name two hosts share.
#
# WITHOUT ROOT OR A NETWORK. The role's paths and the file owner are inputs and
# the tests point them at a temporary tree and the current user; `-e
# ansible_become=false` outranks the tasks' become keyword; the apt path is
# switched off except where a refusal must precede it; and the fakes on PATH
# (connector-check-lib.sh) record every call so the gate can prove what was and
# was not run, and that the token reached none of them in argv.
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
me=$(id -un)
mygroup=$(id -gn)

# Tokens are base64({"a": account, "s": secret, "t": tunnel}). The whole
# encoded token is what a leak would carry, so the whole encoded token is what
# the gate greps for, fixed-string, in every fake's argv, in Ansible's output
# and in the rendered unit; a marker inside the JSON would be invisible once
# encoded (measured: the first version of this gate could not fire).
tunnel_a=11111111-1111-1111-1111-111111111111
tunnel_b=22222222-2222-2222-2222-222222222222
mk_token() { printf '{"a":"acct","s":"secret-%s","t":"%s"}' "$2" "$1" | base64 | tr -d '\n'; }
token_a=$(mk_token "$tunnel_a" one)
token_a2=$(mk_token "$tunnel_a" two)
token_b=$(mk_token "$tunnel_b" three)

cat >"$work/play.yml" <<'EOF'
- name: cloudflared connector
  hosts: all
  connection: local
  gather_facts: false
  serial: "{{ lookup('env', 'BILLET_TEST_SERIAL') | default('100%', true) }}"
  vars:
    billet_cloudflared_manage_apt: "{{ lookup('env', 'BILLET_TEST_MANAGE_APT') | default('false', true) | bool }}"
    billet_cloudflared_prerequisites: []
    billet_cloudflared_owner: "{{ lookup('env', 'BILLET_TEST_USER') }}"
    billet_cloudflared_group: "{{ lookup('env', 'BILLET_TEST_GROUP') }}"
    billet_cloudflared_signing_key_url: "file://{{ lookup('env', 'BILLET_TEST_KEY_FILE') }}"
    billet_cloudflared_config_dir: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/etc/cloudflared"
    billet_cloudflared_unit_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/etc/systemd/system/cloudflared.service"
    billet_cloudflared_stage_dir: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/stage"
    billet_cloudflared_keyring_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/keyrings/cloudflare-main.gpg"
    billet_cloudflared_binary: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/bin/cloudflared"
  roles:
    - role: junioryono.billet.cloudflared_connector
EOF

# The same play with the variable name set on the role invocation, a scope
# hostvars does not see: the play-wide list shows distinct fallbacks while
# every host reads the one name, and only the per-host consistency check
# catches it.
sed -e 's/^    - role: junioryono.billet.cloudflared_connector$/    - role: junioryono.billet.cloudflared_connector\n      vars:\n        billet_cloudflared_token_env: BILLET_CLOUDFLARED_TOKEN_NODE_A/' "$work/play.yml" >"$work/play-rolevar.yml"

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
        BILLET_TEST_USER="$me" BILLET_TEST_GROUP="$mygroup" \
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

# The token must be in the token file and nowhere else: not in any fake's
# argv, not in Ansible's output (run with --diff -v, the most a callback
# prints), not in the rendered unit.
no_secret_leaked() {
    for t in "$token_a" "$token_a2" "$token_b"; do
        if grep -Fq -- "$t" "$work/calls"; then
            echo "FAIL $1: a token reached a command's argv: $(grep -F -- "$t" "$work/calls")" >&2; exit 1
        fi
        if grep -Fq -- "$t" "$work/out.log"; then
            echo "FAIL $1: a token appears in Ansible's output" >&2; exit 1
        fi
        if [ -f "$work/root/etc/systemd/system/cloudflared.service" ] && grep -Fq -- "$t" "$work/root/etc/systemd/system/cloudflared.service"; then
            echo "FAIL $1: a token appears in the rendered unit" >&2; exit 1
        fi
    done
}

# The installer's unit, byte for byte (cmd/cloudflared/linux_service.go's
# template with the binary path and the token-file arguments filled in), so a
# host `cloudflared service install` set up converges to changed=0.
expected_unit() {
    printf '[Unit]\nDescription=Cloudflare Tunnel client\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nTimeoutStartSec=15\nType=notify\nExecStart=%s --no-autoupdate tunnel run --token-file %s\nRestart=on-failure\nRestartSec=5s\n\n[Install]\nWantedBy=multi-user.target\n' \
        "$work/root/bin/cloudflared" "$work/root/etc/cloudflared/token"
}

# The wait asked the journal for exactly the invocation systemd runs now, and
# nothing else: an old window or a guessed id would not be this.
awaited_current_invocation() {
    current=$(cat "$work/state/unit-active")
    [ -n "$current" ] || { echo "FAIL $1: the fake systemd holds no invocation" >&2; exit 1; }
    grep -q "^journalctl --no-pager _SYSTEMD_INVOCATION_ID=$current\$" "$work/calls" || { echo "FAIL $1: the registration was not awaited on the current invocation $current: $(grep '^journalctl' "$work/calls")" >&2; exit 1; }
    if grep '^journalctl' "$work/calls" | grep -qv "_SYSTEMD_INVOCATION_ID=$current"; then echo "FAIL $1: the journal was read other than by the current invocation: $(grep '^journalctl' "$work/calls")" >&2; exit 1; fi
}

restore_installed() {
    rm -rf "$work/root" "$work/state"; cp -R "$work/root-installed" "$work/root"; cp -R "$work/state-installed" "$work/state"
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

# 3. A token with no expected tunnel to check it against is refused, before any
#    fake is called: the default must not skip the one check that matters.
run "a token with no expected tunnel is refused" "The id is what proves the token is this host's" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" --
[ ! -s "$work/calls" ] || { echo "FAIL: the missing-id refusal came after a fake was called: $(cat "$work/calls")" >&2; exit 1; }

# 4. A malformed token fails before anything is written.
run "a malformed token fails before any write" "is not a base64 token" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=not-base64-json" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ ! -e "$work/root/etc/cloudflared/token" ] || { echo "FAIL: a malformed token was written" >&2; exit 1; }

# 5. The right token on a fresh root: the token file 0600 holding the bytes
#    verbatim, the unit byte-equal to the installer's, the service started, the
#    registration awaited on the invocation systemd started, no secret leaked.
run "the right token installs the connector" pass \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
tok=$work/root/etc/cloudflared/token
[ -f "$tok" ] || { echo "FAIL: no token file was written" >&2; exit 1; }
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: the token file does not hold the token" >&2; exit 1; }
[ "$(wc -c <"$tok" | tr -d ' ')" = "${#token_a}" ] || { echo "FAIL: the token file is not the token's bytes verbatim (the installer writes no newline)" >&2; exit 1; }
# GNU FIRST: on coreutils `stat -f` is the filesystem form and succeeds, so a
# BSD-first fallback never ran on Linux and compared filesystem statistics
# against 600 (measured, ubuntu:24.04); BSD stat refuses -c and falls through.
mode=$(stat -c '%a' "$tok" 2>/dev/null || stat -f '%Lp' "$tok")
[ "$mode" = 600 ] || { echo "FAIL: the token file is mode $mode, want 600" >&2; exit 1; }
unit=$work/root/etc/systemd/system/cloudflared.service
expected_unit >"$work/expected.service"
cmp -s "$unit" "$work/expected.service" || { echo "FAIL: the rendered unit is not byte-equal to the installer's:" >&2; diff "$work/expected.service" "$unit" >&2; exit 1; }
grep -q '^systemctl .*daemon-reload' "$work/calls" || { echo "FAIL: systemd was not reloaded after the unit was written: $(cat "$work/calls")" >&2; exit 1; }
awaited_current_invocation "the right token installs the connector"
no_secret_leaked "the right token installs the connector"
cp -R "$work/root" "$work/root-installed"; cp -R "$work/state" "$work/state-installed"

# 6. The same token again: nothing changes and nothing waits.
restore_installed
run "an unchanged token changes nothing" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: an unchanged token reported changed=$(recap_changed_of "$work/out.log")" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
if grep -q '^journalctl' "$work/calls"; then echo "FAIL: the registration wait ran on an unchanged host" >&2; exit 1; fi
if grep -qE '^systemctl .*(restart|daemon-reload)' "$work/calls"; then echo "FAIL: an unchanged token restarted or reloaded: $(cat "$work/calls")" >&2; exit 1; fi

# 7. A rotated token with nothing said about the transport: could-not-tell, refused.
restore_installed
run "a rotation with an unknown transport is refused" "nothing says whether this play reaches the host" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: a refused rotation rewrote the token" >&2; exit 1; }
grep -q 'would rewrite the credential and restart the running connector' "$work/out.log" || { echo "FAIL: the refusal did not name the credential as what would change" >&2; exit 1; }
no_secret_leaked "a rotation with an unknown transport is refused"

# 8. A rotated token with the transport declared true: refused.
restore_installed
run "a rotation over the play's own transport is refused" "reaches the host through that connector" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=true

# 9. An answer that is neither true nor false is refused, not read as empty.
restore_installed
run "a transport answer that is not true or false is refused" "it must be true, false, or left empty" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=yes -e billet_cloudflared_routed_address=10.9.9.9
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: an unreadable transport answer let the rotation through" >&2; exit 1; }

# 10. The transport derived from a routed address equal to ansible_host: refused.
restore_installed
run "a rotation is refused when ansible_host is the routed address" "reaches the host through that connector" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_routed_address=127.0.0.1

# 11. A routed address declared but no ansible_host to compare it with: could-not-tell.
restore_installed
printf 'node-a\n' >"$work/inventory.ini"
run "a rotation is refused when ansible_host is not declared" "nothing says whether this play reaches the host" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_routed_address=10.9.9.9
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: an undeclared ansible_host let the rotation through" >&2; exit 1; }

# 12. A name against an address: two spellings can name one address, so
#     could-not-tell rather than false.
restore_installed
printf 'node-a ansible_host=localhost\n' >"$work/inventory.ini"
run "a rotation is refused when ansible_host is a name" "nothing says whether this play reaches the host" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_routed_address=127.0.0.1
[ "$(cat "$tok")" = "$token_a" ] || { echo "FAIL: a name for ansible_host let the rotation through" >&2; exit 1; }
printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 13. The routed address declared and ansible_host a different IP literal: the
#     rotation applies, the service restarts, the registration is awaited on the
#     new invocation, no secret leaked.
restore_installed
run "a rotation applies when the play reaches the host another way" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_routed_address=10.9.9.9
[ "$(cat "$tok")" = "$token_a2" ] || { echo "FAIL: the rotation did not rewrite the token" >&2; exit 1; }
grep -q '^systemctl .*restart' "$work/calls" || { echo "FAIL: the rotation did not restart the connector: $(cat "$work/calls")" >&2; exit 1; }
awaited_current_invocation "a rotation applies when the play reaches the host another way"
no_secret_leaked "a rotation applies when the play reaches the host another way"

# 14. An explicit false applies the rotation whatever the address says.
restore_installed
run "an explicit transport false applies the rotation" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a2" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=false -e billet_cloudflared_routed_address=127.0.0.1
[ "$(cat "$tok")" = "$token_a2" ] || { echo "FAIL: an explicit false did not apply the rotation" >&2; exit 1; }

# 15. THE UNIT IS GUARDED LIKE THE CREDENTIAL. A unit that drifted from the
#     rendered one (a comment, an edit, a different binary path) would be
#     rewritten and the connector restarted; with the token unchanged and the
#     transport unknown that is refused, and named as the unit.
restore_installed
printf '# drift\n' >>"$unit"
run "a drifted unit is not rewritten under an unknown transport" "would rewrite the unit and restart the running connector" BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
grep -q '^# drift$' "$unit" || { echo "FAIL: a refused unit rewrite rewrote the unit" >&2; exit 1; }
if grep -qE '^systemctl .*(restart|daemon-reload)' "$work/calls"; then echo "FAIL: a refused unit rewrite restarted or reloaded: $(cat "$work/calls")" >&2; exit 1; fi

# 16. The same drift with the transport definitely not this connector: the unit
#     is restored, reloaded, the connector restarted and the registration awaited.
restore_installed
printf '# drift\n' >>"$unit"
run "a drifted unit is restored when the play reaches the host another way" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=false
cmp -s "$unit" "$work/expected.service" || { echo "FAIL: the drifted unit was not restored" >&2; exit 1; }
grep -q '^systemctl .*daemon-reload' "$work/calls" || { echo "FAIL: the restored unit was not reloaded" >&2; exit 1; }
grep -q '^systemctl .*restart' "$work/calls" || { echo "FAIL: the restored unit did not restart the connector" >&2; exit 1; }
awaited_current_invocation "a drifted unit is restored when the play reaches the host another way"

# 16b. A connector still activating (a long ExecStartPost) is a running one:
#      the drift is refused under an unknown transport exactly as for active.
restore_installed
printf '# drift\n' >>"$unit"
run "a drifted unit is not rewritten while the connector is activating" "would rewrite the unit and restart the running connector" BILLET_TEST_KEEP_ROOT=1 \
    BILLET_FAKE_ACTIVE_STATE=activating "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
grep -q '^# drift$' "$unit" || { echo "FAIL: a refused unit rewrite rewrote the unit while activating" >&2; exit 1; }

# 16c. A unit an older installer wrote with the token in ExecStart is adopted
#      under a definite false, and neither the old unit's token nor the new
#      one appears anywhere but the token file.
restore_installed
printf '[Service]\nExecStart=%s --no-autoupdate tunnel run --token %s\n' "$work/root/bin/cloudflared" "$token_b" >"$unit"
run "a unit carrying an inline token is replaced without disclosing it" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=false
cmp -s "$unit" "$work/expected.service" || { echo "FAIL: the inline-token unit was not replaced" >&2; exit 1; }
no_secret_leaked "a unit carrying an inline token is replaced without disclosing it"

# 16d. The same adoption in a dry run, which rewrites nothing: the service
#      task reads the unit systemd still runs, ExecStart and token included,
#      and that must not reach the output either.
restore_installed
printf '[Service]\nExecStart=%s --no-autoupdate tunnel run --token %s\n' "$work/root/bin/cloudflared" "$token_b" >"$unit"
run "a dry run over a unit carrying an inline token discloses nothing" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- --check -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_carries_ansible_transport=false
grep -Fq -- "--token $token_b" "$unit" || { echo "FAIL: a dry run rewrote the unit" >&2; exit 1; }
# The service task did run and read the unit's status (a skipped task would
# disclose nothing and prove nothing): the module's bare `show` is the record.
grep -qE '^systemctl show cloudflared' "$work/calls" || { echo "FAIL: the dry run never asked systemd about the unit, so the disclosure was not exercised: $(grep '^systemctl' "$work/calls")" >&2; exit 1; }
# The unit still carries it by construction; argv and the output must not.
if grep -Fq -- "$token_b" "$work/calls"; then echo "FAIL: the adopted unit's token reached a command's argv" >&2; exit 1; fi
if grep -Fq -- "$token_b" "$work/out.log"; then echo "FAIL: the adopted unit's token appears in the dry run's output" >&2; exit 1; fi

# 16e. A connector caught deactivating with nothing to rewrite is started, and
#      that start is awaited on its invocation like any other.
restore_installed
run "a start from deactivating awaits the registration" pass BILLET_TEST_KEEP_ROOT=1 \
    BILLET_FAKE_ACTIVE_STATE=deactivating "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
grep -q '^systemctl .*start' "$work/calls" || { echo "FAIL: a deactivating connector was not started: $(cat "$work/calls")" >&2; exit 1; }
awaited_current_invocation "a start from deactivating awaits the registration"

# 17. The apt path with a key that is not the pinned one: refused before the
#     keyring copy, the repository and the package.
BILLET_TEST_STAGED=staged run "a signing key with another fingerprint is refused" "not exactly one primary key with the pinned fingerprint" \
    BILLET_TEST_MANAGE_APT=true "BILLET_TEST_KEY_FILE=$work/bad-key.gpg" "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ ! -e "$work/root/keyrings/cloudflare-main.gpg" ] || { echo "FAIL: a refused key reached the keyring" >&2; exit 1; }
grep -q '^gpg .*--show-keys' "$work/calls" || { echo "FAIL: the key was never read by gpg" >&2; exit 1; }

# 18. Two hosts mapping to one variable name are refused before any host acts.
printf 'node-a ansible_host=127.0.0.1\nnode.a ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts sharing one token variable are refused" "read their cloudflared token from the same environment variable" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ ! -s "$work/calls" ] || { echo "FAIL: the collision refusal came after a fake was called" >&2; exit 1; }

# 19. The same through an inventory override of the variable name, which is
#     the hostvars path the collision check reads.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1 billet_cloudflared_token_env=BILLET_CLOUDFLARED_TOKEN_NODE_A\n' >"$work/inventory.ini"
run "two hosts sharing one token variable by override are refused" "read their cloudflared token from the same environment variable" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ ! -s "$work/calls" ] || { echo "FAIL: the override collision refusal came after a fake was called" >&2; exit 1; }

# 19b. A name shared through a scope every host sees (-e) is refused too.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts sharing one token variable through -e are refused" "cloudflared token" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a" -e billet_cloudflared_token_env=BILLET_CLOUDFLARED_TOKEN_NODE_A
[ ! -s "$work/calls" ] || { echo "FAIL: the -e collision refusal came after a fake was called" >&2; exit 1; }

# A name set on the role invocation itself, which the play-wide check cannot
# see: refused by the per-host check, before any fake is called.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1\n' >"$work/inventory.ini"
BILLET_TEST_STAGED=staged BILLET_TEST_PLAY=play-rolevar run "a name the play-wide check cannot see is refused" "a variable the play-wide collision check could not see" \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
# node-a's own name IS the computed one, so it converges; node-b, which reads
# node-a's variable, is the host refused. The staged allowance is for node-a's
# writes; node-b's row must be unchanged.
sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^node-b +: +ok=[0-9]+ +changed=0 .*failed=1' || { echo "FAIL: node-b was not the host refused, unchanged" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -qE '^node-a +: .*failed=0' || { echo "FAIL: node-a, whose name is its own, was refused too" >&2; exit 1; }

# 20. Two distinct hosts under serial: 1 converge, one batch at a time: the
#     collision check reads the inventory, not a fact a later batch has not
#     recorded, so the first batch does not fail on an undefined name.
printf 'node-a ansible_host=127.0.0.1\nnode-b ansible_host=127.0.0.1\n' >"$work/inventory.ini"
run "two hosts converge one batch at a time" pass BILLET_TEST_SERIAL=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" "BILLET_CLOUDFLARED_TOKEN_NODE_B=$token_a" -- -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
[ "$(sed -n '/PLAY RECAP/,$p' "$work/out.log" | grep -cE '^node-[ab] +: +ok=[1-9]')" = 2 ] || { echo "FAIL: both hosts did not converge under serial" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }
printf 'node-a ansible_host=127.0.0.1\n' >"$work/inventory.ini"

# 21. Check mode on a fresh root (no binary): exits 0, reports the stop, writes nothing.
rm -rf "$work/root"; mkdir -p "$work/root/etc/systemd/system" "$work/root/bin" "$work/root/keyrings"
run "check mode on a fresh host reports and stops" pass BILLET_TEST_KEEP_ROOT=1 \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- --check -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
grep -q 'the real converge stages and verifies' "$work/out.log" || { echo "FAIL: the dry run did not report what it cannot see" >&2; exit 1; }
[ ! -e "$work/root/etc/cloudflared/token" ] || { echo "FAIL: a dry run wrote the token" >&2; exit 1; }

# 22. Check mode on a host that has the binary but no staged key (provisioned
#     another way, or its cache cleaned): the apt path reports what it cannot
#     verify and stops rather than failing on a file the dry run never fetched.
run "check mode without a staged key reports and stops" pass BILLET_TEST_MANAGE_APT=true \
    "BILLET_CLOUDFLARED_TOKEN_NODE_A=$token_a" -- --check -e "billet_cloudflared_expected_tunnel_id=$tunnel_a"
grep -q 'fetches and verifies it before trusting the repository' "$work/out.log" || { echo "FAIL: the dry run did not report the unverified key" >&2; exit 1; }
if grep -q '^gpg' "$work/calls"; then echo "FAIL: a dry run read a key it never fetched" >&2; exit 1; fi
[ ! -e "$work/root/etc/cloudflared/token" ] || { echo "FAIL: a dry run wrote the token" >&2; exit 1; }

echo "cloudflared-connector-check: every case behaved as the role requires"
