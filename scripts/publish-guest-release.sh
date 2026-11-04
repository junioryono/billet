#!/bin/sh
set -eu

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${SOURCE_SHA:?SOURCE_SHA is required: the commit of the release the image was built from}"
: "${TAG:?TAG is required}"

out=${1:-out}
if ! printf '%s\n' "$TAG" | grep -Eq '^guest-[0-9]{8}-[0-9]{6}$'; then
    printf 'invalid guest release tag: %s\n' "$TAG" >&2
    exit 2
fi
case "$SOURCE_SHA" in
    ''|*[!0-9a-f]*) printf 'invalid source commit: %s\n' "$SOURCE_SHA" >&2; exit 2 ;;
esac
case "${#SOURCE_SHA}" in
    40|64) ;;
    *) printf 'invalid source commit length: %s\n' "$SOURCE_SHA" >&2; exit 2 ;;
esac

# ONE RELEASE, ONE OR TWO SETS (ADR-022). manifest.json and what it names are the
# x86_64 image, which every release carries and every deployed reader reads; a release
# may also carry the aarch64 image, described by manifest-aarch64.json, whose every
# asset names its architecture. A set is checked whole, and nothing is tagged until
# each is, because a release is immutable.
#
# check_set SUFFIX proves one set and adds its files to $scratch/upload.
#
# THE ASSETS COME FROM THE MANIFEST, NOT FROM A LIST HERE.
#
# A parity-sized image is published as parts, because a release asset must be
# under 2 GiB -- so the set of files to upload is decided by the build and can no
# longer be spelled out in this script. Naming them here again would be a second
# list to keep in step with the first, and the failure when they drift is a
# release whose manifest promises an asset nobody uploaded: every node fetches it,
# gets a 404, and the image is undownloadable until somebody republishes.
#
# EVERY NAMED FILE MUST EXIST BEFORE ANYTHING IS PUBLISHED. A release is
# immutable once created, so a missing asset cannot be added afterwards under the
# same tag -- the check has to happen here, not after the upload.
#
# THE ASSEMBLED NAME IS NOT AN ASSET. For a multipart image the manifest's
# rootfs_multipart.name is what the parts join INTO on the node; only the parts
# are uploaded. Uploading the assembled name would mean publishing the whole
# file, which is the thing that does not fit.
#
# AND EACH IS THE FILE THE MANIFEST DESCRIBES, by its digest. An artifact download
# that ran out of disk reported success with files missing (2026-09-29), so a file
# that is present can still be a truncated one, and nothing but its digest says so.
#
# EACH FIELD IS PROVED IN THE JSON, before anything splits it on whitespace: a name
# the reader would accept (its own pattern) and a sha256 of 64 lowercase hex
# digits. Split first, a name carrying a space and a digest would be checked as a
# different file from the one the manifest promises.
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
: >"$scratch/upload"

pairs() { # program file out
    awk -F '\t' "$1" "$2" >"$3.raw" && LC_ALL=C sort -u "$3.raw" >"$3"
}

check_set() {
manifest="manifest$1.json"
suffix=$1
sources="image-sources$1"

assets=$(jq -r '
    [.kernel]
    + (if .rootfs_multipart then .rootfs_multipart.parts else [.rootfs] end)
    | .[]
    | if ((.name | type) == "string" and (.name | test("^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"))
          and (.sha256 | type) == "string" and (.sha256 | test("^[0-9a-f]{64}$")))
      then "\(.name) \(.sha256)"
      else error("the manifest names an asset with an unusable name or digest: \(.name | tojson)")
      end
' "$out/$manifest")

if [ -z "$assets" ]; then
    printf 'the manifest %s names no assets to publish\n' "$manifest" >&2
    exit 2
fi

# THE SIGNATURE AND THE SOURCE MANIFEST TOO, before any ref is written.
# image-sources.tsv is the image's /opt/billet/IMAGE_SOURCES.tsv, which the gate held
# to what the image installed: every package, its source and where apt could fetch
# it, which a release that redistributes GNU-licensed packages has to say (#584).
for file in "manifest$suffix.sigstore.json" "$sources.tsv"; do
    if [ ! -r "$out/$file" ]; then
        printf 'the publication has no %s, so nothing is tagged or released\n' "$file" >&2
        exit 2
    fi
done

# THE SOURCE MANIFEST IS ITS HEADER AND ONE OR MORE COMPLETE ROWS: seven cells,
# none empty. The gate compared this copy with the image's own; this refuses an
# empty, truncated or reformatted file handed over in its place.
#
# A BAD ROW IS RECORDED, NOT EXITED ON: awk runs END after an exit in a rule, and
# an END that exits again replaces the status.
if ! awk -F '\t' '
    NR == 1 { if ($0 != "package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense") bad = 1; next }
    { if (NF != 7) bad = 1; for (i = 1; i <= 7; i++) if ($i == "") bad = 1; rows++ }
    END { exit (bad || rows == 0) ? 1 : 0 }' "$out/$sources.tsv"; then
    printf 'the publication'"'"'s %s.tsv is not a source manifest naming a package, so nothing is tagged or released\n' "$sources" >&2
    exit 2
fi

# THE CORRESPONDING SOURCE (#584, ADR-019), proved whole before anything is
# tagged: a release is immutable, so a source missing from it can never be added.
#
# - image-sources.parts names the archive: its first line is the whole tar, its
#   sha256 and size; any further lines are its parts, .part000 upwards, each with
#   its sha256. The parts joined in order must be the whole.
# - image-sources-index.tsv must be the index the archive itself carries, byte for
#   byte, and must account for exactly the source packages image-sources.tsv
#   marks gnu or unclassified: each fetched, or, for an unclassified one only,
#   reviewed as needing none. It also names the guest kernel's source, once.
for file in "$sources-index.tsv" "$sources.parts"; do
    if [ ! -s "$out/$file" ]; then
        printf 'the publication has no %s, so nothing is tagged or released\n' "$file" >&2
        exit 2
    fi
done
if [ "$(head -n 1 "$out/$sources-index.tsv")" != "$(printf 'source\tversion\tstatus\tfile\tsha256\tsize')" ]; then
    printf 'the publication'"'"'s %s-index.tsv is not an index of the source, so nothing is tagged or released\n' "$sources" >&2
    exit 2
fi

work=$(mktemp -d "$scratch/set.XXXXXX")

name='' whole='' size='' extra=''
read -r name whole size extra <"$out/$sources.parts" || [ -n "$name" ] || true
if [ "$name" != "$sources.tar" ] || [ -n "$extra" ] ||
    ! printf '%s\n' "$whole" | grep -Eq '^[0-9a-f]{64}$' ||
    ! printf '%s\n' "$size" | grep -Eq '^[0-9]+$'; then
    printf '%s.parts does not begin with the whole archive, its sha256 and size\n' "$sources" >&2
    exit 2
fi

sed 1d "$out/$sources.parts" >"$work/parts"
# THE ARCHIVE BY ITS FILE NAMES ALONE, each checked against the one pattern a
# part may have, so the list splits safely on spaces and is read from inside $out,
# whatever $out's own path holds.
archive=''
n=0
# A LAST LINE WITH NO NEWLINE IS A PART TOO: read fails on it while still setting
# the fields, and skipping it would publish the tar without its last part.
while read -r part want rest || [ -n "$part" ]; do
    expected=$(printf '%s.tar.part%03d' "$sources" "$n")
    if [ "$part" != "$expected" ]; then
        printf '%s.parts names %s where it should name %s\n' "$sources" "$part" "$expected" >&2
        exit 2
    fi
    if [ -n "$rest" ] || ! printf '%s\n' "$want" | grep -Eq '^[0-9a-f]{64}$'; then
        printf '%s.parts gives %s no single sha256\n' "$sources" "$part" >&2
        exit 2
    fi
    if [ ! -r "$out/$part" ]; then
        printf '%s.parts names %s and the build did not produce it\n' "$sources" "$part" >&2
        exit 2
    fi
    got=$(sha256sum "$out/$part" | cut -d ' ' -f 1)
    if [ "$got" != "$want" ]; then
        printf '%s is not the part %s.parts describes (sha256 %s, want %s)\n' "$part" "$sources" "$got" "$want" >&2
        exit 2
    fi
    archive="$archive $part"
    n=$((n + 1))
done <"$work/parts"
if [ "$n" -eq 0 ]; then
    archive=$sources.tar
    if [ ! -r "$out/$archive" ]; then
        printf '%s.parts names the whole archive and the build did not produce it\n' "$sources" >&2
        exit 2
    fi
fi

# shellcheck disable=SC2086
got=$(cd "$out" && cat $archive | sha256sum | cut -d ' ' -f 1)
# shellcheck disable=SC2086
length=$(cd "$out" && cat $archive | wc -c | tr -d ' ')
if [ "$got" != "$whole" ] || [ "$length" != "$size" ]; then
    printf 'the source archive'"'"'s parts are not the archive %s.parts describes (sha256 %s, %s bytes; want %s, %s bytes)\n' "$sources" "$got" "$length" "$whole" "$size" >&2
    exit 2
fi
# shellcheck disable=SC2086
if ! (cd "$out" && cat $archive) | tar -xOf - ./index.tsv >"$work/index" ||
    ! cmp -s "$work/index" "$out/$sources-index.tsv"; then
    printf '%s-index.tsv is not the index the source archive carries\n' "$sources" >&2
    exit 2
fi

# EACH SET IS EXTRACTED TO A FILE WHOSE STATUS IS CHECKED, then sorted: dash has
# no pipefail, so an awk that failed into a sort would read as an empty set, and an
# empty set of reviewed or GNU-licensed sources is a pass.
if ! pairs 'NR > 1 && ($7 == "gnu" || $7 == "unclassified") { print $4 "\t" $5 }' "$out/$sources.tsv" "$work/needed" ||
    ! pairs 'NR > 1 && $7 == "gnu" { print $4 "\t" $5 }' "$out/$sources.tsv" "$work/gnu" ||
    ! pairs 'NR > 1 && $1 != "guest-kernel" && ($3 == "fetched" || $3 == "reviewed") { print $1 "\t" $2 }' \
        "$out/$sources-index.tsv" "$work/indexed" ||
    ! pairs 'NR > 1 && $3 == "reviewed" { print $1 "\t" $2 }' "$out/$sources-index.tsv" "$work/reviewed" ||
    ! pairs 'NR > 1 && $1 == "guest-kernel" && $3 == "fetched" { print $2 }' "$out/$sources-index.tsv" "$work/kernels"; then
    printf 'could not read the sources %s.tsv needs or %s-index.tsv holds, so nothing is tagged or released\n' "$sources" "$sources" >&2
    exit 2
fi

# THE KERNEL'S SOURCE IS THE KERNEL THE MANIFEST NAMES, once.
kernels=$(cat "$work/kernels")
kernel=$(jq -r '.kernel.version // empty' "$out/$manifest")
# A FAILED comm IS NOT AN EMPTY ONE: its status is checked before its output is
# read, and it collates as the sorts above did.
if ! LC_ALL=C comm -12 "$work/reviewed" "$work/gnu" >"$work/reviewed-gnu"; then
    printf 'could not compare the reviewed sources with the GNU-licensed ones, so nothing is tagged or released\n' >&2
    exit 2
fi
if ! cmp -s "$work/needed" "$work/indexed" || [ -s "$work/reviewed-gnu" ] ||
    [ -z "$kernel" ] || [ "$kernels" != "$kernel" ] || [ ! -s "$work/needed" ]; then
    printf '%s-index.tsv does not account for exactly the sources %s.tsv needs, and kernel %s'"'"'s\n' "$sources" "$sources" "${kernel:-?}" >&2
    exit 2
fi

{
    printf '%s\n' "$manifest" "manifest$suffix.sigstore.json" "$sources.tsv" "$sources-index.tsv" "$sources.parts"
    for part in $archive; do
        printf '%s\n' "$part"
    done
} >>"$scratch/upload"

while read -r name want; do
    # A BARE FILE NAME, CHECKED HERE TOO. The Go reader validates this before a
    # node acts on it, but this script runs first and interpolates the value into
    # a path -- so a name carrying a separator would upload something from
    # outside the output directory.
    case "$name" in
        */*|..|.|'') printf 'the manifest names an unusable asset: %s\n' "$name" >&2; exit 2 ;;
    esac

    if [ ! -r "$out/$name" ]; then
        printf 'the manifest names %s and the build did not produce it; a release is\n' "$name" >&2
        printf 'immutable, so a missing asset cannot be added under this tag afterwards\n' >&2
        exit 2
    fi

    got=$(sha256sum "$out/$name" | cut -d ' ' -f 1)
    if [ "$got" != "$want" ]; then
        printf '%s is not the file the manifest describes (sha256 %s, want %s); a\n' "$name" "$got" "$want" >&2
        printf 'truncated download is published as a release no node can use\n' >&2
        exit 2
    fi

    printf '%s\n' "$name" >>"$scratch/upload"
done <<ASSETS
$assets
ASSETS
}

# THE NOTES ARE THE RELEASE'S, proved before any ref is written: gh reads them only
# once the tag exists, so a missing file there would leave the tag behind.
if [ ! -r "$out/release-notes.md" ]; then
    printf 'the publication has no release-notes.md, so nothing is tagged or released\n' >&2
    exit 2
fi

check_set ""

# A SET IS THERE ONLY AS A WHOLE, of the machine its name says and the contract the
# release speaks. An aarch64 manifest of another contract is one no node of this
# release's billet can import, and the channel would still say the release carries it.
if [ -e "$out/manifest-aarch64.json" ]; then
    if ! jq -e '.arch == "aarch64"' "$out/manifest-aarch64.json" >/dev/null; then
        printf 'manifest-aarch64.json does not describe an aarch64 image, so nothing is tagged or released\n' >&2
        exit 2
    fi
    if [ "$(jq -r '.guest_contract' "$out/manifest-aarch64.json")" != "$(jq -r '.guest_contract' "$out/manifest.json")" ]; then
        printf 'manifest-aarch64.json speaks another guest contract than manifest.json, so nothing is tagged or released\n' >&2
        exit 2
    fi
    check_set -aarch64
fi

set --
while IFS= read -r name; do
    set -- "$@" "$out/$name"
done <"$scratch/upload"

# THE TAG POINTS AT THE RELEASE COMMIT THE IMAGE WAS BUILT FROM, never at the
# main commit that published it: the manifest's schema is closed, so the tag's
# target is the provenance record.
#
# THE TAG IS CREATED BEFORE THE RELEASE, AND AFTER EVERY ASSET IS PROVED. --target
# affects only a missing tag, so handing an existing attacker-chosen tag to gh
# release create would publish the checked assets under a different commit. Both
# local and remote conflicts fail; neither command is allowed to rewrite a ref. A
# refusal above leaves no tag, where a tag pushed first stayed behind as a dated
# name with no release.
git tag "$TAG" "$SOURCE_SHA"
git push origin "refs/tags/$TAG"

gh release create "$TAG" \
    --repo "$GITHUB_REPOSITORY" \
    --verify-tag \
    --prerelease \
    --title "Guest image $TAG" \
    --notes-file "$out/release-notes.md" \
    "$@"

immutable=$(gh release view "$TAG" \
    --repo "$GITHUB_REPOSITORY" --json isImmutable --jq .isImmutable)
if [ "$immutable" != true ]; then
    printf 'published guest release is not immutable\n' >&2
    exit 1
fi
