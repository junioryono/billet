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
        if [ -f "$BILLET_FAKE_STATE/running" ]; then
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

fresh() {
    rm -rf "$work/root" "$work/state"
    mkdir -p "$work/root/etc/billet" "$work/state"
    chmod 0755 "$work/root/etc/billet"
}

calls_are() {
    got=$(sed 's/ --config .*//' "$work/calls" | tr '\n' ';')
    [ "$got" = "$1" ] || { echo "FAIL: $2: billet was called as [$got], want [$1]" >&2; exit 1; }
}

no_candidate() {
    [ ! -e "$work/root/etc/billet/.billet.yaml.candidate" ] || { echo "FAIL: $1: the candidate was left behind" >&2; exit 1; }
}

# 1. No billet_config: nothing asked of billet, nothing changed.
fresh
run "no billet_config does nothing" pass --
[ ! -s "$work/calls" ] || { echo "FAIL: a Mac with no billet_config called billet: $(cat "$work/calls")" >&2; exit 1; }
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: a Mac with no billet_config changed something" >&2; exit 1; }
[ ! -e "$conf" ] || { echo "FAIL: a Mac with no billet_config was given one" >&2; exit 1; }

# 2. A first configuration: checked as a candidate, installed 0600, brought up;
#    nothing to drain, because no configuration was running.
fresh
run "a first configuration is checked, installed and brought up" pass -- -e "@$work/config-a.yml"
calls_are "billet check;billet local up;" "a first configuration"
grep -q 'max_vcpu' "$conf" && { echo "FAIL: the first configuration is the wrong one" >&2; exit 1; }
grep -q '^    name: mac-1$' "$conf" || { echo "FAIL: the configuration is not the rendered billet_config: $(cat "$conf")" >&2; exit 1; }
cmp -s "$work/state/checked" "$conf" || { echo "FAIL: the check was not given the configuration that was installed" >&2; exit 1; }
[ "$(stat -c %a "$conf" 2>/dev/null || stat -f %Lp "$conf")" = 600 ] || { echo "FAIL: the configuration is not 0600" >&2; exit 1; }
no_candidate "a first configuration"

# 3. The same configuration again: `local up` alone, and the run is changed=0.
run "an unchanged configuration only brings the node up" pass -- -e "@$work/config-a.yml"
calls_are "billet local up;" "an unchanged configuration"
[ "$(recap_changed_of "$work/out.log")" = 0 ] || { echo "FAIL: an unchanged configuration was not changed=0" >&2; sed -n '/PLAY RECAP/,$p' "$work/out.log" >&2; exit 1; }

# 4. A changed configuration: checked, then drained AGAINST THE OLD ONE, then
#    replaced (the old one kept as a backup), then brought up on the new one.
cp "$conf" "$work/old.yaml"
run "a changed configuration drains, replaces and brings up" pass -- -e "@$work/config-b.yml"
calls_are "billet check;billet local down;billet local up;" "a changed configuration"
cmp -s "$work/state/at-down" "$work/old.yaml" || { echo "FAIL: the drain did not run against the configuration the node was running" >&2; exit 1; }
grep -q 'max_vcpu: 16' "$work/state/checked" || { echo "FAIL: the check did not see the new configuration" >&2; exit 1; }
grep -q 'max_vcpu: 16' "$work/state/at-up" || { echo "FAIL: the node was brought up on the old configuration" >&2; exit 1; }
kept=false
for f in "$work/root/etc/billet"/billet.yaml.*~; do
    [ -f "$f" ] && cmp -s "$f" "$work/old.yaml" && kept=true
done
[ "$kept" = true ] || { echo "FAIL: the previous configuration was not kept" >&2; ls -la "$work/root/etc/billet" >&2; exit 1; }
no_candidate "a changed configuration"

# 5. A candidate billet refuses: no drain, the configuration untouched.
cp "$conf" "$work/before.yaml"
run "a refused candidate drains nothing" "The node was not stopped" BILLET_FAKE_CHECK_RC=1 -- -e "@$work/config-a.yml"
calls_are "billet check;" "a refused candidate"
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a refused candidate changed the configuration" >&2; exit 1; }
grep -q 'tart binary is not installed' "$work/out.log" || { echo "FAIL: the refusal did not quote the check" >&2; exit 1; }
no_candidate "a refused candidate"

# 6. A drain that fails: nothing replaced and nothing brought up.
run "a failed drain replaces nothing" "is unchanged" BILLET_FAKE_DOWN_RC=1 -- -e "@$work/config-a.yml"
calls_are "billet check;billet local down;" "a failed drain"
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a failed drain was followed by a replacement" >&2; exit 1; }

# 7. `local up` refusing after the replacement: the run fails and says where
#    the previous configuration is.
BILLET_TEST_STAGED=staged run "a node that does not come up is reported with the backup" "the previous configuration is at" \
    BILLET_FAKE_UP_RC=1 -- -e "@$work/config-a.yml"
calls_are "billet check;billet local down;billet local up;" "a node that did not come up"
grep -q 'differs from its plist' "$work/out.log" || { echo "FAIL: the refusal did not quote local up" >&2; exit 1; }

# 8. A dry run of a change: the diff, and billet is never asked anything.
cp "$conf" "$work/before.yaml"
run "a dry run diffs and runs nothing" pass -- --check -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a dry run called billet: $(cat "$work/calls")" >&2; exit 1; }
cmp -s "$conf" "$work/before.yaml" || { echo "FAIL: a dry run changed the configuration" >&2; exit 1; }
grep -q '+    max_vcpu: 16' "$work/out.log" || { echo "FAIL: a dry run printed no diff" >&2; exit 1; }
grep -q 'runs `billet local down`' "$work/out.log" || { echo "FAIL: a dry run did not say a converge would drain" >&2; exit 1; }

# 9. Refusals before anything is asked of billet.
run "a host that is not a Mac is refused" "reports Linux" BILLET_TEST_PLATFORM=Linux -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a Linux host reached billet" >&2; exit 1; }

run "a Mac with no billet is refused" "never installs or moves billet" "BILLET_TEST_BINARY=$work/absent/billet" -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a Mac with no billet reached billet" >&2; exit 1; }

chmod 0775 "$work/root/etc/billet"
run "a configuration directory others can write is refused" "writable by its group or by others" -- -e "@$work/config-b.yml"
[ ! -s "$work/calls" ] || { echo "FAIL: a writable directory reached billet" >&2; exit 1; }
chmod 0755 "$work/root/etc/billet"

echo "macos-node-check: every case behaved as the role requires"
