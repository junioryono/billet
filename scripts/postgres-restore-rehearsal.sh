#!/bin/sh
# The restore rehearsal for a deployment whose LEDGER IS SOMEWHERE ELSE, inside a
# fresh Linux machine with a real PostgreSQL beside it.
#
# WHAT THIS PROVES THAT scripts/restore-rehearsal.sh CANNOT, and it is not the
# same operation with a different database underneath. On this profile billet
# archives HALF a deployment on purpose: the ledger is pg_dump's or the
# provider's to copy, so the archive holds the deployment identity, the node-wire
# authority and the GitHub App private key and records the ledger as external
# external. Everything about restoring that half is a path the SQLite rehearsal
# never takes — a different archive schema, an entry set one short, and two
# refusals that exist only here.
#
# AND IT IS THE PROFILE WHERE THE ARCHIVE MATTERS MOST. The control-plane-postgres
# module has no ledger volume by design — a volume pins the instance to one
# availability zone, which is exactly what makes the SQLite controller
# un-replaceable — its root volume is delete_on_termination, and GitHub issues
# the App key exactly once. On that host this archive is the only copy of the
# deployment's identity there is.
#
# WHAT IT DELIBERATELY DOES NOT RE-PROVE: everything the SQLite rehearsal already
# does about the package itself — that it creates the service account, installs a
# binary and a config template, and that a root-run restore leaves files a
# service account can read. Those are the same code on both profiles. What is
# repeated here is only the ownership proof, because it is reached through a
# DIFFERENT instrument: `billet server --upgrade-probe` is refused outright on an
# external ledger (the transactional host upgrade snapshots a state directory an
# external ledger does not have), so this asks `billet ca issue` and `billet
# status` instead — between them they read the identity, open the CA private key
# and write to the ledger, all as the service account.
#
# It runs as root, because that is what an operator restoring onto a packaged
# host is: the App key lands in root-owned /etc/billet.
set -eu

fail() {
    echo >&2
    echo "postgres restore rehearsal FAILED: $*" >&2
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

# absent is the same rule facing the other way: a key that should be GONE.
absent() {
    grep -qF -- "$2" "$1" && fail "editing $1 left '$2' behind"
    return 0
}

# untouched proves a REFUSED operation published nothing.
#
# EVERY REFUSAL AND THE DRY RUN SHARE ONE DESTINATION, so without this a
# regression that publishes and THEN refuses seeds the success that follows: the
# real restore would find byte-identical files, report AlreadyPresent, and pass.
# A dry run that mistakenly performed the whole restore is the same shape and is
# the one this was written for.
#
# THE WHOLE DIRECTORY AND THE KEY, not just deployment-id. The first version
# checked one file, so a partial publication of the authority or the App key —
# which is the credential GitHub issues exactly once — went unseen.
untouched() {
    left=$(find "${IDENTITY_B}" -mindepth 1 2>/dev/null | head -n 5)
    test -z "${left}" ||
        fail "$1 published into ${IDENTITY_B}: ${left}"

    test -e /etc/billet/restored-app-private-key.pem &&
        fail "$1 published the GitHub App key"

    return 0
}

IDENTITY_A=/var/lib/billet/server
IDENTITY_B=/var/lib/billet/restored
CONFIG_A=/etc/billet/billet.yaml
CONFIG_B=/etc/billet/billet-restored.yaml
CONFIG_SQLITE=/etc/billet/billet-sqlite.yaml
ARCHIVE=/var/backups/billet/a
BUNDLE=/var/lib/billet/node-a-tls

# THE VARIABLE'S NAME IS WHAT THE CONFIG CARRIES, never the DSN itself: a DSN
# holds a password, and a secret written into YAML ends up in a backup, a paste
# buffer and eventually a support thread.
DSN_ENV=BILLET_STATE_DSN
export BILLET_STATE_DSN='postgres://billet:billet@127.0.0.1:5432/billet?sslmode=disable'

step "install the package and a PostgreSQL onto a machine that has never run billet"
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
${APT} install --yes /tmp/billet.deb openssl postgresql jq >/dev/null

id billet >/dev/null 2>&1 || fail "the package did not create the service account"
test -x /usr/bin/billet || fail "the package did not install the binary"

step "start the ledger's database"
# THE CLUSTER THE DISTRIBUTION SHIPS, started the way the distribution starts it.
# pg_ctlcluster drops to the postgres user itself; running initdb by hand here
# would be a second implementation of something the package already does.
# THE VERSION, THE NAME AND THE PORT ARE ALL READ, none of them assumed.
# MEASURED in ubuntu:24.04: `pg_lsclusters -h` prints
# "16 main 5432 down postgres /var/lib/postgresql/16/main ...". Hard-coding any
# of the three would make this gate fail on an Ubuntu that moved one — and the
# port matters twice over, because the DSN above names 5432 and a cluster
# listening elsewhere would fail much later as a connection error that says
# nothing about why.
cluster_line=$(pg_lsclusters -h | head -n 1)
test -n "${cluster_line}" || fail "the postgresql package installed no cluster"

cluster_ver=$(printf '%s\n' "${cluster_line}" | awk '{print $1}')
cluster_name=$(printf '%s\n' "${cluster_line}" | awk '{print $2}')
cluster_port=$(printf '%s\n' "${cluster_line}" | awk '{print $3}')

test "${cluster_port}" = "5432" ||
    fail "the cluster listens on ${cluster_port} and this rehearsal's DSN names 5432"

# ONLY IF IT IS DOWN. `pg_ctlcluster start` FAILS on a cluster that is already
# running, and whether the package leaves it up is the distribution's business
# rather than something this gate should depend on. A false red is still a red.
cluster_status=$(printf '%s\n' "${cluster_line}" | awk '{print $4}')
if [ "${cluster_status}" != "online" ]; then
    pg_ctlcluster "${cluster_ver}" "${cluster_name}" start
fi

waited=0
while [ "${waited}" -lt 30 ]; do
    if runuser -u postgres -- pg_isready >/dev/null 2>&1; then
        break
    fi
    waited=$((waited + 1))
    sleep 1
done
runuser -u postgres -- pg_isready >/dev/null 2>&1 || fail "postgresql never became ready"

runuser -u postgres -- psql -q -c "CREATE ROLE billet LOGIN PASSWORD 'billet'"
runuser -u postgres -- psql -q -c "CREATE DATABASE billet OWNER billet"

step "make the shipped config name an external ledger"
# THE PACKAGED TEMPLATE, EDITED — not a config written from scratch here. What is
# under test includes the shape an operator actually starts from, and a
# hand-written one would drift from it silently.
#
# state_dir BECOMES identity_dir, because the two are mutually exclusive at load:
# a config that wrote both is refused, and one that kept state_dir beside a
# `state:` block would be this profile in name only.
sed -i \
    -e 's/^  max_vcpu: 0$/  max_vcpu: 8/' \
    -e 's/^  max_memory: 0$/  max_memory: 16GiB/' \
    -e 's/^  org: your-org$/  org: acme/' \
    -e 's/^  app_id: 0$/  app_id: 12345/' \
    -e 's/^  installation_id: 0$/  installation_id: 67890/' \
    -e "s#^  state_dir: ${IDENTITY_A}\$#  identity_dir: ${IDENTITY_A}\\n  state:\\n    backend: postgres\\n    postgres:\\n      dsn_env: ${DSN_ENV}#" \
    "${CONFIG_A}"
edited "${CONFIG_A}" "max_vcpu: 8"
edited "${CONFIG_A}" "identity_dir: ${IDENTITY_A}"
edited "${CONFIG_A}" "backend: postgres"
edited "${CONFIG_A}" "dsn_env: ${DSN_ENV}"
absent "${CONFIG_A}" "  state_dir: ${IDENTITY_A}"

# A throwaway App key in the shape GitHub issues (PKCS#1), with the mode billet
# insists on: the hardened read refuses anything group- or world-readable.
openssl genrsa -traditional -out /etc/billet/app-private-key.pem 2048 2>/dev/null
chown billet:billet /etc/billet/app-private-key.pem
chmod 600 /etc/billet/app-private-key.pem

# THE IDENTITY DIRECTORY AS systemd LEAVES IT: owned by the service account,
# 0700, empty.
install -d -o billet -g billet -m 0700 "${IDENTITY_A}"
install -d -o billet -g billet -m 0700 /var/backups/billet
install -d -o billet -g billet -m 0700 /var/lib/billet/bundles

step "commission the deployment, the way a first install does"
# `billet ca issue` mints the identity and the authority on local disk AND writes
# the admission trail into the ledger — which on this profile is the database, so
# this is also what creates and migrates the schema there.
#
# runuser RESETS THE ENVIRONMENT, so the DSN has to be handed across explicitly.
# `env` can do that because billet is a FILE; it could not run a shell builtin.
runuser -u billet -- env "${DSN_ENV}=${BILLET_STATE_DSN}" \
    billet ca issue node-a --config "${CONFIG_A}" --out "${BUNDLE}"

test -f "${IDENTITY_A}/deployment-id" || fail "no deployment identity was minted"
test -f "${IDENTITY_A}/ca/ca.key" || fail "no authority was minted"
test -f "${BUNDLE}/node.crt" || fail "no node bundle was written"

# AND THE LEDGER IS REALLY IN POSTGRESQL. Without this the whole rehearsal could
# be running against a SQLite file nobody noticed, proving the ordinary path
# twice.
test -f "${IDENTITY_A}/billet.db" &&
    fail "a SQLite ledger was created beside an external one; this is not the profile under test"
runuser -u postgres -- psql -d billet -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_name = 'schema_migrations'" |
    grep -qx 1 || fail "the ledger's schema is not in PostgreSQL"

step "back it up, as the service account"
runuser -u billet -- env "${DSN_ENV}=${BILLET_STATE_DSN}" \
    billet local backup --config "${CONFIG_A}" --out "${ARCHIVE}"

test -f "${ARCHIVE}/manifest.json" || fail "the backup wrote no manifest"

# THE ARCHIVE IS THE IDENTITY-ONLY ONE, asserted STRUCTURALLY rather than by
# substring. The first version grepped for `"external": true` and friends, which
# establishes three fragments of text: it cannot say the ledger is absent from
# the manifest's own file list, and it goes red on a formatting change that
# breaks nothing.
manifest_shape=$(jq -r '[
    .schema,
    (.ledger.external | tostring),
    .ledger.backend,
    .ledger.dsn_env,
    ([.files[].path | select(startswith("ledger/"))] | length | tostring)
] | join("|")' "${ARCHIVE}/manifest.json") ||
    fail "the archive's manifest is not readable JSON"

test "${manifest_shape}" = "2|true|postgres|${DSN_ENV}|0" ||
    fail "the manifest reads [${manifest_shape}] and an identity-only archive is [2|true|postgres|${DSN_ENV}|0]"

test -e "${ARCHIVE}/ledger" &&
    fail "the archive holds a ledger directory its manifest says does not exist"

# AND THE SECRET ITSELF IS NOWHERE IN IT. The manifest records the variable's
# NAME because a DSN carries a password and this directory travels off-site — so
# a regression that also wrote the VALUE would pass every assertion above while
# putting the database credential in the backup.
#
# grep's status is THREE-VALUED: 0 found, 1 not found, anything above 1 could not
# look. Folding the third into "not found" would turn an unreadable archive into
# a gate that passes.
set +e
grep -rqF -- "${BILLET_STATE_DSN}" "${ARCHIVE}"
dsn_hit=$?
set -e
case "${dsn_hit}" in
    0) fail "the archive contains the DSN itself, which carries the database password" ;;
    1) ;;
    *) fail "billet could not search the archive for the DSN (grep exited ${dsn_hit})" ;;
esac

# AND IT STILL CARRIES EVERY OTHER PIECE. That is the half of the fix that matters:
# the command used to fail outright, so an operator got NOTHING — not a smaller
# archive, no archive at all.
for entry in identity/deployment-id github/app-private-key.pem \
    authority/ca.key authority/ca.crt authority/authority-created; do
    test -f "${ARCHIVE}/${entry}" || fail "the archive is missing ${entry}"
done

# 0700 over 0600, because two of those files are private keys.
mode=$(stat -c '%a' "${ARCHIVE}")
test "${mode}" = "700" || fail "the archive directory is mode ${mode}, want 700"
mode=$(stat -c '%a' "${ARCHIVE}/github/app-private-key.pem")
test "${mode}" = "600" || fail "the archived App key is mode ${mode}, want 600"

step "a restore is REFUSED until the operator says the ledger is back"
# BILLET CANNOT CHECK THIS ONE, which is exactly why it asks. The database is on
# the other end of a connection string, and whether the one answering is the one
# this identity was minted beside is not something this process can establish.
# An identity restored beside an EMPTY ledger is a control plane that advertises
# capacity for a fleet it has no record of.
sed -e "s#identity_dir: ${IDENTITY_A}#identity_dir: ${IDENTITY_B}#" \
    -e 's#private_key_path: /etc/billet/app-private-key.pem#private_key_path: /etc/billet/restored-app-private-key.pem#' \
    "${CONFIG_A}" > "${CONFIG_B}"
edited "${CONFIG_B}" "identity_dir: ${IDENTITY_B}"
edited "${CONFIG_B}" "private_key_path: /etc/billet/restored-app-private-key.pem"

install -d -o billet -g billet -m 0700 "${IDENTITY_B}"

if billet local restore --config "${CONFIG_B}" --from "${ARCHIVE}" \
    --old-controller-fenced >/tmp/unattached.log 2>&1; then
    cat /tmp/unattached.log >&2
    fail "an identity-only restore was allowed without the operator asserting the ledger is back"
fi
grep -q -- '--external-ledger-attached' /tmp/unattached.log ||
    fail "the refusal does not name the flag that answers it"
untouched "the restore refused for want of the operator's assertion"

step "and it is REFUSED onto a host whose config would build its own ledger"
# THE HALF BILLET CAN CHECK. A config naming a local ledger leaves the control
# plane to create an empty billet.db beside the restored identity and start
# against it: every node's certificate valid, every lease gone. That is the same
# lost fleet as restoring with no ledger at all, arriving through a config
# mistake rather than a missing file.
sed -e "s#identity_dir: ${IDENTITY_B}#state_dir: ${IDENTITY_B}#" \
    -e '/^  state:$/d' -e '/^    backend: postgres$/d' \
    -e '/^    postgres:$/d' -e "/^      dsn_env: ${DSN_ENV}\$/d" \
    "${CONFIG_B}" > "${CONFIG_SQLITE}"
edited "${CONFIG_SQLITE}" "state_dir: ${IDENTITY_B}"
absent "${CONFIG_SQLITE}" "backend: postgres"

if billet local restore --config "${CONFIG_SQLITE}" --from "${ARCHIVE}" \
    --old-controller-fenced --external-ledger-attached >/tmp/wrong-backend.log 2>&1; then
    cat /tmp/wrong-backend.log >&2
    fail "an identity-only restore was allowed onto a host configured for a local ledger"
fi
grep -q 'empty ledger of its own' /tmp/wrong-backend.log ||
    fail "the refusal does not say what the mismatch would cost"
untouched "the restore refused for a ledger-backend mismatch"

step "restore it where nothing has seen it — as root, which is what an operator is"
# THE SAME DATABASE, A NEW IDENTITY DIRECTORY. That is the replacement-controller
# shape this profile exists for: the ledger survived the instance, and what has
# to come back is the half that did not.
# THE DRY RUN CARRIES THE SAME ASSERTION AS THE REAL ONE. Without it the plan
# still holds the refusal above and exits non-zero, which is correct — a preview
# that hid a refusal the real command will meet would be a preview of a different
# operation — and it is not what this step is here to show.
billet local restore --config "${CONFIG_B}" --from "${ARCHIVE}" \
    --external-ledger-attached --dry-run
untouched "the dry run"

billet local restore --config "${CONFIG_B}" --from "${ARCHIVE}" \
    --old-controller-fenced --external-ledger-attached

step "what root restored, and who owns it"
ls -la "${IDENTITY_B}" "${IDENTITY_B}/ca"

step "the deployment that came back is the same deployment"
cmp "${IDENTITY_A}/deployment-id" "${IDENTITY_B}/deployment-id" ||
    fail "the restored deployment identity differs from the original"
cmp "${IDENTITY_A}/ca/ca.crt" "${IDENTITY_B}/ca/ca.crt" ||
    fail "the restored authority certificate differs from the original"
cmp "${IDENTITY_A}/ca/ca.key" "${IDENTITY_B}/ca/ca.key" ||
    fail "the restored authority key differs from the original"
cmp /etc/billet/app-private-key.pem /etc/billet/restored-app-private-key.pem ||
    fail "the restored App key differs from the original; GitHub issues it once"

# AND NO LEDGER CAME WITH IT, which is the whole shape of this profile: the
# archive said the ledger is elsewhere, so a restore that produced one would be
# installing a second answer beside the database the config names.
test -e "${IDENTITY_B}/billet.db" &&
    fail "the restore installed a ledger the archive says it does not carry"

# ASKED OF A DIFFERENT TOOL, so this is not billet agreeing with itself: the
# certificate the ORIGINAL deployment issued still verifies against the restored
# authority, which is what a node's handshake will do.
openssl verify -CAfile "${IDENTITY_B}/ca/ca.crt" "${BUNDLE}/node.crt" >/dev/null ||
    fail "a certificate the original deployment issued does not verify against the restored authority"

step "the service account can use what root restored, against the ledger it kept"
# THE PROOF THAT MATTERS ON LINUX, reached through a different instrument here.
# A root-run restore writes root-owned files into a directory the service account
# owns, and systemd will not repair them — StateDirectory= repairs ownership
# recursively only when the TOP directory's owner is wrong. `billet server
# --upgrade-probe` is what the SQLite rehearsal asks and is REFUSED on an
# external ledger, so this asks the two commands that between them read the
# identity, open the CA private key and write to the ledger.
runuser -u billet -- env "${DSN_ENV}=${BILLET_STATE_DSN}" \
    billet ca issue node-b --config "${CONFIG_B}" --out /var/lib/billet/bundles/node-b ||
    fail "the service account could not use the deployment root restored"

openssl verify -CAfile "${IDENTITY_A}/ca/ca.crt" /var/lib/billet/bundles/node-b/node.crt >/dev/null ||
    fail "the restored authority issued a certificate the original authority does not vouch for"

# AND THE LEDGER IT WROTE TO IS THE ONE THE ORIGINAL USED.
#
# ASKED OF psql RATHER THAN OF BILLET, which is the same rule as verifying the
# node certificate with openssl: this is the assertion that the two halves were
# actually PAIRED, and billet reporting that it can see its own rows would be
# billet agreeing with itself.
#
# NOT `billet status`, WHICH WAS THE FIRST ATTEMPT AND PROVES SOMETHING ELSE. It
# lists REGISTERED nodes — machines that have connected — and nothing here ever
# starts a node, so it printed `held none` and named neither. An admission is a
# row in issued_certs, written by `billet ca issue`, and that is the thing whose
# presence says these two commands reached one database.
admitted=$(runuser -u postgres -- psql -d billet -tAc \
    "SELECT string_agg(node, ',' ORDER BY node) FROM issued_certs")
case "${admitted}" in
    'node-a,node-b') ;;
    *)
        fail "the ledger holds admissions for [${admitted}] and it must hold both node-a, which the ORIGINAL deployment issued, and node-b, which the RESTORED one did — anything less means the identity and the ledger were never paired"
        ;;
esac

# AND THE SERVICE ACCOUNT CAN READ IT THROUGH BILLET TOO, which is the other half
# of the ownership proof: `ca issue` above proved it can WRITE, and a control
# plane that cannot read its own ledger starts and reports an empty fleet.
runuser -u billet -- env "${DSN_ENV}=${BILLET_STATE_DSN}" \
    billet status --config "${CONFIG_B}" >/tmp/restored-status.log 2>&1 ||
    fail "the service account could not read the restored deployment's ledger"

step "and it can open the App key, which nothing above touches"
# THE ONE CREDENTIAL GITHUB ISSUES EXACTLY ONCE, and until this the rehearsal
# never opened it as the service account: `cmp` above runs as ROOT, and neither
# `ca issue` nor `status` reads it. A restore that repaired every identity file
# and left /etc/billet/restored-app-private-key.pem 0600 root:root passed — and
# the restored control plane then cannot authenticate to GitHub at all, which is
# the whole deployment being useless for a reason nothing reported.
#
# THROUGH `billet check`, NOT `openssl rsa`, because what has to hold is BILLET's
# read rather than readability in general: one descriptor opened O_NONBLOCK so a
# FIFO cannot hang it, regular file, bounded, MODE-CHECKED and actually parsed.
# openssl would pass on a key the hardened read refuses.
#
# BILLET_MAINTENANCE=1 SKIPS THE NETWORK, which is what makes this runnable at
# all: there is no App behind app_id 12345 and no GitHub to reach from here. What
# is left is every local check, and the App key is one of them.
runuser -u billet -- env BILLET_MAINTENANCE=1 "${DSN_ENV}=${BILLET_STATE_DSN}" \
    billet check --config "${CONFIG_B}" >/tmp/restored-check.log 2>&1 || {
    cat /tmp/restored-check.log >&2
    fail "the service account could not check the restored deployment"
}

# AND IT REALLY LOOKED AT THE KEY. `billet check` skipping it silently — because
# the config named no App, say — would make the step above pass while proving
# nothing about the credential it is here for.
grep -qi 'app-private-key\|app key\|private key' /tmp/restored-check.log || {
    cat /tmp/restored-check.log >&2
    fail "billet check said nothing about the App private key, so this step did not exercise it"
}

step "\`billet local recover\` is REFUSED on this profile, and says what to run instead"
# Recover exists to put a deployment back over ITSELF, and the only thing it
# moves is the ledger: it seals, waits for quiescence, and renames the live
# billet.db aside. None of that exists for a database billet does not hold.
if billet local recover --config "${CONFIG_A}" --from "${ARCHIVE}" \
    --old-controller-fenced >/tmp/recover.log 2>&1; then
    cat /tmp/recover.log >&2
    fail "billet local recover was allowed against an external ledger"
fi
grep -q 'billet local restore' /tmp/recover.log ||
    fail "the refusal does not name the operation that IS available"

echo
echo "postgres restore rehearsal: PASSED"
