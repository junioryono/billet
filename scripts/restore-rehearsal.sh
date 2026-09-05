#!/bin/sh
# The restore rehearsal, INSIDE a fresh Linux machine.
#
# WHAT THIS PROVES THAT THE GO TEST CANNOT. internal/e2e's rehearsal assembles a
# control plane in process, so it says nothing about the three things that are
# only true on a packaged Linux host: the `billet` service account, the mode and
# OWNERSHIP of a state directory systemd's StateDirectory= created, and the real
# binary's `billet local backup` / `billet local restore` — config loading, the
# hardened App-key read, the lifecycle lock, the fence, all of it. None of the
# restore work was ever exercised there until this rehearsal existed.
#
# WHAT IT CANNOT PROVE, said here rather than left to be assumed: nothing starts
# a control plane, because `billet server` cannot start without reaching GitHub
# (server.Run returns an error when EnsureScaleSet fails). `--upgrade-probe` is
# the furthest a machine with no App gets — it opens the ledger, migrates it,
# builds the allocator from the restored rows and reads the App key, then waits —
# and that is exactly the part a restore is responsible for. The serving half is
# the Go test's; the systemd units and `billet local up` are still only exercised
# by hand.
#
# It runs as root, because that is what an operator restoring onto a packaged
# host is: the App key lands in root-owned /etc/billet.
set -eu

fail() {
    echo >&2
    echo "restore rehearsal FAILED: $*" >&2
    exit 1
}

step() {
    echo
    echo "=== $*"
}

# edited proves a scripted substitution actually applied. An edit that did not
# apply looks exactly like an edit that did, and this whole script is about not
# believing things that were never checked.
edited() {
    grep -qF -- "$2" "$1" || fail "editing $1 did not produce '$2'"
}

STATE_A=/var/lib/billet/server
STATE_B=/var/lib/billet/restored
CONFIG_A=/etc/billet/billet.yaml
CONFIG_B=/etc/billet/billet-restored.yaml
ARCHIVE=/var/backups/billet/a
BUNDLE=/var/lib/billet/node-a-tls

step "install the package onto a machine that has never run billet"
export DEBIAN_FRONTEND=noninteractive
# APT FAILS FAST OR NOT AT ALL. On 2026-09-05 Ubuntu's archive was mid-sync and a
# fresh container's apt-get update sat silent for eighteen minutes inside a
# thirty-minute job, then was killed with nothing to read; the run that did finish
# reported "File has unexpected size ... Mirror sync in progress?" after ten. A
# gate that can only fail by being killed is not a gate: every fetch is bounded,
# the transient half is retried, and the whole call is capped so the failure
# arrives with its reason while the job still has time to print it (-v names the
# signal and the command it went to, -k kills a dpkg that shrugs off TERM). And an index
# apt-get update could not fetch is only a WARNING to it, exit 0, measured against
# a black-holed proxy; Error-Mode=any makes that the failure it is, here rather
# than two commands later as "openssl has no installation candidate".
APT="timeout -v -k 10 300 apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 -o Acquire::http::Timeout=30"
${APT} update >/dev/null
${APT} install --yes /tmp/billet.deb openssl >/dev/null

id billet >/dev/null 2>&1 || fail "the package did not create the service account"
test -x /usr/bin/billet || fail "the package did not install the binary"
test -f "${CONFIG_A}" || fail "the package did not install a config"

step "make the shipped config say something true"
# The template refuses to start until somebody sizes the machine and names an
# App, which is deliberate: a package must not guess a capacity ceiling.
sed -i \
    -e 's/^  max_vcpu: 0$/  max_vcpu: 8/' \
    -e 's/^  max_memory: 0$/  max_memory: 16GiB/' \
    -e 's/^  org: your-org$/  org: acme/' \
    -e 's/^  app_id: 0$/  app_id: 12345/' \
    -e 's/^  installation_id: 0$/  installation_id: 67890/' \
    "${CONFIG_A}"
edited "${CONFIG_A}" "max_vcpu: 8"
edited "${CONFIG_A}" "max_memory: 16GiB"
edited "${CONFIG_A}" "org: acme"
edited "${CONFIG_A}" "app_id: 12345"
edited "${CONFIG_A}" "installation_id: 67890"

# A SECOND TARGET, repository-scoped, with a key of its own. The archive has to
# carry every target's key or refuse, and a restore has to install every one;
# a rehearsal with a single target would pass with the second key silently
# dropped. With two targets every tier must say which it belongs to.
sed -i \
    -e 's/^  - label: billet-2vcpu-ubuntu-2404$/  - label: billet-2vcpu-ubuntu-2404\n    target: default/' \
    "${CONFIG_A}"
edited "${CONFIG_A}" "target: default"
cat >>"${CONFIG_A}" <<'EOF'

targets:
  - name: personal
    repository: someone/widgets
    app_id: 12346
    installation_id: 67891
    private_key_path: /etc/billet/app-private-key-personal.pem
EOF
edited "${CONFIG_A}" "private_key_path: /etc/billet/app-private-key-personal.pem"

# A throwaway App key in the shape GitHub issues (PKCS#1), with the mode billet
# insists on: the hardened read refuses anything group- or world-readable.
openssl genrsa -traditional -out /etc/billet/app-private-key.pem 2048 2>/dev/null
chown billet:billet /etc/billet/app-private-key.pem
chmod 600 /etc/billet/app-private-key.pem
openssl genrsa -traditional -out /etc/billet/app-private-key-personal.pem 2048 2>/dev/null
chown billet:billet /etc/billet/app-private-key-personal.pem
chmod 600 /etc/billet/app-private-key-personal.pem

# THE STATE DIRECTORY AS systemd LEAVES IT: owned by the service account, 0700,
# empty. That is the case that matters, because StateDirectory= repairs
# ownership recursively only when the TOP directory's owner is wrong — measured
# on systemd 255 — so anything root writes underneath a correctly-owned
# directory stays root's forever.
install -d -o billet -g billet -m 0700 "${STATE_A}"
install -d -o billet -g billet -m 0700 /var/backups/billet
install -d -o billet -g billet -m 0700 /var/lib/billet/bundles

step "commission the deployment, the way a first install does"
# `billet ca issue` on a fresh install mints the identity and the authority and
# writes the admission trail — there is no server yet, and there does not need
# to be one.
runuser -u billet -- billet ca issue node-a --config "${CONFIG_A}" --out "${BUNDLE}"

test -f "${STATE_A}/deployment-id" || fail "no deployment identity was minted"
test -f "${STATE_A}/ca/ca.key" || fail "no authority was minted"
test -f "${BUNDLE}/node.crt" || fail "no node bundle was written"

step "back it up, as the service account"
runuser -u billet -- billet local backup --config "${CONFIG_A}" --out "${ARCHIVE}"

test -f "${ARCHIVE}/manifest.json" || fail "the backup wrote no manifest"

# 0700 over 0600, because two of those files are private keys.
mode=$(stat -c '%a' "${ARCHIVE}")
test "${mode}" = "700" || fail "the archive directory is mode ${mode}, want 700"
mode=$(stat -c '%a' "${ARCHIVE}/github/app-private-key.pem")
test "${mode}" = "600" || fail "the archived App key is mode ${mode}, want 600"
mode=$(stat -c '%a' "${ARCHIVE}/github/personal/app-private-key.pem")
test "${mode}" = "600" || fail "the archived second target's App key is mode ${mode}, want 600"

step "restore it where nothing has seen it — as root, which is what an operator is"
sed -e "s#state_dir: ${STATE_A}#state_dir: ${STATE_B}#" \
    -e 's#private_key_path: /etc/billet/app-private-key.pem#private_key_path: /etc/billet/restored-app-private-key.pem#' \
    -e 's#private_key_path: /etc/billet/app-private-key-personal.pem#private_key_path: /etc/billet/restored-app-private-key-personal.pem#' \
    "${CONFIG_A}" > "${CONFIG_B}"
edited "${CONFIG_B}" "state_dir: ${STATE_B}"
edited "${CONFIG_B}" "private_key_path: /etc/billet/restored-app-private-key.pem"
edited "${CONFIG_B}" "private_key_path: /etc/billet/restored-app-private-key-personal.pem"

# Prepared exactly as the other host's would be by the units it already has
# installed: created, owned by the service account, and empty.
install -d -o billet -g billet -m 0700 "${STATE_B}"

billet local restore --config "${CONFIG_B}" --from "${ARCHIVE}" --dry-run
billet local restore --config "${CONFIG_B}" --from "${ARCHIVE}" --old-controller-fenced

step "what root restored, and who owns it"
ls -la "${STATE_B}" "${STATE_B}/ca"

step "the deployment that came back is the same deployment"
cmp "${STATE_A}/deployment-id" "${STATE_B}/deployment-id" ||
    fail "the restored deployment identity differs from the original"
cmp "${STATE_A}/ca/ca.crt" "${STATE_B}/ca/ca.crt" ||
    fail "the restored authority certificate differs from the original"
cmp "${STATE_A}/ca/ca.key" "${STATE_B}/ca/ca.key" ||
    fail "the restored authority key differs from the original"
cmp /etc/billet/app-private-key.pem /etc/billet/restored-app-private-key.pem ||
    fail "the restored App key differs from the original; GitHub issues it once"
cmp /etc/billet/app-private-key-personal.pem /etc/billet/restored-app-private-key-personal.pem ||
    fail "the restored second target's App key differs from the original; GitHub issues it once"

# BOTH KEYS HANDED TO THE SERVICE ACCOUNT, because a root-run restore that
# repaired the first and forgot the second would start a control plane that can
# serve one target and refuses the other.
for key in /etc/billet/restored-app-private-key.pem /etc/billet/restored-app-private-key-personal.pem; do
    owner_mode=$(stat -c '%U:%a' "${key}")
    test "${owner_mode}" = "billet:600" ||
        fail "${key} is ${owner_mode} after the restore, want billet:600"
done

# ASKED OF A DIFFERENT TOOL, so this is not billet agreeing with itself: the
# certificate the ORIGINAL deployment issued still verifies against the restored
# authority, which is what a node's handshake will do.
openssl verify -CAfile "${STATE_B}/ca/ca.crt" "${BUNDLE}/node.crt" >/dev/null ||
    fail "a certificate the original deployment issued does not verify against the restored authority"

step "the service account can use what root restored"
# THE PROOF THAT MATTERS ON LINUX. A root-run restore writes root-owned files
# into a directory the service account owns, and systemd will not repair them.
# If this fails, a restored control plane cannot start — which is the whole
# operation being useless on the platform it is documented for.
probe=/tmp/upgrade-probe.log
runuser -u billet -- billet server --upgrade-probe --config "${CONFIG_B}" >"${probe}" 2>&1 &
probe_pid=$!

waited=0
while [ "${waited}" -lt 60 ]; do
    if grep -q 'upgrade probe ready' "${probe}"; then
        break
    fi
    if ! kill -0 "${probe_pid}" 2>/dev/null; then
        break
    fi
    waited=$((waited + 1))
    sleep 1
done

kill "${probe_pid}" 2>/dev/null || true
wait "${probe_pid}" 2>/dev/null || true

if ! grep -q 'upgrade probe ready' "${probe}"; then
    echo "--- what the probe said:" >&2
    cat "${probe}" >&2
    fail "the service account could not open the deployment root restored"
fi

step "and it can still issue a certificate from the restored authority"
runuser -u billet -- billet ca issue node-b --config "${CONFIG_B}" --out /var/lib/billet/bundles/node-b
openssl verify -CAfile "${STATE_A}/ca/ca.crt" /var/lib/billet/bundles/node-b/node.crt >/dev/null ||
    fail "the restored authority issued a certificate the original authority does not vouch for"

step "a restore over the commissioned original is REFUSED"
# The ledger there has rows in it, so replacing it would leave a control plane
# with no lease for compute created since the backup — which node recovery
# destroys as orphans. `billet local recover` is the operation for that, and it
# is not this one.
if billet local restore --config "${CONFIG_A}" --from "${ARCHIVE}" --old-controller-fenced \
    >/tmp/over-original.log 2>&1; then
    cat /tmp/over-original.log >&2
    fail "a restore over a commissioned deployment was allowed"
fi
grep -q 'commissioned deployment' /tmp/over-original.log ||
    fail "the refusal did not say the target is a commissioned deployment"
grep -q 'billet local recover' /tmp/over-original.log ||
    fail "the refusal did not name the operation that IS right there"

step "and \`billet local recover\` is the operation that IS right there"
# THE ORDINARY DISASTER SHAPE: the ledger on disk is this deployment's own and
# not one the operator wants to keep. Recover seals the deployment, proves it is
# holding nothing, replaces the ledger and moves the old one aside — and leaves
# admission CLOSED, because the nodes may still hold compute the restored ledger
# has never heard of.
billet local recover --config "${CONFIG_A}" --from "${ARCHIVE}" --old-controller-fenced

superseded=$(find "${STATE_A}" -maxdepth 1 -name 'billet.db.superseded-*' -print -quit)
test -n "${superseded}" ||
    fail "the recovery did not preserve the ledger it replaced"
test -s "${superseded}" ||
    fail "the ledger it preserved is empty"

step "the recovered deployment is SEALED, and the service account can open it"
# THE SEAL IS THE POINT. The restored ledger carries the admission it had when
# the backup was taken — open — so a recovery that did not seal again would hand
# back a control plane that takes new work while its nodes hold compute it has
# never heard of.
runuser -u billet -- billet status --config "${CONFIG_A}" >/tmp/recovered-status.log 2>&1 ||
    fail "the service account could not read the recovered deployment"
grep -q 'not taking new work' /tmp/recovered-status.log || {
    cat /tmp/recovered-status.log >&2
    fail "the recovered deployment is not sealed"
}
# AND IT IS AN OPERATOR'S SEAL, which is what makes it survive `billet local up`
# — a shutdown's would be cleared by the next start, reopening a deployment whose
# nodes still hold compute the restored ledger has never heard of.
grep -q 'survives a restart' /tmp/recovered-status.log || {
    cat /tmp/recovered-status.log >&2
    fail "the recovered deployment's seal would be cleared by the next \`billet local up\`"
}

echo
echo "restore rehearsal: PASSED"
