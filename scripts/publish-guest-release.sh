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
assets=$(jq -r '
    [.kernel]
    + (if .rootfs_multipart then .rootfs_multipart.parts else [.rootfs] end)
    | .[]
    | if ((.name | type) == "string" and (.name | test("^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$"))
          and (.sha256 | type) == "string" and (.sha256 | test("^[0-9a-f]{64}$")))
      then "\(.name) \(.sha256)"
      else error("the manifest names an asset with an unusable name or digest: \(.name | tojson)")
      end
' "$out/manifest.json")

if [ -z "$assets" ]; then
    printf 'the manifest names no assets to publish\n' >&2
    exit 2
fi

# THE SIGNATURE, THE NOTES AND THE SOURCE MANIFEST TOO, before any ref is written:
# gh reads the notes only once the tag exists, so a missing file there would leave
# the tag behind. image-sources.tsv is the image's /opt/billet/IMAGE_SOURCES.tsv,
# which the gate held to what the image installed: every package, its source and
# where apt could fetch it, which a release that redistributes GNU-licensed
# packages has to say (#584).
for file in manifest.sigstore.json release-notes.md image-sources.tsv; do
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
    END { exit (bad || rows == 0) ? 1 : 0 }' "$out/image-sources.tsv"; then
    printf 'the publication'"'"'s image-sources.tsv is not a source manifest naming a package, so nothing is tagged or released\n' >&2
    exit 2
fi

# THE CORRESPONDING SOURCE (#584, ADR-018), proved whole before anything is
# tagged: a release is immutable, so a source missing from it can never be added.
#
# - image-sources.parts names the archive: its first line is the whole tar, its
#   sha256 and size; any further lines are its parts, .part000 upwards, each with
#   its sha256. The parts joined in order must be the whole.
# - image-sources-index.tsv must be the index the archive itself carries, byte for
#   byte, and must account for exactly the source packages image-sources.tsv
#   marks gnu or unclassified: each fetched, or, for an unclassified one only,
#   reviewed as needing none. It also names the guest kernel's source, once.
for file in image-sources-index.tsv image-sources.parts; do
    if [ ! -s "$out/$file" ]; then
        printf 'the publication has no %s, so nothing is tagged or released\n' "$file" >&2
        exit 2
    fi
done
if [ "$(head -n 1 "$out/image-sources-index.tsv")" != "$(printf 'source\tversion\tstatus\tfile\tsha256\tsize')" ]; then
    printf 'the publication'"'"'s image-sources-index.tsv is not an index of the source, so nothing is tagged or released\n' >&2
    exit 2
fi

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

name='' whole='' size='' extra=''
read -r name whole size extra <"$out/image-sources.parts" || [ -n "$name" ] || true
if [ "$name" != image-sources.tar ] || [ -n "$extra" ] ||
    ! printf '%s\n' "$whole" | grep -Eq '^[0-9a-f]{64}$' ||
    ! printf '%s\n' "$size" | grep -Eq '^[0-9]+$'; then
    printf 'image-sources.parts does not begin with the whole archive, its sha256 and size\n' >&2
    exit 2
fi

sed 1d "$out/image-sources.parts" >"$scratch/parts"
# THE ARCHIVE BY ITS FILE NAMES ALONE, each checked against the one pattern a
# part may have, so the list splits safely on spaces and is read from inside $out,
# whatever $out's own path holds.
archive=''
n=0
# A LAST LINE WITH NO NEWLINE IS A PART TOO: read fails on it while still setting
# the fields, and skipping it would publish the tar without its last part.
while read -r part want rest || [ -n "$part" ]; do
    expected=$(printf 'image-sources.tar.part%03d' "$n")
    if [ "$part" != "$expected" ]; then
        printf 'image-sources.parts names %s where it should name %s\n' "$part" "$expected" >&2
        exit 2
    fi
    if [ -n "$rest" ] || ! printf '%s\n' "$want" | grep -Eq '^[0-9a-f]{64}$'; then
        printf 'image-sources.parts gives %s no single sha256\n' "$part" >&2
        exit 2
    fi
    if [ ! -r "$out/$part" ]; then
        printf 'image-sources.parts names %s and the build did not produce it\n' "$part" >&2
        exit 2
    fi
    got=$(sha256sum "$out/$part" | cut -d ' ' -f 1)
    if [ "$got" != "$want" ]; then
        printf '%s is not the part image-sources.parts describes (sha256 %s, want %s)\n' "$part" "$got" "$want" >&2
        exit 2
    fi
    archive="$archive $part"
    n=$((n + 1))
done <"$scratch/parts"
if [ "$n" -eq 0 ]; then
    archive=image-sources.tar
    if [ ! -r "$out/$archive" ]; then
        printf 'image-sources.parts names the whole archive and the build did not produce it\n' >&2
        exit 2
    fi
fi

# shellcheck disable=SC2086
got=$(cd "$out" && cat $archive | sha256sum | cut -d ' ' -f 1)
# shellcheck disable=SC2086
length=$(cd "$out" && cat $archive | wc -c | tr -d ' ')
if [ "$got" != "$whole" ] || [ "$length" != "$size" ]; then
    printf 'the source archive'"'"'s parts are not the archive image-sources.parts describes (sha256 %s, %s bytes; want %s, %s bytes)\n' "$got" "$length" "$whole" "$size" >&2
    exit 2
fi
# shellcheck disable=SC2086
if ! (cd "$out" && cat $archive) | tar -xOf - ./index.tsv >"$scratch/index" ||
    ! cmp -s "$scratch/index" "$out/image-sources-index.tsv"; then
    printf 'image-sources-index.tsv is not the index the source archive carries\n' >&2
    exit 2
fi

awk -F '\t' 'NR > 1 && ($7 == "gnu" || $7 == "unclassified") { print $4 "\t" $5 }' "$out/image-sources.tsv" |
    LC_ALL=C sort -u >"$scratch/needed"
awk -F '\t' 'NR > 1 && $7 == "gnu" { print $4 "\t" $5 }' "$out/image-sources.tsv" | LC_ALL=C sort -u >"$scratch/gnu"
awk -F '\t' 'NR > 1 && $1 != "guest-kernel" && ($3 == "fetched" || $3 == "reviewed") { print $1 "\t" $2 }' \
    "$out/image-sources-index.tsv" | LC_ALL=C sort -u >"$scratch/indexed"
awk -F '\t' 'NR > 1 && $3 == "reviewed" { print $1 "\t" $2 }' "$out/image-sources-index.tsv" | LC_ALL=C sort -u >"$scratch/reviewed"
# THE KERNEL'S SOURCE IS THE KERNEL THE MANIFEST NAMES, once.
kernels=$(awk -F '\t' 'NR > 1 && $1 == "guest-kernel" && $3 == "fetched" { print $2 }' "$out/image-sources-index.tsv" | LC_ALL=C sort -u)
kernel=$(jq -r '.kernel.version // empty' "$out/manifest.json")
# A FAILED comm IS NOT AN EMPTY ONE: its status is checked before its output is
# read, and it collates as the sorts above did.
if ! LC_ALL=C comm -12 "$scratch/reviewed" "$scratch/gnu" >"$scratch/reviewed-gnu"; then
    printf 'could not compare the reviewed sources with the GNU-licensed ones, so nothing is tagged or released\n' >&2
    exit 2
fi
if ! cmp -s "$scratch/needed" "$scratch/indexed" || [ -s "$scratch/reviewed-gnu" ] ||
    [ -z "$kernel" ] || [ "$kernels" != "$kernel" ] || [ ! -s "$scratch/needed" ]; then
    printf 'image-sources-index.tsv does not account for exactly the sources image-sources.tsv needs, and kernel %s'"'"'s\n' "${kernel:-?}" >&2
    exit 2
fi

set -- "$out/manifest.json" "$out/manifest.sigstore.json" "$out/image-sources.tsv" \
    "$out/image-sources-index.tsv" "$out/image-sources.parts"
for part in $archive; do
    set -- "$@" "$out/$part"
done

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

    set -- "$@" "$out/$name"
done <<ASSETS
$assets
ASSETS

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
