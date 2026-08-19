#!/bin/sh
set -eu

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"
: "${TAG:?TAG is required}"

out=${1:-out}
if ! printf '%s\n' "$TAG" | grep -Eq '^guest-[0-9]{8}-[0-9]{6}$'; then
    printf 'invalid guest release tag: %s\n' "$TAG" >&2
    exit 2
fi
case "$GITHUB_SHA" in
    ''|*[!0-9a-f]*) printf 'invalid publication commit: %s\n' "$GITHUB_SHA" >&2; exit 2 ;;
esac
case "${#GITHUB_SHA}" in
    40|64) ;;
    *) printf 'invalid publication commit length: %s\n' "$GITHUB_SHA" >&2; exit 2 ;;
esac

# THE TAG IS CREATED BEFORE THE RELEASE. --target affects only a missing tag, so
# handing an existing attacker-chosen tag to gh release create would publish the
# checked assets under a different commit. Both local and remote conflicts fail;
# neither command is allowed to rewrite a ref.
git tag "$TAG" "$GITHUB_SHA"
git push origin "refs/tags/$TAG"

gh release create "$TAG" \
    --repo "$GITHUB_REPOSITORY" \
    --verify-tag \
    --prerelease \
    --title "Guest image $TAG" \
    --notes-file "$out/release-notes.md" \
    "$out/manifest.json" "$out/manifest.sigstore.json" \
    "$out/rootfs.img.zst" "$out/vmlinux-billet"

immutable=$(gh release view "$TAG" \
    --repo "$GITHUB_REPOSITORY" --json isImmutable --jq .isImmutable)
if [ "$immutable" != true ]; then
    printf 'published guest release is not immutable\n' >&2
    exit 1
fi
