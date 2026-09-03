#!/usr/bin/env bash
#
# Decide whether a built guest kernel is fit to publish.
#
# THE GATE THE BUILD SCRIPT ALWAYS CLAIMED TO HAVE. build-guest-kernel.sh named
# moby's check-config.sh as its Docker compatibility test and then ran it only if
# somebody had happened to put a copy beside the work directory -- which no release
# run ever did, so every published kernel printed "skipped" and shipped. Worse, the
# branch that DID run it discarded the answer:
#
#   (cd "$WORK" && ./check-config.sh cfg | sed ... | grep -Ei missing | head -30) \
#     || echo "NOTHING MISSING"
#
# MEASURED in ubuntu:24.04 against the committed config with CONFIG_VETH=y removed:
# the checker exits 1, that pipeline prints all fifteen "missing" lines AND THEN
# "NOTHING MISSING", and the script continues and exits 0. Under `pipefail` the
# checker's failure becomes the pipeline's status, `|| echo` swallows it, and the
# reassuring sentence is printed by the failure path. So this script captures the
# checker's status BEFORE anything filters its output, and nothing here decides
# from text.
#
# WHAT IT PROVES, IN ORDER:
#
#   1. the pinned checker is present and is the audited copy;
#   2. every option billet requires is built IN, and nothing is a module;
#   3. moby's own checker passes.
#
# The checker is verified before it is used and run last, because running a script
# is the one step that can do something; and billet's own rules come before it so
# they can be exercised anywhere -- the vendored checker needs /proc, GNU sed and
# GNU stat, so a macOS `make check` can reach step 2 and not step 3.
#
# Usage: scripts/check-guest-kernel-config.sh <path to a built .config>
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CONFIG="${1:-}"

if [ -z "$CONFIG" ]; then
	echo "usage: $0 <kernel .config>" >&2
	echo "" >&2
	echo "This is the config the kernel was BUILT with -- \$OUT/vmlinux-billet.config," >&2
	echo "not the base config in the repository, which is only where the build starts." >&2
	exit 1
fi

if [ ! -r "$CONFIG" ]; then
	echo "cannot read the kernel config at $CONFIG." >&2
	exit 1
fi

# OVERRIDABLE WITH COMMITTED DEFAULTS, the shape KERNEL_SHA256 already has in
# build-guest-kernel.sh: an operator pinning a newer moby revision, or a test
# driving this with a stand-in checker, supplies BOTH the file and the digest it
# must have -- so the digest step is exercised rather than skipped.
CHECK_CONFIG_SH="${CHECK_CONFIG_SH:-$REPO_ROOT/scripts/kernel/check-config.sh}"
CHECK_CONFIG_PIN="${CHECK_CONFIG_PIN:-$REPO_ROOT/scripts/kernel/check-config.pin}"
REQUIRED_BUILTINS="${REQUIRED_BUILTINS:-$REPO_ROOT/scripts/kernel/required-builtins.txt}"

# sha256 of a file, printed bare, on either of the two platforms this runs on.
file_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# ---------------------------------------------------------------- the checker

# THE PRIVATE WORK DIRECTORY IS TAKEN FIRST, because the checker is COPIED into it
# before it is hashed. Hashing a path and then executing that path a second later is
# two lookups of a name something else may change in between -- so the digest would
# vouch for bytes other than the ones that ran. One copy, hashed and executed, and
# the two questions are about the same file.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

if [ ! -r "$CHECK_CONFIG_SH" ]; then
	echo "the kernel checker is not at $CHECK_CONFIG_SH, so this cannot say whether the" >&2
	echo "kernel can run docker -- and a kernel nothing checked must not be published." >&2
	echo "" >&2
	echo "It is a vendored copy of moby's contrib/check-config.sh and lives in the" >&2
	echo "repository; restore it with git rather than downloading one, so that what runs" >&2
	echo "is the revision this project audited." >&2
	exit 1
fi

want_sum="${CHECK_CONFIG_SHA256:-}"

if [ -z "$want_sum" ]; then
	if [ ! -r "$CHECK_CONFIG_PIN" ]; then
		echo "cannot read $CHECK_CONFIG_PIN, which names the commit the checker came from" >&2
		echo "and the digest it must have." >&2
		exit 1
	fi

	want_sum=$(awk 'NR==1{print $2}' "$CHECK_CONFIG_PIN")
	pinned_commit=$(awk 'NR==1{print $1}' "$CHECK_CONFIG_PIN")
else
	pinned_commit="(named by CHECK_CONFIG_SHA256)"
fi

if [ -z "$want_sum" ]; then
	echo "$CHECK_CONFIG_PIN names no digest for the checker; its first line is" >&2
	echo "'<upstream commit> <sha256>'." >&2
	exit 1
fi

# COPIED, THEN HASHED, THEN RUN -- all against $checker rather than against the
# path. A source replaced mid-copy is caught too, because what is hashed is what
# landed.
checker="$work/check-config.sh"

if ! cat "$CHECK_CONFIG_SH" >"$checker"; then
	echo "the kernel checker at $CHECK_CONFIG_SH could not be read." >&2
	exit 1
fi

got_sum=$(file_sha256 "$checker")

if [ "$want_sum" != "$got_sum" ]; then
	echo "the kernel checker at $CHECK_CONFIG_SH hashes to $got_sum" >&2
	echo "and its pin names $want_sum." >&2
	echo "" >&2
	echo "This script decides whether a kernel every deployment boots is fit to publish," >&2
	echo "so an unreviewed edit to it must stop the build. Restore the file, or refresh" >&2
	echo "the copy and its pin together." >&2
	exit 1
fi

echo "kernel checker: moby $pinned_commit, digest verified"

# ------------------------------------------------- billet's own, stricter rule

if [ ! -r "$REQUIRED_BUILTINS" ]; then
	echo "cannot read $REQUIRED_BUILTINS, which lists what this kernel must carry." >&2
	exit 1
fi

missing=""
required_count=0

# READ WITH A REDIRECT AND CHECKED WITH A FILE ARGUMENT, never through a pipe.
# `grep -q` on a pipeline returns the WRITER's SIGPIPE under pipefail, which reads
# as "not found" for a value that IS present -- intermittently. Given a filename
# there is no writer to signal.
while IFS= read -r line || [ -n "$line" ]; do
	case "$line" in
	'' | '#'*) continue ;;
	esac

	option=$(printf '%s' "$line" | tr -d '[:space:]')
	[ -n "$option" ] || continue

	required_count=$((required_count + 1))

	# THE SAME THREE-VALUED STATUS, AND HERE THE COLLAPSE IS THE SAFE DIRECTION: a
	# read that could not be performed is counted as "not present", which refuses the
	# kernel and names the option. The module check below cannot do that, because
	# there the answer that costs nothing is the one that publishes.
	if grep -qxF "CONFIG_$option=y" "$CONFIG"; then
		continue
	fi

	missing="$missing $option"
done <"$REQUIRED_BUILTINS"

if [ "$required_count" -eq 0 ]; then
	echo "$REQUIRED_BUILTINS lists no options, so this proved nothing about the kernel." >&2
	exit 1
fi

if [ -n "$missing" ]; then
	echo "" >&2
	echo "these options are required and are not built in:" >&2

	for option in $missing; do
		state=$(grep -E "^(CONFIG_$option=|# CONFIG_$option is not set)" "$CONFIG" || true)
		echo "  CONFIG_$option: ${state:-absent from the config}" >&2
	done

	echo "" >&2
	echo "A microVM has no initramfs, so an option that is not =y is not there at all." >&2
	echo "Docker, the runner or a service container fails in the middle of somebody's" >&2
	echo "job rather than at boot, which is the failure this refuses to publish." >&2
	exit 1
fi

# A MODULE IS THE SAME AS ABSENT, so the config may not contain one anywhere --
# which is also how billet stays stricter than moby's checker without keeping a
# second copy of moby's option list: check-config.sh accepts =m for every flag it
# grades, and this refuses =m for all of them at once.
#
# grep's STATUS IS THREE-VALUED -- 0 found, 1 not found, anything above 1 COULD NOT
# LOOK -- and folding the third into "not found" is the could-not-tell/no collapse
# this repository has removed from the credential paths and from the toolcache
# gates. Here it would accept a config it failed to read as one carrying no modules,
# and moby's checker does not restore the verdict, because it accepts =m.
set +e
grep -qE '^CONFIG_[A-Za-z0-9_]+=m$' "$CONFIG"
modules=$?
set -e

if [ "$modules" -gt 1 ]; then
	echo "could not read $CONFIG to look for modules (grep exited $modules)." >&2
	echo "This decides whether a kernel every deployment boots is fit to publish, so" >&2
	echo "a read it could not perform is a refusal rather than an answer." >&2
	exit 1
fi

if [ "$modules" -eq 0 ]; then
	echo "" >&2
	echo "this config builds these as modules:" >&2

	# ONE awk, FOR THE REASON THE PASS BRANCH GIVES, and this branch had the same
	# defect: MEASURED, `grep -E ... file | head -20` on a 3.2MB config exits **141**
	# under `set -euo pipefail`, so the refusal below -- the sentence that says WHY --
	# was never printed and the gate died on its own diagnostic. The verdict was still
	# a refusal, which is why it took a second look to see.
	awk '
		/^CONFIG_[A-Za-z0-9_]+=m$/ { n++; if (n <= 20) print }
		END { if (n > 20) printf "... and %d more\n", n - 20 }
	' "$CONFIG" >&2

	echo "" >&2
	echo "The guest boots one kernel with no initramfs, so nothing can load them." >&2
	exit 1
fi

echo "required built-ins: $required_count present, no modules"

# ---------------------------------------------------- and then moby's checker

# THE STATUS IS CAPTURED BEFORE ANYTHING READS THE OUTPUT. `set +e` around the run
# rather than `|| status=$?`, because the latter is an AND-OR list whose failure
# would be inherited by nothing and whose output would still have to be held.
#
# $checker IS THE VERIFIED COPY, not the path the digest was taken from.
out="$work/check-config.out"

set +e
NO_COLOR=1 sh "$checker" "$CONFIG" >"$out" 2>&1
status=$?
set -e

if [ "$status" -ne 0 ]; then
	echo "" >&2
	echo "moby's check-config.sh exited $status against $CONFIG." >&2
	echo "" >&2
	cat "$out" >&2
	echo "" >&2
	echo "Its status is not only about the config: read at the pinned revision it also" >&2
	echo "fails for a cgroup v1 hierarchy that is not properly mounted, for apparmor" >&2
	echo "enabled without apparmor_parser, and for kernel.keys.root_maxkeys <= 10000 --" >&2
	echo "all facts about THIS MACHINE rather than about the kernel being published." >&2
	echo "The 'Generally Necessary' section above is the part that is about the kernel." >&2
	exit 1
fi

# PRINTED FOR A PERSON, AFTER THE DECISION IS ALREADY MADE. Every "missing" line
# here is from an OPTIONAL section -- the checker exited 0 -- and on a passing
# kernel there are about a dozen of them, which is exactly why reading this text
# was never a way to decide anything.
#
# ONE awk RATHER THAN `grep | head`, and this is the trap this whole file exists
# about, one line from the end. MEASURED in ubuntu:24.04 under `set -euo pipefail`:
# `grep -Fi missing <2.9MB file> | head -30` exits **141** -- head leaves after
# thirty lines, grep takes SIGPIPE, pipefail reports the writer's status and set -e
# ends the script. A PASSING kernel would fail the gate over its own DIAGNOSTIC,
# depending on how much output the checker happened to produce. awk reads to the end
# and has nothing to signal; it also takes a filename, so there is no writer either.
awk '
	BEGIN { n = 0 }
	tolower($0) ~ /missing/ {
		n++
		if (n <= 30) lines[n] = $0
	}
	END {
		if (n == 0) {
			print "check-config.sh passed with nothing missing at all"
			exit
		}

		if (n > 30) {
			printf "check-config.sh passed; %d optional features it reports missing (30 shown):\n", n
		} else {
			print "check-config.sh passed; optional features it reports missing:"
		}

		for (i = 1; i <= n && i <= 30; i++) print lines[i]
	}' "$out"

echo "=== KERNEL CONFIG OK ==="
