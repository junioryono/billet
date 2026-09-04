#!/bin/sh
# The inventory and playbook `billet init hybrid` writes are ones Ansible can
# read, and the one tier catalogue reaches both hosts through the anchor.
#
# WHY THIS EXISTS. The Go tests parse the inventory with a YAML library, and
# that library and Ansible agree on anchors -- but not on everything: a
# playbook that names a role the collection does not ship, a host variable the
# role refuses, or a group the playbook never targets are Ansible's to notice.
# The syntax check resolves the role against THIS checkout's collection, and
# `ansible-inventory --list` is what proves each host carries the catalogue
# after Ansible's own expansion, which is the seam a YAML anchor could silently
# fail to cross.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}
repo_root=$collections_root

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

billet=${BILLET_HYBRID_CHECK_BINARY:-}
if [ -z "$billet" ]; then
    echo "Building billet to generate the shape..."
    (cd "$repo_root" && go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi

python=""
if command -v ansible >/dev/null 2>&1; then
    python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
fi
if [ -z "$python" ] || [ ! -x "$python" ]; then
    python=$(command -v python3) || { echo "hybrid-emission-check: no python3" >&2; exit 1; }
fi

out=$work/hybrid
outputs=$work/outputs.json
cat >"$outputs" <<'EOF'
{
  "control_plane_private_ip": {"sensitive": false, "type": "string", "value": "10.60.0.10"},
  "ledger_volume_id": {"sensitive": false, "type": "string", "value": "vol-0abc"},
  "subnet_id": {"sensitive": false, "type": "string", "value": "subnet-0abc"},
  "runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-trusted"},
  "untrusted_runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-untrusted"},
  "ami_payload_bucket": {"sensitive": false, "type": "string", "value": "hybrid-check-ami-payloads-1"}
}
EOF

# THE COMMISSION RENDER, because it is the most complete one: both hosts carry
# a node, the controller carries the server, the App and the backup block.
"$billet" init hybrid --out "$out" \
    --name hybrid-check --region us-west-2 --org acme \
    --control-plane-private-ip 10.60.0.10 \
    --local-vcpu 32 --local-memory 128GiB \
    --max-vcpu 16 --max-memory 32GiB \
    --instance-type 'c7i.xlarge=4,8GiB,0.17' \
    --instance-type 'c7i.2xlarge=8,16GiB,0.34' \
    --terraform-output "$outputs" --commission --ami ami-0123456789abcdef0 \
    >"$work/gen.log" 2>&1 || {
    echo "hybrid-emission-check: the generation failed" >&2; cat "$work/gen.log" >&2; exit 1; }

export ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections"
export ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1

ansible-playbook --syntax-check -i "$out/inventory.yml" "$out/site.yml" >"$work/syntax.log" 2>&1 || {
    echo "hybrid-emission-check: the generated playbook fails Ansible's syntax check" >&2
    cat "$work/syntax.log" >&2; exit 1; }
echo "ok   the generated playbook and inventory pass the syntax check"

ansible-inventory -i "$out/inventory.yml" --list >"$work/inventory.json" 2>"$work/inventory.err" || {
    echo "hybrid-emission-check: ansible-inventory could not read the generated inventory" >&2
    cat "$work/inventory.err" >&2; exit 1; }

# THE CATALOGUE CROSSED THE ANCHOR, as Ansible expands it, onto both hosts and
# into every group the playbook targets. Read from the JSON Ansible produced,
# not from the file billet wrote.
"$python" - "$work/inventory.json" <<'EOF'
import json, sys

doc = json.load(open(sys.argv[1]))
hostvars = doc["_meta"]["hostvars"]
groups = {g: doc[g].get("hosts", []) for g in ("control_plane", "linux")}

for group, hosts in groups.items():
    if len(hosts) != 1:
        sys.exit(f"hybrid-emission-check: group {group} holds {hosts}, want exactly one host")

controller = hostvars[groups["control_plane"][0]]
local = hostvars[groups["linux"][0]]

for name, vars in (("the controller", controller), ("the local host", local)):
    tiers = vars.get("billet_config", {}).get("tiers")
    if not tiers:
        sys.exit(f"hybrid-emission-check: {name} carries no billet_config.tiers after Ansible's expansion; the anchor did not cross")

if [t["label"] for t in controller["billet_config"]["tiers"]] != [t["label"] for t in local["billet_config"]["tiers"]]:
    sys.exit("hybrid-emission-check: the two hosts carry different catalogues")

for t in controller["billet_config"]["tiers"]:
    if t.get("providers") != ["firecracker", "ec2"]:
        sys.exit(f"hybrid-emission-check: tier {t['label']} providers {t.get('providers')}, want [firecracker, ec2]")
    if "ec2" not in t.get("launch", {}) or "firecracker" not in t.get("launch", {}):
        sys.exit(f"hybrid-emission-check: tier {t['label']} lacks a launch entry for one backend")

if "server" in local["billet_config"] or "github" in local["billet_config"]:
    sys.exit("hybrid-emission-check: the local host carries a server or github block; both absences are load-bearing")
if controller.get("billet_server_prepare_only") is not False or controller.get("billet_enable_node") is not True:
    sys.exit("hybrid-emission-check: the commission render must lift the hold and enable the node")
if controller["billet_config"]["node"]["ec2"]["subnet_id"] != "subnet-0abc":
    sys.exit("hybrid-emission-check: the ec2 placement did not come from the terraform outputs")

print(f"ok   both hosts carry the same {len(controller['billet_config']['tiers'])}-tier catalogue through the anchor")
EOF

echo "hybrid-emission-check: the generated shape is one Ansible reads"
