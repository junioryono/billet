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

# THE TAG POINTS AT THE RELEASE COMMIT THE IMAGE WAS BUILT FROM, never at the
# main commit that published it: the manifest's schema is closed, so the tag's
# target is the provenance record.
#
# THE TAG IS CREATED BEFORE THE RELEASE. --target affects only a missing tag, so
# handing an existing attacker-chosen tag to gh release create would publish the
# checked assets under a different commit. Both local and remote conflicts fail;
# neither command is allowed to rewrite a ref.
git tag "$TAG" "$SOURCE_SHA"
git push origin "refs/tags/$TAG"

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
assets=$(jq -r '
    [.kernel.name]
    + (if .rootfs_multipart then [.rootfs_multipart.parts[].name] else [.rootfs.name] end)
    | .[]
' "$out/manifest.json")

if [ -z "$assets" ]; then
    printf 'the manifest names no assets to publish\n' >&2
    exit 2
fi

set -- "$out/manifest.json" "$out/manifest.sigstore.json"

for name in $assets; do
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

    set -- "$@" "$out/$name"
done

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
