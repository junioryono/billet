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
#   no-manifest   a release with no manifest is noted, records nothing, and is
#                 not asked again
#   refetch       a candidate staged without its evidence gets it, unchanged
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
cleanup() {
    [ -z "$origin_pid" ] || kill "$origin_pid" 2>/dev/null || true
    sudo -n rm -rf "$work"
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

release() { # version binary manifest-binary|none
    local version=$1 binary=$2 named=$3 dir=$work/origin/$1 dist=$work/dist-$1
    local archive=billet_${version#v}_linux_${arch}.tar.gz
    mkdir -p "$dir" "$dist/stage"
    cp "$binary" "$dist/stage/billet"
    tar -C "$dist/stage" -czf "$dir/$archive" billet
    (cd "$dir" && sha256sum "$archive" >checksums.txt)
    [ "$named" = none ] && return 0
    mkdir -p "$dist/named"
    cp "$named" "$dist/stage/billet"
    tar -C "$dist/stage" -czf "$dist/named/$archive" billet
    (cd "$repo_root" && go run ./scripts/mkreleasemanifest --dist "$dist/named" --version "$version" \
        --commit 0000000000000000000000000000000000000000 --out "$dir/release-manifest.json") >/dev/null
}
release v0.16.9 "$billet" "$billet"
release v0.16.10 "$billet" "$other"
release v0.16.11 "$billet" none

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
      when: case != 'supplied'

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

fail() { echo "FAIL $1" >&2; [ -z "${2:-}" ] || sed -n '/fatal\|FAILED/,+12p' "$2" >&2; exit 1; }

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

# no-manifest: noted once, recorded nothing.
run no-manifest v0.16.11
[ -f "$stage/billet-v0.16.11.release-manifest.absent" ] || fail "no-manifest: the absence was not noted"
[ ! -e "$record" ] || fail "no-manifest: something was recorded"
grep -q 'publishes no release-manifest.json' "$work/no-manifest.log" || fail "no-manifest: the absence was not said" "$work/no-manifest.log"
run no-manifest v0.16.11
grep -q 'TASK \[junioryono.billet.host : Download the release manifest' "$work/no-manifest.log" &&
    grep -A1 'TASK \[junioryono.billet.host : Download the release manifest' "$work/no-manifest.log" | grep -q '^ok:\|^changed:' &&
    fail "no-manifest: the absent manifest was asked for again" "$work/no-manifest.log"
echo "ok   no-manifest: a release with none is noted and not asked again"

# refetch: a candidate a collection from before #91 staged, with no evidence.
rm -f "$stage/billet-v0.16.9.tar.gz" "$stage/billet-v0.16.9.release-manifest.json"
candidate_before=$(sha "$stage/billet-v0.16.9")
run transaction v0.16.9
[ -f "$stage/billet-v0.16.9.tar.gz" ] && [ -f "$stage/billet-v0.16.9.release-manifest.json" ] ||
    fail "refetch: the evidence was not fetched for a staged candidate" "$work/transaction.log"
[ "$(sha "$stage/billet-v0.16.9")" = "$candidate_before" ] || fail "refetch: the staged candidate was replaced"
[ "$(field binary_sha256)" = "$(sha /usr/bin/billet)" ] || fail "refetch: nothing was recorded from the refetched evidence"
echo "ok   refetch: a candidate staged without evidence gets it and is not replaced"

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
