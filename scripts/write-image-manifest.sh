#!/usr/bin/env bash
#
# Describe a built guest image, on stdout, as the manifest a node will read.
#
# WHY THIS IS A SEPARATE SCRIPT. The build produces bytes; this says what they
# are. Keeping them apart means the description is derived from the artifacts
# that actually exist — their real sizes, their real digests — rather than from
# the inputs somebody hoped produced them. A publisher that describes an image
# from its own environment reports the version it REQUESTED, and that differs
# from the version it INSTALLED exactly when the request was blank, which is the
# normal scheduled case.
#
# EVERY FACT COMES FROM AN ARTIFACT OR FROM build-info.env, which the build wrote
# after it finished. Nothing here restates a constant.
set -euo pipefail

REPO="${REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
WORK="${WORK:-/var/tmp/billet-guest}"
INFO="${INFO:-$WORK/build-info.env}"

# The packed artifacts, as they will be uploaded.
ROOTFS="${ROOTFS:?ROOTFS must name the packed root filesystem}"
KERNEL="${KERNEL:?KERNEL must name the guest kernel}"

# SCHEMA IS THE ONE CONSTANT HERE, and it belongs to the document rather than to
# the image. It is checked against the Go reader by a test.
SCHEMA="${SCHEMA:-1}"

die() {
	echo "write-image-manifest: $*" >&2
	exit 1
}

[ -r "$INFO" ] || die "cannot read $INFO; run build-guest-image.sh first (it writes this
after the filesystem is built, recording what it actually installed)"

# SOURCED, NOT PARSED BY HAND. The file is written by the build a few lines after
# it finishes, in this repository, with values that cannot contain a newline --
# so `source` is safe here in a way it would not be for a file arriving from
# anywhere else.
# shellcheck disable=SC1090
. "$INFO"

[ -n "${RUNNER_VERSION:-}" ] || die "$INFO names no runner version"
[ -n "${GUEST_CONTRACT:-}" ] || die "$INFO names no guest contract"
[ -n "${ARCH:-}" ] || die "$INFO names no architecture"

[ -r "$ROOTFS" ] || die "cannot read the packed root filesystem at $ROOTFS"
[ -r "$KERNEL" ] || die "cannot read the guest kernel at $KERNEL"

# THE KERNEL NAMES ITSELF. Reading the version out of the built binary rather
# than out of the script that built it means a cached kernel -- which is the
# normal case, since the cache key is its config -- is described by what it IS
# rather than by what the current checkout would build. Those differ the moment
# the pinned version changes and the cache has not been invalidated, and the
# manifest would otherwise claim a kernel that was never in it.
kernel_version=$(strings -a "$KERNEL" |
	sed -n 's/^Linux version \([0-9][0-9.]*[0-9]\).*/\1/p' | head -1)

[ -n "$kernel_version" ] || die "could not read a version out of $KERNEL; refusing to
describe a kernel this cannot identify"

digest() { sha256sum "$1" | cut -d' ' -f1; }

# `stat -c` ON GNU, `stat -f` ON BSD. This runs on a hosted runner (GNU) and on a
# developer's machine (which may not be), and a silent empty size would become a
# manifest whose size field is zero -- refused by the reader, but with a message
# blaming the publisher for something this script did.
filesize() {
	stat -c %s "$1" 2>/dev/null || stat -f %z "$1"
}

rootfs_name=$(basename "$ROOTFS")
kernel_name=$(basename "$KERNEL")

# COMPRESSION IS READ FROM THE NAME, because that is what determines how the
# reader must unpack it, and the reader refuses a value it does not know rather
# than treating it as raw bytes.
case "$rootfs_name" in
	*.zst) rootfs_compression="zstd" ;;
	*) rootfs_compression="" ;;
esac

# THE PARTS, IF THE PACK STEP HAD TO MAKE ANY.
#
# ROOTFS_PARTS is a newline-separated list of paths in concatenation order, as
# split-image.sh printed them. Empty means the image fits in one release asset,
# which is the ordinary case for anything but a full-parity image.
#
# THE SCHEMA FOLLOWS THE ARTIFACT RATHER THAN A FLAG. A one-asset image is
# described as schema 1, which every already deployed billet can read; only an
# image that genuinely had to be split is described as schema 2, and reading that
# needs a binary new enough to join it. Tying the layout to a switch instead would
# mean the smallest image could still be published in a form the fleet cannot
# read.
#
# THE WHOLE-FILE DIGEST IS OF $ROOTFS, WHICH STILL EXISTS. It is the check a
# reader uses to prove the parts were joined back into this exact file, so it must
# be taken from the unsplit artifact and never derived from the pieces.
rootfs_parts_json="[]"
part_count=0

if [ -n "${ROOTFS_PARTS:-}" ]; then
	while IFS= read -r part; do
		[ -n "$part" ] || continue
		[ -r "$part" ] || die "ROOTFS_PARTS names $part, which cannot be read"

		part_count=$((part_count + 1))

		rootfs_parts_json=$(jq -c \
			--arg name "$(basename "$part")" \
			--arg sha "$(digest "$part")" \
			--argjson size "$(filesize "$part")" \
			'. + [{name: $name, sha256: $sha, size: $size}]' <<<"$rootfs_parts_json")
	done <<<"$ROOTFS_PARTS"
fi

if [ "$part_count" -gt 0 ]; then
	SCHEMA=2
fi

# BUILT BY jq, NOT BY printf. Every value here is interpolated into JSON, and a
# hand-built document is one unescaped character away from being invalid -- or,
# worse, valid and wrong. jq --arg escapes each one.
jq -n \
	--argjson schema "$SCHEMA" \
	--arg contract "$GUEST_CONTRACT" \
	--arg arch "$ARCH" \
	--arg runner "$RUNNER_VERSION" \
	--arg built_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
	--arg rootfs_name "$rootfs_name" \
	--arg rootfs_sha "$(digest "$ROOTFS")" \
	--argjson rootfs_size "$(filesize "$ROOTFS")" \
	--arg rootfs_compression "$rootfs_compression" \
	--argjson rootfs_parts "$rootfs_parts_json" \
	--arg kernel_name "$kernel_name" \
	--arg kernel_sha "$(digest "$KERNEL")" \
	--argjson kernel_size "$(filesize "$KERNEL")" \
	--arg kernel_version "$kernel_version" \
	'{
		schema: $schema,
		guest_contract: $contract,
		arch: $arch,
		runner_version: $runner,
		built_at: $built_at,
	}
	+ (if ($rootfs_parts | length) == 0 then {
		rootfs: ({
			name: $rootfs_name,
			sha256: $rootfs_sha,
			size: $rootfs_size,
		} + (if $rootfs_compression == "" then {} else {compression: $rootfs_compression} end)),
	} else {
		rootfs_multipart: ({
			name: $rootfs_name,
			sha256: $rootfs_sha,
			size: $rootfs_size,
		} + (if $rootfs_compression == "" then {} else {compression: $rootfs_compression} end)
		  + {parts: $rootfs_parts}),
	} end)
	+ {
		kernel: {
			name: $kernel_name,
			sha256: $kernel_sha,
			size: $kernel_size,
			version: $kernel_version,
		},
	}'
