#!/bin/sh
# The macos_node role, as converges that must each end the right way.
#
# WHY A GATE OF ITS OWN: the role's whole value is an order (check the
# candidate, drain, replace, bring up) and its refusals, and no other suite
# gives a Mac a billet_config, so every one of them could be deleted with
# everything green. It runs on Linux against a fake billet that records its
# argv and what the configuration held at each call, with the role's
# platform, paths and PATH pointed at the fakes.
#
# What this cannot prove, stated: launchd, tart and the real `local up` and
# `local down` on a Mac; the reference deployment's Mac proves those, and
# cmd/billet's own tests prove the commands.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
. "$here/connector-check-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
mkdir -p "$work/bin"

# THE FAKE BILLET judges nothing: each exit status is the case's to choose, and
# every call records its argv and the configuration's content at that moment,
# which is what proves the drain ran against the configuration the node was
# running and the check against the candidate.
cat >"$work/bin/billet" <<'EOF'
#!/bin/sh
printf 'billet %s\n' "$*" >>"$BILLET_FAKE_CALLS"
config=""; prev=""
for a in "$@"; do
    [ "$prev" = --config ] && config=$a
    prev=$a
done
case "$1 ${2:-}" in
    "check "*)
        cp "$config" "$BILLET_FAKE_STATE/checked" 2>/dev/null || :
        echo "config   $config"
        [ "${BILLET_FAKE_CHECK_RC:-0}" = 0 ] || echo "node: tart: the tart binary is not installed"
        exit "${BILLET_FAKE_CHECK_RC:-0}"
        ;;
    "local down")
        cp "$config" "$BILLET_FAKE_STATE/at-down" 2>/dev/null || :
        [ "${BILLET_FAKE_DOWN_RC:-0}" = 0 ] || { echo "stop     sh.billet.node did not stop" >&2; exit "$BILLET_FAKE_DOWN_RC"; }
        rm -f "$BILLET_FAKE_STATE/running"
        echo "stop     sh.billet.node"
        ;;
    "local up")
        cp "$config" "$BILLET_FAKE_STATE/at-up" 2>/dev/null || :
        [ "${BILLET_FAKE_UP_RC:-0}" = 0 ] || { echo "refuse   the loaded sh.billet.node differs from its plist" >&2; exit "$BILLET_FAKE_UP_RC"; }
        if [ -f "$BILLET_FAKE_STATE/running" ] || [ -n "${BILLET_FAKE_RESTARTED:-}" ]; then
            echo "start    sh.billet.node is already running; left alone (a restart is a drain)"
            echo "enable   sh.billet.node is already enabled"
        else
            : >"$BILLET_FAKE_STATE/running"
            echo "start    sh.billet.node"
            echo "         the same pid survived a settle window"
            echo "enable   sh.billet.node (in the gui domain)"
        fi
        echo "schedule sh.billet.upgrade is installed and loaded"
        ;;
esac
exit 0
EOF
chmod +x "$work/bin/billet"

# THE FAKE ls answers only the role's `ls -lde`: a listing, with an access
# control entry under the macOS format when the case asks for one.
cat >"$work/bin/ls" <<'EOF'
#!/bin/sh
[ "${BILLET_FAKE_LS_RC:-0}" = 0 ] || { echo "ls: cannot read" >&2; exit "$BILLET_FAKE_LS_RC"; }
shift
for p in "$@"; do
    echo "-rw-------  1 node  staff  64 Sep 28 04:21 $p"
    [ -n "${BILLET_FAKE_ACL:-}" ] && echo " 0: group:everyone allow write"
done
exit 0
EOF
chmod +x "$work/bin/ls"

cat >"$work/play.yml" <<'EOF'
- name: macos node
  hosts: all
  connection: local
  gather_facts: false
  vars:
    billet_macos_node_platform: "{{ lookup('env', 'BILLET_TEST_PLATFORM') | default('Darwin', true) }}"
    billet_macos_node_config_path: "{{ lookup('env', 'BILLET_TEST_ROOT') }}/etc/billet/billet.yaml"
    billet_macos_node_binary: "{{ lookup('env', 'BILLET_TEST_BINARY') }}"
    billet_macos_node_path: "{{ lookup('env', 'BILLET_TEST_BIN') }}:/usr/bin:/bin"
    billet_macos_node_poll: 1
  roles:
    - role: junioryono.billet.macos_node
EOF

cat >"$work/config-a.yml" <<'EOF'
billet_config:
  node:
    name: mac-1
    server_addr: 10.0.0.1:7717
    provider: tart
EOF
cat >"$work/config-b.yml" <<'EOF'
billet_config:
  node:
    name: mac-1
    server_addr: 10.0.0.1:7717
    provider: tart
    max_vcpu: 16
EOF
printf 'mac-1 ansible_host=127.0.0.1\n' >"$work/inventory.ini"
conf=$work/root/etc/billet/billet.yaml

# run <name> <expect> [ENV=value ...] -- [ansible-playbook args]
#
# The root and the fake's state persist between cases unless a case resets
# them, because the idempotence and the change cases converge the Mac a case
# earlier left behind.
run() {
    name=$1; expect=$2; shift 2
    envs=""
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do envs="$envs $1"; shift; done
    [ "$#" -gt 0 ] && shift
    : >"$work/calls"
    rm -f "$work/state/checked" "$work/state/at-down" "$work/state/at-up"
    status=0
    # shellcheck disable=SC2086
    env BILLET_FAKE_CALLS="$work/calls" BILLET_FAKE_STATE="$work/state" \
        BILLET_TEST_ROOT="$work/root" BILLET_TEST_BIN="$work/bin" BILLET_TEST_BINARY="$work/bin/billet" \
        ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1 \
        $envs \
        ansible-playbook -i "$work/inventory.ini" --diff -v "$@" "$work/play.yml" \
        >"$work/out.log" 2>&1 || status=$?
    judge "$name" "$status" "$work/out.log" "$expect" "${BILLET_TEST_STAGED:-}"
    BILLET_TEST_STAGED=
}

# A SET-UP MAC: billet installed, a configuration the setup wrote, and a
# running node, which is what docs/deploying/mac-tart.md leaves behind.
fresh() {
    rm -rf "$work/root" "$work/state"
    mkdir -p "$work/root/etc/billet" "$work/state"
    chmod 0755 "$work/root/etc/billet"
    printf 'node:\n  name: mac-1\n  provider: tart\n' >"$conf"
    chmod 0600 "$conf"
    : >"$work/state/running"
}

calls_are() {
    got=$(sed 's/ --config .*//' "$work/calls" | tr '\n' ';')
    [ "$got" = "$1" ] || { echo "FAIL: $2: billet was called as [$got], want [$1]" >&2; exit 1; }
}

no_candidate() {
    for f in "$work/root/etc/billet"/.billet.yaml.candidate*; do
        if [ -e "$f" ]; then echo "FAIL: $1: the candidate $f was left behind" >&2; exit 1; fi
    done
}

# is_config <file> <max_vcpu or none>: the file is, byte for byte, what
# to_nice_yaml(indent=2, sort_keys=false) renders for config-a (or config-b):
# the host role's billet.yaml.j2, key order and trailing newline included
# (measured with ansible-core's filter, 2026-09-28).
is_config() {
    printf 'node:\n  name: mac-1\n  server_addr: 10.0.0.1:7717\n  provider: tart\n' >"$work/want.yaml"
    [ "$2" = none ] || printf '  max_vcpu: %s\n' "$2" >>"$work/want.yaml"
    cmp -s "$1" "$work/want.yaml" || { echo "FAIL: $1 is not the rendered billet_config: $(cat "$1")" >&2; exit 1; }
}

# 1. No billet_config: nothing asked of billet, nothing changed.
fresh
cp "$conf" "$work/before.yaml"
run "no billet_config does nothing" pass --
[ ! -s "$work/calls" ] || { echo "FAIL: a Mac with no billet_config called billet: $(cat "$work/calls")" >&2; exit 1; }
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: a Mac with no billet_config changed something" >&2; exit 1; }
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a Mac with no billet_config had its configuration changed" >&2; exit 1; }

# 2. A Mac whose configuration is missing: nothing can tell whether a node is
#    running from a file that was removed, so nothing is asked or written.
rm -f "$conf"
run "a missing configuration is refused" "cannot be told apart from a running node" -- -e "@$work/config-a.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a missing configuration reached billet: $(cat "$work/calls")" >&2; exit 1; }
[ ! -e "$conf" ] || { echo "FAIL: a missing configuration was written" >&2; exit 1; }

# 3. The inventory's configuration over the setup's: checked as a candidate,
#    drained against the setup's, installed 0600, brought up.
fresh
cp "$conf" "$work/old.yaml"
run "a changed configuration drains, replaces and brings up" pass -- -e "@$work/config-a.yml"
calls_are "billet check;billet local down;billet local up;" "a changed configuration"
is_config "$conf" none
cmp -s "$work/state/checked" "$conf" || { echo "FAIL: the check was not given the configuration that was installed" >&2; exit 1; }
cmp -s "$work/state/at-down" "$work/old.yaml" || { echo "FAIL: the drain did not run against the configuration the node was running" >&2; exit 1; }
cmp -s "$work/state/at-up" "$conf" || { echo "FAIL: the node was brought up on another configuration" >&2; exit 1; }
[ "$(stat -c %a "$conf" 2>/dev/null || stat -f %Lp "$conf")" = 600 ] || { echo "FAIL: the configuration is not 0600" >&2; exit 1; }
kept=false
for f in "$work/root/etc/billet"/billet.yaml.*~; do
    if [ -f "$f" ] && cmp -s "$f" "$work/old.yaml"; then kept=true; fi
done
[ "$kept" = true ] || { echo "FAIL: the previous configuration was not kept" >&2; ls -la "$work/root/etc/billet" >&2; exit 1; }
no_candidate "a changed configuration"

# 4. The same configuration again: `local up` alone, and the run is changed=0.
run "an unchanged configuration only brings the node up" pass -- -e "@$work/config-a.yml"
calls_are "billet local up;" "an unchanged configuration"
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: an unchanged configuration was not changed=0" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }

# 5. A candidate billet refuses: no drain, the configuration untouched.
cp "$conf" "$work/before.yaml"
run "a refused candidate drains nothing" "The node was not stopped" BILLET_FAKE_CHECK_RC=1 -- -e "@$work/config-b.yml"
calls_are "billet check;" "a refused candidate"
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a refused candidate changed the configuration" >&2; exit 1; }
grep -q 'tart binary is not installed' "$work/out.log" || { echo "FAIL: the refusal did not quote the check" >&2; exit 1; }
no_candidate "a refused candidate"

# 6. A drain that fails: nothing replaced and nothing brought up.
run "a failed drain replaces nothing" "is unchanged" BILLET_FAKE_DOWN_RC=1 -- -e "@$work/config-b.yml"
calls_are "billet check;billet local down;" "a failed drain"
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a failed drain was followed by a replacement" >&2; exit 1; }
no_candidate "a failed drain"

# 7. A dry run of a change: the diff, and billet is never asked anything.
run "a dry run diffs and runs nothing" pass -- --check -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a dry run called billet: $(cat "$work/calls")" >&2; exit 1; }
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a dry run changed the configuration" >&2; exit 1; }
grep -q '^+  max_vcpu: 16$' "$work/out.log" || { echo "FAIL: a dry run printed no diff" >&2; exit 1; }
grep -q 'runs `billet local down`' "$work/out.log" || { echo "FAIL: a dry run did not say a converge would drain" >&2; exit 1; }

# 8. `local up` refusing after the replacement: the run fails, says where the
#    previous configuration is, and does not claim the node is stopped.
BILLET_TEST_STAGED=staged run "a node that does not come up is reported with the backup" "The previous configuration is at" \
    BILLET_FAKE_UP_RC=1 -- -e "@$work/config-b.yml"
calls_are "billet check;billet local down;billet local up;" "a node that did not come up"
grep -q 'differs from its plist' "$work/out.log" || { echo "FAIL: the refusal did not quote local up" >&2; exit 1; }
grep -q 'billet local status' "$work/out.log" || { echo "FAIL: the refusal did not name local status" >&2; exit 1; }
is_config "$conf" 16

# 9. Refusals before anything is asked of billet.
fresh
run "a host that is not a Mac is refused" "reports Linux" BILLET_TEST_PLATFORM=Linux -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a Linux host reached billet" >&2; exit 1; }

run "a Mac with no billet is refused" "never installs or moves billet" "BILLET_TEST_BINARY=$work/absent/billet" -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a Mac with no billet reached billet" >&2; exit 1; }

chmod 0775 "$work/root/etc/billet"
run "a configuration directory others can write is refused" "writable by its group or by others" -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a writable directory reached billet" >&2; exit 1; }
chmod 0755 "$work/root/etc/billet"

chmod 0620 "$conf"
run "a configuration others can write is refused" "writable by nobody else" -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a writable configuration reached billet" >&2; exit 1; }
chmod 0600 "$conf"

run "an access control entry is refused" "carries an access control entry" BILLET_FAKE_ACL=1 -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a configuration with an ACL reached billet" >&2; exit 1; }
grep -q 'group:everyone allow write' "$work/out.log" || { echo "FAIL: the ACL refusal did not quote the entry" >&2; exit 1; }

run "an unreadable ACL listing is refused" "could not be listed" BILLET_FAKE_LS_RC=1 -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: an unreadable ACL listing reached billet" >&2; exit 1; }

# 10. Something starting the node between the drain and the install: the
#     final `up` finds it running on the replaced configuration, and the run
#     must fail rather than report a converged node.
BILLET_TEST_STAGED=staged run "a node restarted during the converge is refused" "something started it between the drain and the install" \
    BILLET_FAKE_RESTARTED=1 -- -e "@$work/config-b.yml"
calls_are "billet check;billet local down;billet local up;" "a node restarted during the converge"

echo "macos-node-check: every case behaved as the role requires"
