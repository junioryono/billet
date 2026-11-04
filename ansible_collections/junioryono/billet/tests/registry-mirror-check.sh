#!/bin/sh
# The site's pull-through mirrors, converged on this host's real systemd with the
# role's own tasks: three Distributions listening on a bridge address, verified with
# the authority a guest is handed, each answering for its upstream; a second
# converge that changes nothing; the node told about them; and each way of not
# running them. It needs passwordless sudo and the network: a manifest comes through
# Docker Hub's mirror, which is what a guest's pull asks first.
#
# A dummy interface stands in for the guest bridges, so nothing here can reach a
# real guest network, and everything is removed on the way out.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collections_root=${here%/ansible_collections/*}

if [ "$(uname -s)" != "Linux" ]; then
    echo "registry-mirror-check: skipped on $(uname -s): the mirrors are linux-only"
    exit 0
fi

work=$(mktemp -d)
bridge_a=billetmirror0
bridge_b=billetmirror1
address_a=192.0.2.71
address_b=192.0.2.72

fail() {
    echo "registry-mirror-check: $*" >&2
    exit 1
}

cleanup() {
    status=$?
    trap - EXIT INT TERM
    if [ "$status" -ne 0 ]; then
        for unit in docker-io ghcr-io quay-io; do
            sudo -n journalctl -u "billet-registry@$unit.service" -n 30 --no-pager 2>/dev/null || true
        done
    fi
    converge -e billet_registry_mirrors=false -e billet_registry_mirrors_purge=true >/dev/null 2>&1 || true
    sudo -n ip link delete "$bridge_a" 2>/dev/null || true
    sudo -n ip link delete "$bridge_b" 2>/dev/null || true
    rm -rf "$work"
    exit "$status"
}
trap cleanup EXIT INT TERM

converge() {
    ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
        ansible-playbook -i localhost, \
        -e "gate_output=$work" \
        -e "gate_bridge_a=$bridge_a" -e "gate_bridge_b=$bridge_b" \
        -e "gate_address_a=$address_a" -e "gate_address_b=$address_b" \
        "$@" "$here/registry-mirror-check.yml"
}

# json_get FILE EXPRESSION: one value of the configuration the node would be told.
node_value() {
    python3 -I -c 'import json,sys; node=json.load(open(sys.argv[1]))["node"]; print(json.dumps(node.get(sys.argv[2])))' \
        "$work/config.json" "$1"
}

status_of() {
    curl -sS -o /dev/null -w '%{http_code}' --max-time 30 --cacert /etc/billet-registry/ca.pem "$@"
}

for link in "$bridge_a:$address_a" "$bridge_b:$address_b"; do
    sudo -n ip link add "${link%%:*}" type dummy
    sudo -n ip address add "${link#*:}/32" dev "${link%%:*}"
    sudo -n ip link set "${link%%:*}" up
done

echo "== a site with the mirror"
converge >"$work/first.log" 2>&1 || { cat "$work/first.log" >&2; fail "the first converge failed"; }

for unit in docker-io ghcr-io quay-io; do
    [ "$(systemctl is-active "billet-registry@$unit.service")" = active ] ||
        fail "billet-registry@$unit.service is not active"
    [ "$(systemctl is-enabled "billet-registry@$unit.service")" = enabled ] ||
        fail "billet-registry@$unit.service is not enabled"
done

# THE AUTHORITY IS ALL A GUEST NEEDS, and without it the mirror is not trusted: that
# second half is what proves this was the authority's doing and not the system's.
waited=0
until [ "$(status_of "https://$address_a:7740/v2/" || true)" = 200 ]; do
    waited=$((waited + 1))
    [ "$waited" -lt 30 ] || fail "the Docker Hub mirror did not answer /v2/ within 30 seconds"
    sleep 1
done
for port in 7741 7742; do
    [ "$(status_of "https://$address_a:$port/v2/")" = 200 ] || fail "the mirror on $port did not answer /v2/"
done
if curl -sS -o /dev/null --max-time 10 "https://$address_a:7740/v2/" 2>/dev/null; then
    fail "the mirror was trusted without its authority, so nothing here proved the authority works"
fi

# EVERY BRIDGE'S ADDRESS IS ON THE CERTIFICATE: a guest on the second bridge may
# address the host by either, and a listener reached there verifies the same.
sans=$(openssl x509 -in /etc/billet-registry/leaf.pem -noout -ext subjectAltName)
for address in "$address_a" "$address_b"; do
    case "$sans" in *"IP Address:$address"*) ;; *) fail "the certificate does not name $address: $sans" ;; esac
done

# THE GUEST'S FIRST QUESTION, through to the upstream.
manifest=$(curl -sS --max-time 60 --cacert /etc/billet-registry/ca.pem \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://$address_a:7740/v2/library/alpine/manifests/latest") ||
    fail "a manifest did not come through the Docker Hub mirror"
case "$manifest" in
*'"manifests"'*) ;;
*) fail "the Docker Hub mirror answered something that is not a manifest list: $manifest" ;;
esac

# A PROXY REFUSES A PUSH, which is what keeps one job from putting something in front
# of another.
push=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 --cacert /etc/billet-registry/ca.pem \
    -X POST "https://$address_a:7740/v2/library/poison/blobs/uploads/")
case "$push" in
2*) fail "the mirror took an upload: $push" ;;
esac

# NO KEY A GUEST OR THE MIRROR'S ACCOUNT SHOULD NOT HAVE.
[ "$(sudo -n stat -c '%U:%G %a' /etc/billet-registry/authority/ca.key)" = "root:root 600" ] ||
    fail "the authority's key is not root's alone"
[ "$(sudo -n stat -c '%U:%G %a' /etc/billet-registry/leaf/leaf.key)" = "root:billet-registry 640" ] ||
    fail "the certificate's key is not readable by the mirrors' account alone"
if sudo -n -u billet-registry cat /etc/billet-registry/authority/ca.key >/dev/null 2>&1; then
    fail "the mirrors' account can read the authority's key"
fi

expected_mirrors="{\"docker.io\": \"https://$address_a:7740\", \"ghcr.io\": \"https://$address_a:7741\", \"quay.io\": \"https://$address_a:7742\"}"
[ "$(node_value registry_mirrors)" = "$expected_mirrors" ] ||
    fail "the node is told $(node_value registry_mirrors), want $expected_mirrors"
[ "$(node_value registry_mirrors_ca)" = '"/etc/billet-registry/ca.pem"' ] ||
    fail "the node is told the authority is at $(node_value registry_mirrors_ca)"
grep -q 'tcp dport { 53, 7718, 7740, 7741, 7742 } accept' "$work/network.nft" ||
    fail "a guest bridge's firewall does not admit the mirrors: $(grep 'tcp dport' "$work/network.nft")"

echo "== a second converge changes nothing"
fingerprint=$(sudo -n sha256sum /etc/billet-registry/ca.pem /etc/billet-registry/leaf.pem)
converge >"$work/second.log" 2>&1 || { cat "$work/second.log" >&2; fail "the second converge failed"; }
grep -Eq 'localhost +: ok=[0-9]+ +changed=[0-9]+' "$work/second.log" || fail "no recap in the second converge"
# The two tasks this test adds itself write the same bytes, so a change anywhere is
# the role's.
grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' "$work/second.log" || {
    cat "$work/second.log" >&2
    fail "the second converge changed something"
}
[ "$(sudo -n sha256sum /etc/billet-registry/ca.pem /etc/billet-registry/leaf.pem)" = "$fingerprint" ] ||
    fail "the second converge replaced the authority or the certificate"

echo "== a certificate that names a removed bridge is reissued, under the same authority"
authority=$(sudo -n sha256sum /etc/billet-registry/ca.pem)
address_b=192.0.2.73
sudo -n ip address add "$address_b/32" dev "$bridge_b"
converge >"$work/reissue.log" 2>&1 || { cat "$work/reissue.log" >&2; fail "the reissuing converge failed"; }
sans=$(openssl x509 -in /etc/billet-registry/leaf.pem -noout -ext subjectAltName)
case "$sans" in *"IP Address:$address_b"*) ;; *) fail "the certificate was not reissued: $sans" ;; esac
case "$sans" in *"IP Address:192.0.2.72"*) fail "the certificate kept a bridge it should not: $sans" ;; esac
[ "$(sudo -n sha256sum /etc/billet-registry/ca.pem)" = "$authority" ] || fail "reissuing replaced the authority"
waited=0
until [ "$(status_of "https://$address_a:7740/v2/" || true)" = 200 ]; do
    waited=$((waited + 1))
    [ "$waited" -lt 30 ] || fail "the mirror did not restart onto its new certificate"
    sleep 1
done

echo "== an operator's own mirrors win, and this role runs none"
own='{"registry_mirrors":{"docker.io":"https://a.home.example","ghcr.io":"https://b.home.example","quay.io":"https://c.home.example"}}'
converge -e "{\"gate_own_mirrors\":$own}" >"$work/own.log" 2>&1 || { cat "$work/own.log" >&2; fail "the converge with own mirrors failed"; }
[ "$(node_value registry_mirrors)" = '{"docker.io": "https://a.home.example", "ghcr.io": "https://b.home.example", "quay.io": "https://c.home.example"}' ] ||
    fail "the role replaced an operator's mirrors: $(node_value registry_mirrors)"
[ "$(node_value registry_mirrors_ca)" = null ] || fail "the role named an authority beside an operator's mirrors"
[ ! -e /etc/systemd/system/billet-registry@.service ] || fail "the role kept its unit beside an operator's mirrors"

echo "== a provider whose guests consume no mirror gets none"
converge -e gate_provider=docker >"$work/docker.log" 2>&1 || { cat "$work/docker.log" >&2; fail "the docker converge failed"; }
[ "$(node_value registry_mirrors)" = null ] || fail "a docker node was told of mirrors"
[ ! -e /etc/billet-registry ] || fail "a docker node has the mirrors' files"

echo "== turned on again, then off: the configuration and the firewall follow, and what they pulled is kept"
converge >"$work/again.log" 2>&1 || { cat "$work/again.log" >&2; fail "the converge turning them on again failed"; }
storage_before=$(sudo -n du -s /var/lib/billet-registry | cut -f1)
converge -e billet_registry_mirrors=false >"$work/off.log" 2>&1 || { cat "$work/off.log" >&2; fail "the converge turning them off failed"; }
[ "$(node_value registry_mirrors)" = null ] || fail "the node is still told of mirrors that were turned off"
[ "$(node_value registry_mirrors_ca)" = null ] || fail "the node is still told of an authority that was turned off"
grep -q 'tcp dport { 53, 7718 } accept' "$work/network.nft" ||
    fail "a guest bridge's firewall still admits mirrors that were turned off: $(grep 'tcp dport' "$work/network.nft")"
for unit in docker-io ghcr-io quay-io; do
    if systemctl is-active --quiet "billet-registry@$unit.service"; then
        fail "billet-registry@$unit.service is still running"
    fi
done
[ ! -e /etc/systemd/system/billet-registry@.service ] || fail "the unit outlived the mirrors"
[ ! -e /etc/billet-registry ] || fail "the authority's key outlived the mirrors"
[ -d /var/lib/billet-registry/docker-io ] || fail "turning the mirrors off lost what they pulled"
[ "$(sudo -n du -s /var/lib/billet-registry | cut -f1)" -ge "${storage_before:-1}" ] || fail "what they pulled shrank"

echo "== a retained converge takes up nothing new, and keeps what an earlier one installed"
ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ansible-playbook -i localhost, -e "gate_output=$work" \
    -e "gate_bridge_a=$bridge_a" -e "gate_bridge_b=$bridge_b" \
    -e "gate_address_a=$address_a" -e "gate_address_b=$address_b" -e gate_retained=true \
    "$here/registry-mirror-config-check.yml" >"$work/retained-none.log" 2>&1 ||
    { cat "$work/retained-none.log" >&2; fail "the retained decision failed with no authority"; }
[ "$(node_value registry_mirrors)" = null ] || fail "a retained converge took up mirrors nothing installed"

sudo -n install -d -m 0755 /etc/billet-registry
sudo -n sh -c ': >/etc/billet-registry/ca.pem'
ANSIBLE_COLLECTIONS_PATH="$collections_root:$HOME/.ansible/collections:/usr/share/ansible/collections" \
    ansible-playbook -i localhost, -e "gate_output=$work" \
    -e "gate_bridge_a=$bridge_a" -e "gate_bridge_b=$bridge_b" \
    -e "gate_address_a=$address_a" -e "gate_address_b=$address_b" -e gate_retained=true \
    "$here/registry-mirror-config-check.yml" >"$work/retained-kept.log" 2>&1 ||
    { cat "$work/retained-kept.log" >&2; fail "the retained decision failed with an authority"; }
[ "$(node_value registry_mirrors_ca)" = '"/etc/billet-registry/ca.pem"' ] ||
    fail "a retained converge dropped mirrors an earlier converge installed"
sudo -n rm -rf /etc/billet-registry

echo "registry-mirror-check: passed"
