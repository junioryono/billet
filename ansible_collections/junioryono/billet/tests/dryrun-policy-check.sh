#!/usr/bin/env bash
# Prove the host role's dry run compares across a pending daemon-reload and
# refuses only billet-owned policy that disagrees or cannot be compared.
#
# WHY A GATE OF ITS OWN. On systemd 255 `NeedDaemonReload` answers yes for every
# unit while the manager's unit_file_state_outdated flag is set, which any
# `systemctl enable` on the host (a snap refresh, a package install) does until
# the next reload. The old refusal fired on that flag alone and refused every
# fleet check after such an event until an operator reloaded by hand. The gate
# now compares the facts a dry run reads from the loaded state with the on-disk
# fragment, and every branch of that comparison is exercised here with fake
# `systemctl` and `busctl` answering from fixtures, because no other suite runs
# in check mode against a manager reporting the flag.
#
# `-e ansible_become=false` outranks the tasks' become keyword (a connection
# variable), so the gate runs as the caller against a temporary tree.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
collection=${here%/tests}
collection_root=$(cd "$here/../../../.." && pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

python=""
if command -v ansible >/dev/null 2>&1; then
    python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
fi
if [ -z "$python" ] || [ ! -x "$python" ]; then
    python=$(command -v python3) || { echo "dryrun-policy-check: no python3" >&2; exit 1; }
fi

# --- the module's own resolver, and the notify rule, each self-tested first ---
"$python" "$here/unit_names_check.py"
"$python" "$here/unit_file_notify_check.py" --self-test
"$python" "$here/unit_file_notify_check.py" "$collection/roles/host"

# --- fakes ---------------------------------------------------------------------
#
# `systemctl show` answers from $BILLET_FAKE_SYSTEMD/<unit>.props (Key=Value
# lines); `busctl get-property` answers UnitPath from unitpath.json and a
# unit's ExecStart from <unit>.exec.json, failing when the file is absent.
mkdir -p "$work/bin"
cat >"$work/bin/systemctl" <<'FAKE'
#!/usr/bin/env bash
set -eu
echo "systemctl $*" >>"$BILLET_FAKE_SYSTEMD/calls"
[ "$1" = show ] || { echo "fake systemctl: only show is supported" >&2; exit 64; }
shift
props=(); value=no; unit=""
while [ $# -gt 0 ]; do
    case "$1" in
        --property=*) props+=("${1#--property=}") ;;
        --value) value=yes ;;
        --) shift; unit=$1 ;;
        *) unit=$1 ;;
    esac
    shift
done
file=$BILLET_FAKE_SYSTEMD/$unit.props
for p in "${props[@]}"; do
    v=$(grep -s "^$p=" "$file" | head -n1 | cut -d= -f2- || true)
    if [ "$value" = yes ]; then echo "$v"; else echo "$p=$v"; fi
done
FAKE
cat >"$work/bin/busctl" <<'FAKE'
#!/usr/bin/env bash
set -eu
echo "busctl $*" >>"$BILLET_FAKE_SYSTEMD/calls"
[ "$1" = --json=short ] && [ "$2" = get-property ] || { echo "fake busctl: unsupported" >&2; exit 64; }
prop=$6
case "$prop" in
    UnitPath) cat "$BILLET_FAKE_SYSTEMD/unitpath.json" ;;
    ExecStart)
        label=${4##*/}
        unit=${label//_2d/-}; unit=${unit//_2e/.}
        [ -f "$BILLET_FAKE_SYSTEMD/$unit.exec.json" ] || { echo "Failed to get property ExecStart" >&2; exit 1; }
        cat "$BILLET_FAKE_SYSTEMD/$unit.exec.json" ;;
    *) echo "fake busctl: unknown property $prop" >&2; exit 64 ;;
esac
FAKE
chmod +x "$work/bin/systemctl" "$work/bin/busctl"

cat >"$work/play.yml" <<'PLAY'
---
- name: Exercise the dry-run policy gate
  hosts: all
  gather_facts: false
  tasks:
    - name: Run only the gate
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: dryrun-policy
PLAY

exec_json='{"type":"a(sasbttttuii)","data":[["/usr/bin/billet",["/usr/bin/billet","ROLE","--config","/etc/billet/billet.yaml"],false,0,0,0,0,0,0,0]]}'

# fixture NAME: a lookup tree (etc, run, lib in the manager's order), both
# billet fragments in lib, both loaded exactly as written, the server unit
# reporting the pending flag and the node not.
fixture() {
    local d=$work/$1
    mkdir -p "$d/fake" "$d/etc" "$d/run" "$d/lib"
    printf '{"type":"as","data":["%s","%s","%s"]}\n' "$d/etc" "$d/run" "$d/lib" >"$d/fake/unitpath.json"
    for role in server node; do
        printf '[Unit]\nDescription=billet %s\n\n[Service]\nUser=%s\nGroup=%s\nType=notify\nExecStart=/usr/bin/billet %s --config /etc/billet/billet.yaml\n' "$role" \
            "$([ $role = server ] && echo billet || echo root)" "$([ $role = server ] && echo billet || echo root)" "$role" \
            >"$d/lib/billet-$role.service"
        printf 'NeedDaemonReload=%s\nFragmentPath=%s\nDropInPaths=\nNames=billet-%s.service\nUser=%s\nGroup=%s\nType=notify\n' \
            "$([ $role = server ] && echo yes || echo no)" "$d/lib/billet-$role.service" "$role" \
            "$([ $role = server ] && echo billet || echo root)" "$([ $role = server ] && echo billet || echo root)" \
            >"$d/fake/billet-$role.service.props"
        echo "${exec_json//ROLE/$role}" >"$d/fake/billet-$role.service.exec.json"
    done
    cat >"$d/vars.yml" <<VARS
billet_service_policy_unit_path_allowlist:
  - $d/etc
  - $d/run
  - $d/lib
VARS
}

run() {
    local d=$work/$1
    : >"$d/fake/calls"
    env PATH="$work/bin:$PATH" BILLET_FAKE_SYSTEMD="$d/fake" \
        ANSIBLE_COLLECTIONS_PATH="$collection_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ansible-playbook -i localhost, --connection=local --check -e ansible_become=false -e "@$d/vars.yml" \
        "$work/play.yml" >"$d/out.txt" 2>&1
}

fail() {
    echo "FAIL: $1" >&2
    sed -n '1,80p' "$work/$2/out.txt" >&2
    exit 1
}

# EVERY NON-ZERO RESULT IS A FAILURE, and the message only sharpens the
# diagnosis; an allowed case must SUCCEED, and a refused case must fail with the
# gate's own words, or it proves nothing.
allowed() {
    local name=$1 what=$2
    run "$name" || fail "$what was refused" "$name"
    grep -q 'the run continues' "$work/$name/out.txt" || fail "$what continued without the report" "$name"
    echo "ok   $what"
}

refused() {
    local name=$1 what=$2 words=$3
    if run "$name"; then fail "$what was NOT refused" "$name"; fi
    grep -qF -- "$words" "$work/$name/out.txt" || fail "$what failed without the words [$words]" "$name"
    echo "ok   $what"
}

# 1. Agreement on every fact continues with the report.
fixture agree
allowed agree "a pending reload with the fragment agreeing on User, Group, Type and ExecStart continues"

# 2. No pending reload compares nothing: the loaded identity is never asked for.
fixture quiet
sed -i.bak 's/^NeedDaemonReload=yes/NeedDaemonReload=no/' "$work/quiet/fake/billet-server.service.props"
run quiet || fail "a host with no pending reload was refused" quiet
if grep -q -- '--property=FragmentPath' "$work/quiet/fake/calls"; then
    fail "the loaded identity was inspected with no reload pending" quiet
fi
echo "ok   no pending reload compares nothing"

# 3. Each fact differing refuses, naming the pair.
fixture user
sed -i.bak 's/^User=billet$/User=root/' "$work/user/lib/billet-server.service"
refused user "User differing on disk refuses" "User on disk [root] vs loaded [billet]"

fixture group
sed -i.bak 's/^Group=billet$/Group=root/' "$work/group/lib/billet-server.service"
refused group "Group differing on disk refuses" "Group on disk [root] vs loaded [billet]"

fixture type
sed -i.bak 's/^Type=notify$/Type=notify-reload/' "$work/type/lib/billet-server.service"
refused type "Type differing on disk refuses, literally" "Type on disk [notify-reload] vs loaded [notify]"

fixture execword
sed -i.bak 's|^ExecStart=.*|ExecStart=/usr/bin/billet server --config /etc/billet/other.yaml|' "$work/execword/lib/billet-server.service"
refused execword "an ExecStart word differing on disk refuses" "ExecStart on disk"

# 4. A previously loaded two-argument record is not the ordinary four joined.
fixture tworecord
echo '{"type":"a(sasbttttuii)","data":[["/usr/bin/billet",["/usr/bin/billet","server --config /etc/billet/billet.yaml"],false,0,0,0,0,0,0,0]]}' \
    >"$work/tworecord/fake/billet-server.service.exec.json"
refused tworecord "a loaded record whose boundaries differ refuses although the rendering is equal" "compared word by word"

# 5. The grammar: a tab, a quote, a specifier, a prefix and a repeated line each refuse.
fixture tab
printf '[Service]\nUser=billet\nGroup=billet\nType=notify\nExecStart=/usr/bin/billet server\t--config /etc/billet/billet.yaml\n' >"$work/tab/lib/billet-server.service"
refused tab "an interior tab in the on-disk ExecStart refuses" "not one line this dry run can compare"

# THE FRAGMENT IS READ AS SYSTEMD READS IT: an assignment outside [Service] is
# not the service's, a continued line swallows the one after it, and a stray
# carriage return or indentation is outside the templates' grammar.
fixture wrongsection
printf '[Unit]\nUser=billet\n\n[Service]\nGroup=billet\nType=notify\nExecStart=/usr/bin/billet server --config /etc/billet/billet.yaml\n' >"$work/wrongsection/lib/billet-server.service"
refused wrongsection "User= under [Unit] refuses although its text equals the loaded value" "User= outside [Service]"

fixture continued
printf '[Service]\nDocumentation=man:billet \\\nUser=billet\nGroup=billet\nType=notify\nExecStart=/usr/bin/billet server --config /etc/billet/billet.yaml\n' >"$work/continued/lib/billet-server.service"
refused continued "a continued line before User= refuses" "a continued line"

fixture crlf
printf '[Service]\r\nUser=billet\r\nGroup=billet\r\nType=notify\r\nExecStart=/usr/bin/billet server --config /etc/billet/billet.yaml\r\n' >"$work/crlf/lib/billet-server.service"
refused crlf "a carriage return in the fragment refuses" "a carriage return"

fixture indented
printf '[Service]\n  User=billet\nGroup=billet\nType=notify\nExecStart=/usr/bin/billet server --config /etc/billet/billet.yaml\n' >"$work/indented/lib/billet-server.service"
refused indented "an indented assignment refuses" "an indented line"

fixture spaced
printf '[Service]\nUser = billet\nGroup=billet\nType=notify\nExecStart=/usr/bin/billet server --config /etc/billet/billet.yaml\n' >"$work/spaced/lib/billet-server.service"
refused spaced "whitespace around = on a compared key refuses rather than being read as another key" "whitespace around ="

# THE SHIPPED POLICY PASSES: the packaged units, with their [Unit] and [Install]
# assignments, comments and an empty CapabilityBoundingSet=, and the role's
# server template rendered with a ledger volume (a repeated After=), an
# environment file and a quoted ReadWritePaths=, each agree with a manager that
# loaded them as written.
fixture packaged
cp "$collection_root/deploy/billet-server.service" "$work/packaged/lib/billet-server.service"
cp "$collection_root/deploy/billet-node.service" "$work/packaged/lib/billet-node.service"
sed -i.bak 's/^NeedDaemonReload=no/NeedDaemonReload=yes/' "$work/packaged/fake/billet-node.service.props"
allowed packaged "the packaged server and node units pass"
grep -q 'billet-node.service: systemd reports' "$work/packaged/out.txt" || fail "the packaged node unit was not compared" packaged

fixture rendered
cat >"$work/rendered/render.yml" <<PLAY
---
- name: Render the role's server unit as a converge with a ledger volume would
  hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - name: Render
      ansible.builtin.template:
        src: $collection/roles/host/templates/billet-server.service.j2
        dest: $work/rendered/lib/billet-server.service
        mode: "0644"
      vars:
        billet_service_user: billet
        billet_service_group: billet
        billet_systemd_notify_ready: true
        billet_ledger_volume_id: vol-0123
        billet_ledger_mount_unit_name: var-lib-billet-server.mount
        billet_candidate_server_state_dir: /var/lib/billet/server
        billet_server_prepare_only: false
        billet_server_environment:
          BILLET_PG_DSN: postgres://example
        billet_server_environment_path: /etc/billet/server.env
PLAY
ansible-playbook -i localhost, "$work/rendered/render.yml" >"$work/rendered/render.txt" 2>&1 || { sed -n '1,40p' "$work/rendered/render.txt" >&2; exit 1; }
grep -c '^After=' "$work/rendered/lib/billet-server.service" | grep -q '^2$' || fail "the rendered unit does not carry the repeated After= this case exists for" rendered
allowed rendered "the role's server unit rendered with a ledger volume and an environment file passes"

fixture quote
sed -i.bak 's|^ExecStart=.*|ExecStart=/usr/bin/billet server --config "/etc/billet/billet.yaml"|' "$work/quote/lib/billet-server.service"
refused quote "a quoted argument refuses" "not one line this dry run can compare"

fixture specifier
sed -i.bak 's|^ExecStart=.*|ExecStart=/usr/bin/billet server --config %E/billet/billet.yaml|' "$work/specifier/lib/billet-server.service"
refused specifier "a specifier refuses" "not one line this dry run can compare"

fixture prefix
sed -i.bak 's|^ExecStart=.*|ExecStart=-/usr/bin/billet server --config /etc/billet/billet.yaml|' "$work/prefix/lib/billet-server.service"
refused prefix "an execution prefix refuses" "not one line this dry run can compare"

fixture repeated
printf 'User=billet\n' >>"$work/repeated/lib/billet-server.service"
refused repeated "a repeated User= line refuses" "User on disk [billet, billet]"

# 6. A loaded drop-in, and a drop-in on disk in each location the manager searches.
fixture loadeddropin
sed -i.bak "s|^DropInPaths=.*|DropInPaths=$work/loadeddropin/etc/billet-server.service.d/10-x.conf|" "$work/loadeddropin/fake/billet-server.service.props"
refused loadeddropin "a loaded drop-in refuses" "drop-ins are loaded"

for where in etc/billet-server.service.d run/billet-.service.d lib/service.d; do
    name=disk-${where//\//-}
    fixture "$name"
    mkdir -p "$work/$name/$where"
    echo '[Service]' >"$work/$name/$where/10-x.conf"
    refused "$name" "a drop-in on disk under $where refuses before any reload adopts it" "drop-ins are on disk"
done

# 7. Aliases, resolved by unit name as the manager resolves them.
fixture alias
ln -s billet-server.service "$work/alias/etc/runner.service"
refused alias "an alias on disk refuses" "runner.service is an alias of billet-server.service"

fixture aliasdropin
ln -s billet-server.service "$work/aliasdropin/etc/runner.service"
mkdir -p "$work/aliasdropin/etc/runner.service.d"
echo '[Service]' >"$work/aliasdropin/etc/runner.service.d/10-x.conf"
refused aliasdropin "a drop-in under an alias's .d is found through the alias" "runner.service is an alias"

fixture chain
ln -s "$work/chain/etc/billet-server.service" "$work/chain/lib/b.service"
ln -s b.service "$work/chain/etc/a.service"
cp "$work/chain/lib/billet-server.service" "$work/chain/etc/billet-server.service"
sed -i.bak "s|^FragmentPath=.*|FragmentPath=$work/chain/etc/billet-server.service|" "$work/chain/fake/billet-server.service.props"
refused chain "a cross-directory alias chain refuses naming the chain" "a.service -> b.service -> billet-server.service"

fixture dangling
ln -s missing.service "$work/dangling/etc/runner.service"
refused dangling "a dangling relative alias refuses as could-not-tell" "dangling"

fixture shadowed
printf '[Service]\nExecStart=/bin/true\n' >"$work/shadowed/etc/runner.service"
ln -s billet-server.service "$work/shadowed/lib/runner.service"
allowed shadowed "an alias shadowed by a higher-priority fragment is not loaded and passes"

fixture mask
ln -s /dev/null "$work/mask/etc/snapd.service"
allowed mask "an unrelated mask passes"

fixture external
mkdir -p "$work/external/opt"
cp "$work/external/lib/billet-server.service" "$work/external/opt/billet-server.service"
ln -s "$work/external/opt/billet-server.service" "$work/external/etc/vendor.service"
allowed external "a link out of every lookup path is a linked unit and passes"

fixture names
sed -i.bak 's/^Names=.*/Names=billet-server.service runner.service/' "$work/names/fake/billet-server.service.props"
refused names "a loaded alias in Names refuses" "loaded under other names"

# 8. The manager's lookup list is the authority, and an unknown path refuses.
fixture unknownpath
printf '{"type":"as","data":["%s","%s","%s","/srv/units"]}\n' "$work/unknownpath/etc" "$work/unknownpath/run" "$work/unknownpath/lib" >"$work/unknownpath/fake/unitpath.json"
refused unknownpath "a lookup path outside the allowlist refuses" "unit paths this dry run does not know (/srv/units)"

# 9. A busctl that cannot answer is could-not-tell.
fixture nobus
rm "$work/nobus/fake/billet-server.service.exec.json"
refused nobus "an unreadable loaded ExecStart refuses" "could not be read as exactly one record"

# 10. Both units pending: both compared, both reported.
fixture both
sed -i.bak 's/^NeedDaemonReload=no/NeedDaemonReload=yes/' "$work/both/fake/billet-node.service.props"
allowed both "both units pending and agreeing continue"
[ "$(grep -c 'the run continues' "$work/both/out.txt")" -ge 1 ] || fail "both units were not reported" both
grep -q 'billet-node.service: systemd reports' "$work/both/out.txt" || fail "the node unit was not compared" both

# 11. A unit the manager does not know has no fragment, and nothing to compare.
fixture notfound
sed -i.bak 's|^FragmentPath=.*|FragmentPath=|' "$work/notfound/fake/billet-server.service.props"
run notfound || fail "a not-found unit with the flag was refused" notfound
grep -q 'no fragment on disk' "$work/notfound/out.txt" || fail "the not-found unit was not reported as such" notfound
echo "ok   a unit with no fragment on disk has nothing to compare"

echo "dry-run policy gate: all cases pass"
