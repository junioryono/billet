#!/usr/bin/env bash
#
# Split a packed image into pieces a GitHub release will actually accept.
#
# WHY THIS EXISTS AT ALL. "Each file included in a release must be under 2 GiB",
# and a guest image carrying what a github-hosted runner carries packs to well
# past that. A release holds up to a thousand assets and has no total-size limit,
# so the file is split here and put back together by `billet images pull`.
#
# SPLIT ONLY WHEN IT MUST BE. A file that fits in one asset stays one asset, and
# the manifest that describes it stays schema 1 -- which is what every already
# deployed billet can read. Splitting unconditionally would make the smallest
# image require the newest binary for no reason.
#
# THE PART NAMES SORT IN CONCATENATION ORDER, and that is deliberate rather than
# incidental: `-d -a 3` gives .part000, .part001, and a lexical sort of those is
# the numeric one. The manifest still carries the order explicitly and the reader
# still checks the digest of the reassembled file, because a sort is not a
# contract -- but a listing that happens to be right is one fewer way to be wrong.
set -euo pipefail

FILE="${1:?usage: split-image.sh <file> [part-bytes]}"

# 1900 MiB, NOT 2 GiB. The limit is "under 2 GiB" and a part exactly at the bound
# is the one that fails the upload after an hour-long build. The margin costs an
# extra part on a twenty-gigabyte image and removes the entire class.
PART_BYTES="${2:-1992294400}"

if [ ! -r "$FILE" ]; then
	echo "split-image: cannot read $FILE" >&2
	exit 1
fi

filesize() {
	stat -c %s "$1" 2>/dev/null || stat -f %z "$1"
}

size=$(filesize "$FILE")

if [ "$size" -le "$PART_BYTES" ]; then
	# NOTHING PRINTED, and that is the signal. The manifest writer reads an empty
	# part list as "this is one asset" and emits the schema every deployed reader
	# already understands.
	exit 0
fi

# REMOVED FIRST. A rebuild in the same workspace would otherwise leave parts from
# a larger previous image beside the new ones, and the manifest -- which lists
# what it is told rather than what it globs -- would describe a set that does not
# concatenate into anything.
rm -f "$FILE".part*

split -b "$PART_BYTES" -d -a 3 "$FILE" "$FILE.part"

# PRINTED IN ORDER, one per line, for the manifest writer to consume. Sorted
# explicitly rather than trusting the glob's locale: a collation that ordered
# these differently would produce a manifest whose parts do not join, which the
# reader would catch -- after a full download.
for part in "$FILE".part*; do
	printf '%s\n' "$part"
done | LC_ALL=C sort
