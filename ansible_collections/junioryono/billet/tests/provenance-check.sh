#!/bin/bash
# The host role records which release manifest produced the binary it installs
# (#91), through `billet release record` at the paths billet reads, and this
# runs it: the REAL billet, a REAL manifest written by scripts/mkreleasemanifest
# over the archive it names, served from a local origin, and the role's own task
# files (binary.yml, record-provenance.yml, backfill-provenance.yml), inside a
# private mount namespace whose /usr/bin, /var/lib and /etc are overlays on a
# tmpfs, so nothing on the host changes.
#
# The rules under test, in the order the cases run (each starts from what the
# one before left):
#   transaction   the evidence is kept beside the candidate, and the record
#                 names the manifest's sha256 and the installed binary's
#   unchanged     a binary the record already proves is not recorded again
#   backfill      a host with no record is recorded, and told when it applies
#   supplied      a supplied binary in the transaction leaves no record
#   refused       a manifest that does not name the archive leaves no record,
#                 says so, and fails nothing
#   no-manifest   a release with no manifest records nothing and says so, and
#                 the manifest published later is found by the next converge
#   refetch       a candidate staged without its evidence gets it, unchanged
#   origin-gone   a staged candidate whose origin stopped serving it converges,
#                 records nothing new, says why, and asks once
#   origin-down   an unreachable origin is not retried for evidence alone
#   rollback     the record before a transaction comes back after it, or stays
#                 away, and a journal from before the record was kept leaves it
#   check-mode    a dry run fetches nothing (the record steps are all gated off
#                 a dry run, which host-upgrade-order.yml holds)
#
# Needs root without a password (sudo -n), unshare, ansible-playbook and Go, on
# Linux; skips elsewhere.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collection_root=$(CDPATH= cd -- "$here/../../.." && pwd)
repo_root=${collection_root%/ansible_collections}

if [ "$(uname -s)" != Linux ]; then
    echo "provenance-check: skipped on $(uname -s); it needs a Linux mount namespace"
    exit 0
fi

sudo -n true 2>/dev/null || { echo "provenance-check: needs passwordless sudo, to unshare and mount" >&2; exit 1; }
command -v ansible-playbook >/dev/null || { echo "provenance-check: no ansible-playbook" >&2; exit 1; }

work=$(mktemp -d)
origin_pid=""
# PROVENANCE_CHECK_KEEP=1 keeps the work directory (every case's log and the
# origin) for a failure's diagnosis. What the cases wrote at the real paths
# lived in the private namespace's tmpfs, which is gone with it.
cleanup() {
    [ -z "$origin_pid" ] || kill "$origin_pid" 2>/dev/null || true
    if [ "${PROVENANCE_CHECK_KEEP:-0}" = 1 ]; then
        echo "provenance-check: the work directory is kept at $work"
    else
        sudo -n rm -rf "$work"
    fi
}
trap cleanup EXIT

case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "provenance-check: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# THE RELEASES. v0.16.9 is whole; v0.16.10 serves an archive its manifest does
# not name (the manifest was written over a different build); v0.16.11 publishes
# no manifest at all.
billet=${BILLET_EMITTED_BLOCK_BINARY:-}
if [ -z "$billet" ]; then
    (cd "$repo_root" && CGO_ENABLED=0 go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi
other=$work/other-billet
cp "$billet" "$other"
printf 'not the published build\n' >>"$other"

release() { # version binary served|other|none
    local version=$1 binary=$2 named=$3 dir=$work/origin/$1 dist=$work/dist-$1
    local archive=billet_${version#v}_linux_${arch}.tar.gz
    mkdir -p "$dir" "$dist/stage" "$dist/named"
    cp "$binary" "$dist/stage/billet"
    tar -C "$dist/stage" -czf "$dir/$archive" billet
    (cd "$dir" && sha256sum "$archive" >checksums.txt)
    case "$named" in
        # THE MANIFEST OF THE VERY ARCHIVE SERVED, byte for byte: archiving the
        # binary again would differ in its timestamps. `late` is written and
        # not published, for the case that publishes it after its archive.
        served | late) cp "$dir/$archive" "$dist/named/$archive" ;;
        other)
            cp "$other" "$dist/stage/billet"
            tar -C "$dist/stage" -czf "$dist/named/$archive" billet
            ;;
    esac
    (cd "$repo_root" && go run ./scripts/mkreleasemanifest --dist "$dist/named" --version "$version" \
        --commit 0000000000000000000000000000000000000000 --out "$dist/release-manifest.json") >/dev/null
    [ "$named" = late ] || cp "$dist/release-manifest.json" "$dir/release-manifest.json"
}
release v0.16.9 "$billet" served
release v0.16.10 "$billet" other
release v0.16.11 "$billet" late

python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
[ -x "${python:-}" ] || python=$(command -v python3)
"$python" -u -m http.server --bind 127.0.0.1 --directory "$work/origin" 0 >"$work/origin.log" 2>&1 &
origin_pid=$!
port=""
for _ in $(seq 1 100); do
    port=$(sed -n 's/.*port \([0-9]*\).*/\1/p' "$work/origin.log" | head -1)
    [ -n "$port" ] && break
    sleep 0.1
done
[ -n "$port" ] || { echo "provenance-check: the origin did not start" >&2; cat "$work/origin.log" >&2; exit 1; }

cat >"$work/play.yml" <<'PLAY'
- name: provenance
  hosts: localhost
  connection: local
  gather_facts: true
  vars:
    billet_release_url_base: "{{ origin }}"
    billet_release_stage: /var/lib/billet-provenance-check/stage
    billet_fetch_retries: 1
    billet_fetch_retry_delay: 0
    billet_resolved_version: "{{ billet_version }}"
  tasks:
    - name: Fetch the pinned release as the role does
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: binary
      when: case in ['transaction', 'unchanged', 'backfill', 'refused', 'no-manifest', 'check-mode']

    - name: Preserve the record beside the journal as the transaction does
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: preserve-provenance
      when: case == 'preserve'

    - name: Restore the record as the rollback does
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: restore-provenance
      when: case == 'restore'

    - name: Install the candidate as the transaction does
      ansible.builtin.copy:
        src: "{{ billet_candidate_path }}"
        dest: /usr/bin/billet
        remote_src: true
        mode: "0755"
      when: case in ['transaction', 'refused', 'no-manifest']

    - name: Record as the transaction does
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: record-provenance
      vars:
        billet_provenance_replaces: true
        billet_provenance_archive: "{{ '' if case == 'supplied' else billet_candidate_evidence_archive }}"
        billet_provenance_manifest: "{{ '' if case == 'supplied' else billet_candidate_evidence_manifest }}"
      when: case in ['transaction', 'refused', 'no-manifest', 'supplied']

    - name: Inspect the installed binary as stage-candidate.yml does
      ansible.builtin.stat:
        path: /usr/bin/billet
        checksum_algorithm: sha256
      register: billet_installed_binary
      when: case in ['unchanged', 'backfill']

    - name: Backfill as services.yml does
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: backfill-provenance
      when: case in ['unchanged', 'backfill']
PLAY

cat >"$work/inner.sh" <<'INNER'
#!/bin/bash
# Inside the namespace, as root: the overlays, then every case in order.
set -eu
work=$1 origin=$2 collections=$3 python=$4
mnt=$work/mnt
mkdir -p "$mnt"
mount -t tmpfs tmpfs "$mnt"
for dir in usr/bin var/lib etc; do
    name=${dir//\//-}
    mkdir -p "$mnt/$name-upper" "$mnt/$name-work"
    mount -t overlay overlay -o "lowerdir=/$dir,upperdir=$mnt/$name-upper,workdir=$mnt/$name-work" "/$dir"
done
rm -rf /var/lib/billet /var/lib/billet-provenance-check /usr/bin/billet
mkdir -p /var/lib/billet

record=/var/lib/billet/installed.json
stage=/var/lib/billet-provenance-check/stage

fail() {
    echo "FAIL $1" >&2
    if [ -n "${2:-}" ]; then
        if grep -q 'fatal\|FAILED' "$2"; then
            sed -n '/fatal\|FAILED/,+12p' "$2" >&2
        else
            grep -A8 'TASK \[junioryono.billet.host : \(Download the release manifest\|Keep the release manifest\|Record which release manifest\|Say that nothing records\|Say that nothing here can name\)' "$2" >&2
        fi
    fi
    exit 1
}

run() { # case version [ansible-playbook args]
    local name=$1 version=$2
    shift 2
    ANSIBLE_COLLECTIONS_PATH=$collections ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_NOCOLOR=1 \
        ansible-playbook -i localhost, -e "case=$name" -e "billet_version=$version" -e "origin=$origin" \
        "$@" "$work/play.yml" >"$work/$name.log" 2>&1 || fail "$name: the play failed" "$work/$name.log"
}

field() { "$python" -I -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$record" "$1"; }
sha() { sha256sum "$1" | cut -d' ' -f1; }
changed() { sed -n 's/.*changed=\([0-9]*\).*/\1/p' "$work/$1.log" | tail -1; }

# transaction: kept evidence, and a record of exactly these bytes.
run transaction v0.16.9
for kept in "$stage/billet-v0.16.9.tar.gz" "$stage/billet-v0.16.9.release-manifest.json"; do
    [ -f "$kept" ] || fail "transaction: $kept was not kept beside the candidate"
done
[ -f "$record" ] || fail "transaction: nothing was recorded" "$work/transaction.log"
[ "$(field manifest_digest)" = "$(sha "$work/origin/v0.16.9/release-manifest.json")" ] ||
    fail "transaction: the record names $(field manifest_digest), not the published manifest's digest"
[ "$(field binary_sha256)" = "$(sha /usr/bin/billet)" ] || fail "transaction: the record names other bytes"
[ "$(field version)" = v0.16.9 ] || fail "transaction: the record names $(field version)"
cmp -s "$stage/billet-v0.16.9.release-manifest.json" "$work/origin/v0.16.9/release-manifest.json" ||
    fail "transaction: the kept manifest is not the bytes the origin served"
echo "ok   transaction: the record names the published manifest and the installed bytes"

# unchanged: proved already, so not recorded again.
before=$(sha "$record")
run unchanged v0.16.9
[ "$(sha "$record")" = "$before" ] || fail "unchanged: the record was rewritten"
grep -q 'TASK \[junioryono.billet.host : Record which release manifest produced the installed binary\]' "$work/unchanged.log" &&
    fail "unchanged: billet release record ran for a binary the record already proves" "$work/unchanged.log"
[ "$(changed unchanged)" = 0 ] || fail "unchanged: the converge changed something ($(changed unchanged))" "$work/unchanged.log"
echo "ok   unchanged: a binary the record proves is left alone, and nothing changes"

# backfill: no record, so one is written, and the operator is told when it applies.
rm -f "$record"
run backfill v0.16.9
[ "$(field binary_sha256)" = "$(sha /usr/bin/billet)" ] || fail "backfill: nothing proving the installed bytes was recorded" "$work/backfill.log"
grep -q "from this node's next start" "$work/backfill.log" || fail "backfill: the run did not say when the record applies" "$work/backfill.log"
echo "ok   backfill: an unrecorded host is recorded, without a restart"

# supplied: the transaction's removal stands alone.
run supplied v0.16.9
[ ! -e "$record" ] || fail "supplied: a record survived a binary nothing can attest to"
grep -q 'There is no release evidence to record from' "$work/supplied.log" || fail "supplied: the run did not say why nothing was recorded" "$work/supplied.log"
echo "ok   supplied: a supplied binary leaves no record"

# refused: the manifest does not name the served archive.
run transaction v0.16.9
run refused v0.16.10
[ ! -e "$record" ] || fail "refused: a record survived a refused attestation"
grep -q 'billet release record refused' "$work/refused.log" || fail "refused: the refusal was not reported" "$work/refused.log"
grep -q 'nothing was recorded' "$work/refused.log" || fail "refused: billet's reason was not passed on" "$work/refused.log"
echo "ok   refused: a manifest that does not name the archive records nothing and fails nothing"

# no-manifest: recorded nothing, said so, and asks again on the next converge,
# so a manifest published after its archive is found (404 then 200).
run no-manifest v0.16.11
[ ! -e "$stage/billet-v0.16.11.release-manifest.json" ] || fail "no-manifest: a 404 was kept as a manifest"
[ ! -e "$record" ] || fail "no-manifest: something was recorded"
grep -q 'publishes no release-manifest.json' "$work/no-manifest.log" || fail "no-manifest: the absence was not said" "$work/no-manifest.log"
# PUBLISHED READABLE: this runs as root, and the origin serves as the caller.
install -m 0644 "$work/dist-v0.16.11/release-manifest.json" "$work/origin/v0.16.11/release-manifest.json"
run no-manifest v0.16.11
[ -f "$record" ] || fail "no-manifest: a manifest published after its archive was never found" "$work/no-manifest.log"
[ "$(field manifest_digest)" = "$(sha "$work/origin/v0.16.11/release-manifest.json")" ] ||
    fail "no-manifest: the record does not name the manifest published late"
echo "ok   no-manifest: a release with none records nothing, and its manifest is found once published"

# refetch: a candidate a collection from before #91 staged, with no evidence.
rm -f "$stage/billet-v0.16.9.tar.gz" "$stage/billet-v0.16.9.release-manifest.json"
candidate_before=$(sha "$stage/billet-v0.16.9")
run transaction v0.16.9
[ -f "$stage/billet-v0.16.9.tar.gz" ] && [ -f "$stage/billet-v0.16.9.release-manifest.json" ] ||
    fail "refetch: the evidence was not fetched for a staged candidate" "$work/transaction.log"
[ "$(sha "$stage/billet-v0.16.9")" = "$candidate_before" ] || fail "refetch: the staged candidate was replaced"
[ "$(field binary_sha256)" = "$(sha /usr/bin/billet)" ] || fail "refetch: nothing was recorded from the refetched evidence"
echo "ok   refetch: a candidate staged without evidence gets it and is not replaced"

# origin-gone: the evidence is fetched again for a staged candidate as a
# courtesy, and an origin that no longer serves the release fails nothing.
rm -f "$stage/billet-v0.16.9.tar.gz" "$stage/billet-v0.16.9.release-manifest.json"
mv "$work/origin/v0.16.9" "$work/origin/v0.16.9.gone"
record_before=$(sha "$record")
asked_before=$(grep -c 'GET /v0.16.9/checksums.txt' "$work/origin.log" || true)
run backfill v0.16.9
asked=$(($(grep -c 'GET /v0.16.9/checksums.txt' "$work/origin.log" || true) - asked_before))
mv "$work/origin/v0.16.9.gone" "$work/origin/v0.16.9"
grep -q 'could not be fetched again beside the staged candidate' "$work/backfill.log" ||
    fail "origin-gone: the run did not say the evidence could not be fetched" "$work/backfill.log"
[ "$(sha "$record")" = "$record_before" ] || fail "origin-gone: the record changed without evidence"
# ASKED ONCE: the play allows a retry, which evidence for a staged candidate
# must not spend.
[ "$asked" -eq 1 ] || fail "origin-gone: the evidence was asked for ${asked} times, not once" "$work/origin.log"

# origin-down: an origin that refuses connections is a failure the role retries
# for a candidate it needs, and must not for evidence it does not: neither the
# archive nor the manifest is asked again.
rm -f "$stage/billet-v0.16.9.tar.gz" "$stage/billet-v0.16.9.release-manifest.json"
record_before=$(sha "$record")
run backfill v0.16.9 -e "origin=http://127.0.0.1:9"
if grep -q 'FAILED - RETRYING' "$work/backfill.log"; then
    fail "origin-down: evidence beside a staged candidate was retried" "$work/backfill.log"
fi
grep -q 'could not be fetched again beside the staged candidate' "$work/backfill.log" ||
    fail "origin-down: the run did not say the evidence could not be fetched" "$work/backfill.log"
[ "$(sha "$record")" = "$record_before" ] || fail "origin-down: the record changed without evidence"
echo "ok   origin-down: evidence an unreachable origin cannot serve is asked for once and fails nothing"
echo "ok   origin-gone: an origin that stopped serving the release fails nothing and says so"

# rollback: the record comes back as it was before the transaction, from what
# the transaction left beside its journal, whatever the transaction recorded
# in between; a journal from before the role kept the record leaves it alone.
journal=/var/lib/billet-provenance-check/recovery
rollback() { # name, then whether a record exists before the transaction
    rm -rf "$journal" && mkdir -p "$journal"
    run preserve v0.16.9 -e "billet_upgrade_recovery_dir=$journal"
    printf '{"version": "v9.9.9", "manifest_digest": "candidate", "binary_sha256": "candidate"}\n' >"$record"
    run restore v0.16.9 -e "billet_upgrade_recovery_dir=$journal"
}
before=$(cat "$record")
rollback existing
[ "$(cat "$record")" = "$before" ] || fail "rollback: the record before the transaction did not come back" "$work/restore.log"
[ -f "$journal/installed.json.previous" ] && [ ! -e "$journal/installed.json.absent" ] ||
    fail "rollback: an existing record was not preserved as installed.json.previous"

rm -f "$record"
rollback absent
[ ! -e "$record" ] || fail "rollback: a host that had no record kept the candidate's" "$work/restore.log"
[ -f "$journal/installed.json.absent" ] && [ ! -e "$journal/installed.json.previous" ] ||
    fail "rollback: an absent record was not noted as installed.json.absent"

rm -rf "$journal" && mkdir -p "$journal"
printf '{"version": "v0.16.9", "manifest_digest": "kept", "binary_sha256": "kept"}\n' >"$record"
run restore v0.16.9 -e "billet_upgrade_recovery_dir=$journal"
grep -q '"manifest_digest": "kept"' "$record" || fail "rollback: a journal from before the record was kept changed it"

touch "$journal/installed.json.absent"
cp "$record" "$journal/installed.json.previous"
if ANSIBLE_COLLECTIONS_PATH=$collections ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_NOCOLOR=1 \
    ansible-playbook -i localhost, -e case=restore -e billet_version=v0.16.9 -e "origin=$origin" \
    -e "billet_upgrade_recovery_dir=$journal" "$work/play.yml" >"$work/contradiction.log" 2>&1; then
    fail "rollback: a journal holding both the record and its absence was restored from"
fi
grep -q 'contradicts itself' "$work/contradiction.log" || fail "rollback: the contradiction was not refused for itself" "$work/contradiction.log"
echo "ok   rollback: the record comes back as it was, or stays away, and a journal from before leaves it alone"

# check-mode: a release not yet staged, dry run.
run check-mode v0.16.9 --check -e billet_release_stage=/var/lib/billet-provenance-check/dry
for dry in billet-v0.16.9 billet-v0.16.9.tar.gz billet-v0.16.9.release-manifest.json; do
    [ ! -e "/var/lib/billet-provenance-check/dry/$dry" ] || fail "check-mode: a dry run fetched $dry"
done
grep -q 'would-fetch=' "$work/check-mode.log" || fail "check-mode: the dry run did not say what it would fetch" "$work/check-mode.log"
echo "ok   check-mode: a dry run fetches nothing"
INNER
chmod 0755 "$work/inner.sh"

collections="$collection_root:$HOME/.ansible/collections:/usr/share/ansible/collections"
sudo -n env "PATH=$PATH" unshare -m --propagation private /bin/bash "$work/inner.sh" \
    "$work" "http://127.0.0.1:$port" "$collections" "$python"
echo "provenance-check: every case behaved as the rules require"
