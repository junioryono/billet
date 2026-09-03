#!/usr/bin/env bash
#
# Build the guest image a Firecracker microVM boots, and publish it to Ceph.
#
# A SCRIPT RATHER THAN A `billet` SUBCOMMAND, unlike `billet ami`. That one has to
# drive an API — launch a builder instance, wait, snapshot it — which is a program.
# This one runs debootstrap, chroot and apt on the machine it is already on, and a Go
# program wrapping those is a worse version of a shell script. `billet check` proves
# the result; nothing about building it needs to be in the binary.
#
# WHAT THE GUEST MUST DO, which is what every step below is for:
#
#   1. Boot from /dev/vda, which is a clone of this image.
#   2. Bring up eth0 and reach the metadata service at 169.254.169.254.
#   3. Read the runner registration from it — MMDS V2, so a session token first.
#   4. Export it as ACTIONS_RUNNER_INPUT_JITCONFIG and exec the tier's command.
#   5. Have Docker working, because that is most of what a CI job does.
#
# It is deliberately not idempotent about the pool: publishing makes a NEW snapshot
# rather than moving an existing one, because a generation a running job holds a clone
# of must not change underneath it.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)

# ONE PIN FOR EVERY IMAGE BILLET BUILDS, read from the file the Go code embeds.
# It was a shell default here and a constant in `billet ami`, so bumping the runner
# was two edits in two languages -- and doing one of them leaves a fleet where one
# backend is current and the other is not, found on the day GitHub stops queueing to
# the stale half.
PINNED_RUNNER_FILE="$SCRIPT_DIR/../internal/runnerrelease/pinned.txt"

# GITHUB'S OWN DECLARATION OF WHAT A RUNNER IMAGE CONTAINS, read by BOTH backends.
#
# GitHub does not publish the image its hosted runners boot -- runner-images is
# Packer source whose only builder targets Azure, and every release there carries
# one ~50KB JSON file. What it DOES publish is this: every apt package, toolcache
# entry, JDK, NDK and pinned tool version, machine-readable. So parity is a
# rebuild from a declaration rather than a copy of an artifact.
#
# READ HERE AND IN internal/runnerimages, FROM THE SAME FILE. Two hand-maintained
# package lists in two languages is the shape of the bug runnerrelease exists to
# prevent: a package added for one backend silently missing from the other, found
# on the day a workflow needs it.
TOOLSET_FILE="$SCRIPT_DIR/../internal/runnerimages/toolset-2404.json"
TOOLSET_PIN="$SCRIPT_DIR/../internal/runnerimages/pinned.txt"

# THE NARROW MAPPING FROM A DECLARED NAME TO ONE APT CAN INSTALL.
#
# Read here and by check-guest-image.sh from the same file, so a mapped package is
# still verified as present rather than quietly excused from the check.
APT_ALIASES="${APT_ALIASES:-$SCRIPT_DIR/../internal/runnerimages/apt-aliases.json}"

# TOOLCACHE_DIR IS NOT A CHOICE.
#
# The python builds from actions/python-versions are configured with
# `--enable-shared` and an RPATH pointing at this exact path, and their console
# scripts carry it as a hardcoded shebang. They are NOT relocatable: extracted
# anywhere else, `bin/python3` still loads its libraries from here, and every
# entry point in bin/ points at a path that does not exist. setup-python papers
# over half of that at runtime by exporting LD_LIBRARY_PATH and leaves the
# shebangs broken.
#
# It is also what github's own image uses, which is why every published tarball
# assumes it.
TOOLCACHE_DIR=/opt/hostedtoolcache

# THE TOOLCACHE INSTALLERS LIVE BESIDE THE DECLARATION THEY READ, and the EC2
# backend runs the same file. Sourced rather than executed: they are bash
# functions, and a function definition does not cross a process boundary.
#
# SOURCED HERE, BEFORE main, because toolset_versions and read_toolset_versions
# moved with them and this script's own callers expect them defined.
# shellcheck source=../internal/runnerimages/install-toolcache.sh
. "$SCRIPT_DIR/../internal/runnerimages/install-toolcache.sh"

if [ -z "${RUNNER_VERSION:-}" ] && [ ! -r "$PINNED_RUNNER_FILE" ]; then
	echo "cannot read the pinned runner version at $PINNED_RUNNER_FILE, and RUNNER_VERSION" >&2
	echo "was not set; run this from a checkout, or say which release to install" >&2
	exit 1
fi

RUNNER_VERSION="${RUNNER_VERSION:-$(awk "NR==1{print \$1}" "$PINNED_RUNNER_FILE")}"
SUITE="${SUITE:-noble}"
# 22528MB AGAINST 15392MB MEASURED, leaving about 5481MB free.
#
# MEASURED BY THE BUILD ITSELF, off the mounted filesystem after the apt set, the
# toolcache, the JDKs and the toolchains: `contents: 15392M used, 22559M free of
# 40960M`, read out of a build log rather than derived. That run was given a
# deliberately oversized filesystem precisely so it would COMPLETE and print this
# line; the size here is fitted to what it reported.
#
# THE DECLARED SIZE IS NOT THE USABLE SIZE, and that is the trap in reading that
# line. 15392 + 22559 is 37951, not 40960: ext4's journal, inode tables and the 5%
# reserved-blocks default take 7.3% off the top. So usable space is about 0.927 of
# whatever is declared here, and a margin computed by subtracting content from the
# declaration overstates itself by about two gigabytes. The first draft of this
# revision did exactly that -- claimed 5088M of headroom for a 20480M image that
# actually leaves 3584M. At 22528 the usable space is about 20873M, so the real
# margin is 5481M against a MIN_FREE_MB of 512.
#
# THE HISTORY IS 4096 -> 8192 -> 12288 -> 22528, AND EVERY STEP WAS MEASURED AND
# THEN OUTGROWN. 4096 held 2.6GB. 8192 held 6521MB with a 2.7G toolcache of node,
# go, Python and Java, and was right until PyPy, Ruby and CodeQL took the toolcache
# to 5.2G -- a build then died extracting the codeql bundle with 880M free. 12288
# held 9009MB and was right until the toolchains landed: .NET, PowerShell and its
# modules, and the compilers. A build died again, unpacking .NET:
#
#	tar: ./packs/Microsoft.NETCore.App.Host.linux-x64/10.0.11: Cannot mkdir: No space left on device
#
# THE GUARD BELOW CANNOT CATCH THAT, WHICH IS WORTH KNOWING BEFORE TRUSTING IT. It
# compares free space AFTER the installs, so a build that overflows DURING one never
# reaches it and fails with a tar error naming a package instead -- which is exactly
# what the MIN_FREE_MB comment predicts will happen. It still earns its place for
# the case it does catch, a build that finishes with no margin left, and it is what
# produced the measurement above. It is not a substitute for this number being right.
#
# AN ESTIMATE THAT LANDS IS STILL AN ESTIMATE. The arithmetic before the 12288
# measurement said 9.1GB and was right; the one before that said 5.3GB and was wrong
# by a gigabyte, about 20%, in this same file for the same reason. That is the
# argument for reading the build's own report rather than for trusting a prediction
# that happened to work.
#
# WHY THE GUEST IS SMALLER THAN THE AMI. The EC2 image measures 26.8GiB on x64. The
# difference is the Android SDK and its three NDKs, which this image deliberately
# does not carry: BILLET_TC_ANDROID_ACCEPT_LICENSES is set by the AMI build and not
# by this one, because building in the operator's own account is use and publishing
# this image as a release asset is redistribution.
#
# OVER-SIZING IS CHEAP AND UNDER-SIZING IS NOT, which is why this is rounded up
# rather than fitted to the nearest block. The file is sparse, ext4 allocates only
# what is used, zstd compresses the unused remainder to nearly nothing, and RBD is
# thin-provisioned -- so unused space in the head costs approximately nothing at
# every stage, while a number too small fails an hour-long build at its last step.
# The asymmetry is entirely one way.
#
# It is still not free forever: every generation already published keeps its own
# size, so this should track the measurement rather than drift upward by habit.
SIZE_MB="${SIZE_MB:-22528}"

# MIN_FREE_MB is the build's own margin, checked against a MEASUREMENT.
#
# `SIZE_MB` is not derivable from anything in this script -- it depends on what
# the install steps actually put on disk, which is a fact about a build rather
# than about a configuration. So the build measures what it used, reports it, and
# refuses if the margin is gone, naming the number to raise SIZE_MB to. That is
# what keeps this file's constant a recorded measurement instead of a guess that
# happened to work, which is the rule CLAUDE.md states about byte sizes.
MIN_FREE_MB="${MIN_FREE_MB:-512}"
IMAGE_POOL="${IMAGE_POOL:-billet-images}"
IMAGE_NAME="${IMAGE_NAME:-ubuntu-2404-x64}"
CEPH_USER="${CEPH_USER:-billet}"
WORK_DEFAULT=/var/tmp/billet-guest
WORK="${WORK:-$WORK_DEFAULT}"
PUBLISH="${PUBLISH:-yes}"

# PINNED, NOT "LATEST", for the reason cmd/billet/ami.go gives about the AMI: an
# image is a thing you reproduce, and a build that silently tracked the newest
# release would make two runs of the same command produce different images — a
# difference that surfaces as a job failing on one generation and not another.
#
# THE CHECKSUM COMES FROM THE SAME LINE AS THE VERSION, because it is only true of
# that version. Held apart, a bump updates one of them: either the build fails its
# own integrity check, or -- worse -- the checksum is updated alone and the download
# is verified against a number belonging to a different release.
RUNNER_SHA256="${RUNNER_SHA256:-$(awk "NR==1{print \$2}" "$PINNED_RUNNER_FILE")}"

if [ -z "$RUNNER_SHA256" ]; then
	echo "no checksum for runner $RUNNER_VERSION in $PINNED_RUNNER_FILE; the format is" >&2
	echo "one line: '<version> <sha256 of the linux-x64 tarball>'" >&2
	exit 1
fi

# read_guest_contract reads the protocol version out of the agent that was
# installed, and refuses rather than returning an empty one.
#
# OUT OF THE INSTALLED AGENT, NOT RESTATED HERE. The agent is embedded in a QUOTED
# heredoc -- deliberately, so nothing in it is interpolated -- which means its
# `WANT_CONTRACT=` is a literal this script cannot otherwise see. A second copy
# here would drift silently, and the drift is invisible in the worst way: the
# manifest would advertise a contract the image does not speak, a node would
# accept the image on that basis, and its guests would boot and never report.
#
# A FUNCTION BECAUSE THE ORDERING IS THE BUG IT KEEPS HAVING. It reads through the
# mountpoint, so it must run before the unmount; inlined at the end of the build
# it silently became a read of an empty host directory when the filesystem moved
# to the start. As a function it can be driven by a test against a fixture.
read_guest_contract() {
	local rootfs="$1"
	local agent="$rootfs/usr/local/bin/billet-agent"

	if [ ! -r "$agent" ]; then
		echo "no agent at $agent to read a guest contract from. If the image was already" >&2
		echo "unmounted, this ran too late: everything describing the image reads files" >&2
		echo "inside it and must happen before unmount_rootfs." >&2
		return 1
	fi

	local contract
	contract=$(sed -n 's/^WANT_CONTRACT=\([0-9][0-9]*\)$/\1/p' "$agent" | head -1)

	if [ -z "$contract" ]; then
		echo "could not read the guest contract out of the agent that was just installed;" >&2
		echo "refusing to describe an image whose protocol version is unknown" >&2
		return 1
	fi

	printf '%s\n' "$contract"
}

# verify_toolset proves the vendored declaration is the file that was pinned.
#
# CHECKED HERE TOO, NOT ONLY IN GO. internal/runnerimages verifies it on the path
# that reads it, and this script reads the same file through jq -- so a check that
# lived only on the Go side would leave the thing that actually BUILDS THE IMAGE
# trusting whatever is on disk. This file decides what goes into an image that
# runs other people's CI, so an edit to it without an edit to the pin has to stop
# the build rather than quietly change every image made afterwards.
verify_toolset() {
	[ -r "$TOOLSET_FILE" ] || {
		echo "cannot read the vendored toolset at $TOOLSET_FILE; run this from a checkout" >&2
		exit 1
	}

	[ -r "$TOOLSET_PIN" ] || {
		echo "cannot read $TOOLSET_PIN, which names the digest the toolset must have" >&2
		exit 1
	}

	local want got
	want=$(awk 'NR==1{print $2}' "$TOOLSET_PIN")
	got=$(sha256sum "$TOOLSET_FILE" | cut -d' ' -f1)

	if [ "$want" != "$got" ]; then
		echo "the vendored toolset hashes to $got and pinned.txt names $want." >&2
		echo "Refresh both together, or restore the file. This declares what every image" >&2
		echo "built from here contains, so an unreviewed edit is an unreviewed image." >&2
		exit 1
	fi
}

# toolset_packages prints every apt package GitHub's image installs, in its order.
#
# VITAL, THEN COMMON, THEN CMD, deduplicated. That is the order upstream installs
# them in, and it decides which package resolves a shared dependency first.

# NOT `unique`, WHICH SORTS. jq's unique returns a sorted array, which would
# reorder the three groups into one alphabetical list and lose exactly the
# property this function documents. The reduce below drops repeats while keeping
# first-seen order.
#
# THE ALIAS MAP IS APPLIED HERE, so every consumer of this function installs a
# name apt can resolve. `netcat` is a pure virtual package on noble with two
# providers, which apt refuses to choose between -- it took down the first real
# end-to-end build at stage 2, after debootstrap and before anything else ran.
toolset_packages() {
	jq -er --slurpfile aliases "$APT_ALIASES" '
		($aliases[0] // {}) as $alias
		| [.apt.vital_packages[], .apt.common_packages[], .apt.cmd_packages[],
			(.clang.versions[]? | select(. != null and . != "") | "clang-" + .),
			(.gcc.versions[]?), (.gfortran.versions[]?),
			(.php.versions[]? | select(. != null and . != "") | "php" + . + "-cli"),
			(.postgresql.version | select(. != null and . != "") | "postgresql-client-" + .),
			(if (.pipx | length) > 0 then "pipx" else empty end)]
		| map(select(. != null and . != ""))
		| map(
			$alias[.] as $entry
			| if $entry == null then .
			  # AN ALIAS THAT MAPS TO NOTHING IS AN ERROR, NOT A PASSTHROUGH.
			  # An empty install used to read as "no alias" and produce a BLANK
			  # line, which the caller then filtered out -- silently dropping a
			  # DECLARED package from the install list and from the expected set
			  # the gate checks, so nothing anywhere reported it missing.
			  #
			  # NOTE: no apostrophes in this block. The jq program is a
			  # single-quoted shell string, and one would end it -- which is
			  # exactly how the first version of this comment broke the script.
			  elif ($entry | type) != "object" or ($entry.install // "") == "" then
				error("apt-aliases.json maps \(.) to nothing usable; an entry needs a non-empty install")
			  else $entry.install
			  end)
		| reduce .[] as $p ([]; if index($p) then . else . + [$p] end)
		| .[]
	' "$TOOLSET_FILE"
}

# MOUNTED_ROOTFS is the mountpoint this build is currently writing through, or
# empty. The trap reads it, so it must be set BEFORE the mount and cleared AFTER
# the unmount rather than around them.
MOUNTED_ROOTFS=""

# MOUNTED_PROC is the /proc mounted INSIDE the rootfs, or empty.
#
# A CHROOT WITHOUT /proc CANNOT RUN EVERY RUNTIME THE TOOLCACHE INSTALLS, and two
# of them proved it in the same build. pypy's `pypy3` is a launcher whose library
# sits beside it, found through a DT_RPATH of `$ORIGIN/`; the codeql bundle ships
# its own JVM whose `java` finds libjli.so through `$ORIGIN/../lib`. Both failed
# with "cannot open shared object file" for files present at full size in exactly
# those directories, and pypy said why in passing -- it warned that it could not
# read /proc/cpuinfo. glibc resolves $ORIGIN for a main executable by reading
# /proc/self/exe, which is not there to read.
#
# NESTED, SO IT UNMOUNTS FIRST. A mount inside the rootfs blocks the rootfs
# unmount, and the rootfs unmount is what stops the next run recursively deleting
# a live filesystem -- so this is torn down ahead of it rather than beside it.
MOUNTED_PROC=""

# unmount_guest_proc drops the /proc mounted inside the rootfs.
unmount_guest_proc() {
	[ -n "$MOUNTED_PROC" ] || return 0

	if mountpoint -q "$MOUNTED_PROC" 2>/dev/null; then
		umount "$MOUNTED_PROC" || umount -l "$MOUNTED_PROC" || true
	fi

	MOUNTED_PROC=""
}

# unmount_rootfs is the trap, and it is installed before anything is mounted.
#
# A LEFTOVER MOUNT IS NOT A COSMETIC PROBLEM HERE. The build writes THROUGH the
# mountpoint, and the next run's first act is `rm -rf "$WORK"` -- so a build that
# died between mount and unmount leaves the next one recursively deleting the
# CONTENTS OF A LIVE FILESYSTEM rather than the directory it thinks it is
# clearing. That is the workspace guard being satisfied by exactly the state it
# cannot protect against.
unmount_rootfs() {
	[ -n "$MOUNTED_ROOTFS" ] || return 0

	# SYNCED FIRST. The image file is what gets packed and published, and an
	# unmount that reports success after a lazy detach can leave data unwritten.
	sync

	if mountpoint -q "$MOUNTED_ROOTFS" 2>/dev/null; then
		umount "$MOUNTED_ROOTFS" || umount -l "$MOUNTED_ROOTFS" || true
	fi

	MOUNTED_ROOTFS=""
}

# THE SIGNAL TRAPS RE-RAISE; THE EXIT TRAP DOES NOT.
#
# A bash signal trap does NOT terminate the shell. Installing unmount_rootfs
# directly on INT and TERM meant a signal arriving while the build waited on a
# child would unmount the filesystem, clear MOUNTED_ROOTFS, and then RESUME the
# build -- writing every subsequent step onto the host directory the image used
# to cover, and packing an image that stops at whatever was installed when the
# signal landed. Cleaning up, restoring the default handler and re-raising is
# what makes the shell actually die from the signal it was sent.
on_signal() {
	local signal="$1"

	unmount_guest_proc
	unmount_rootfs

	trap - "$signal"
	kill -s "$signal" $$
}

# THE ORDER IS IN THE TRAP, NOT INSIDE EITHER FUNCTION. A mount nested in the
# rootfs blocks the rootfs unmount, so /proc has to go first -- and writing that
# as a call from unmount_rootfs made the two inseparable, which broke the test
# that exercises the rootfs unmount on its own. Each function drops one mount;
# the sequence they must run in is stated once, here, where both are in view.
trap 'unmount_guest_proc; unmount_rootfs' EXIT
trap 'on_signal INT' INT
trap 'on_signal TERM' TERM

# clear_stale_mount refuses to delete a workspace with a live mount inside it.
#
# UNMOUNTED, NOT DELETED THROUGH. If an earlier run left the filesystem mounted,
# unmount it here; if that cannot be done, stop rather than continue -- because
# the next statement is a recursive delete and the operator can fix a mount they
# are told about.
clear_stale_mount() {
	local dir="$1"

	mountpoint -q "$dir" 2>/dev/null || return 0

	echo "an earlier build left $dir mounted; unmounting it before clearing the workspace" >&2

	if ! umount "$dir" && ! umount -l "$dir"; then
		echo "could not unmount $dir. Refusing to continue: the next step deletes this" >&2
		echo "directory recursively, and doing that through a live mount would erase the" >&2
		echo "filesystem it points at rather than the workspace." >&2
		exit 1
	fi
}

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "this builds a filesystem and maps a block device; run it as root" >&2
		exit 1
	fi
}

need_tools() {
	local missing=()
	# flock IS REQUIRED, NOT OPTIONAL. Without it two builds share a workspace,
	# and the first thing each does is unmount and recursively delete it.
	# mountpoint likewise: the stale-mount guard is what stops that delete from
	# running through a live filesystem, and a guard whose tool is missing is not
	# a guard.
	for t in debootstrap mkfs.ext4 chroot jq flock mountpoint; do
		command -v "$t" >/dev/null 2>&1 || missing+=("$t")
	done

	# rbd IS REQUIRED ONLY WHEN THIS PUBLISHES.
	#
	# PUBLISH=no builds the filesystem and stops, which is exactly what a CI runner
	# does -- it has no Ceph cluster to publish into and no reason to install a
	# client for one. Requiring it unconditionally made the hosted build fail after
	# the kernel had already been built, on a tool it was never going to use, with a
	# message telling the operator to install ceph-common on a machine that has no
	# cluster.
	if [ "$PUBLISH" = "yes" ]; then
		command -v rbd >/dev/null 2>&1 || missing+=("rbd")
	fi

	if [ ${#missing[@]} -ne 0 ]; then
		echo "missing: ${missing[*]} (apt-get install debootstrap e2fsprogs ceph-common)" >&2
		exit 1
	fi

	# THE GUEST'S ARCHITECTURE IS THE HOST'S, AND THE RUNNER IS PINNED TO x64.
	# debootstrap takes the host architecture unless told otherwise, so on an arm64
	# machine this would build an arm64 userspace and drop an x86-64 runner into it.
	# That combination boots perfectly and fails at the moment the agent execs the
	# runner, with an executable-format error inside a guest nobody has a console for
	# -- the exact shape of failure this whole image is built to make impossible.
	local arch
	arch=$(uname -m)

	if [ "$arch" != "x86_64" ]; then
		echo "this builds an x86-64 guest (the pinned runner is linux-x64) and this host is" >&2
		echo "$arch; build it on an x86-64 machine, or teach this script to select the" >&2
		echo "runner and debootstrap architecture together" >&2
		exit 1
	fi
}


main() {
	need_root
	need_tools

	# BEFORE ANYTHING IS BUILT, because this file decides what goes in.
	verify_toolset

	local rootfs="$WORK/rootfs" img="$WORK/$IMAGE_NAME.ext4"

	# THE WORKSPACE IS PROVED TO BE A WORKSPACE BEFORE IT IS DELETED. This runs as
	# root and WORK is an environment override, so `WORK=/var/lib/billet` -- or an
	# empty WORK, which makes this `rm -rf /rootfs` -- destroys unrelated state before
	# the build has done anything. A build script is not worth a recursive delete of
	# a directory nobody checked.
	#
	# The marker is what makes it safe on the SECOND run: a directory this script
	# created carries it, and anything else does not, so a typo names a directory
	# without one and stops here rather than being wiped.
	case "$WORK" in
		/*/*) ;;
		*)
			echo "WORK must be an absolute path at least two levels deep, not '$WORK'" >&2
			exit 1
			;;
	esac

	# THE DEFAULT PATH IS OURS BY CONSTRUCTION and needs no marker: it is this
	# script's own directory, named right here, and requiring proof of ownership for
	# it would mean every first run after this check was added fails on a workspace
	# an earlier run left behind. The check is about an OVERRIDE, which is where a
	# typo can name something that matters.
	if [ "$WORK" != "$WORK_DEFAULT" ] &&
		[ -e "$WORK" ] && [ ! -e "$WORK/.billet-guest-workspace" ]; then
		echo "WORK=$WORK exists and was not created by this script; refusing to delete it." >&2
		echo "Remove it yourself, or point WORK at a path this script may own." >&2
		exit 1
	fi

	# ONE BUILD PER WORKSPACE, TAKEN BEFORE ANYTHING IS UNMOUNTED OR DELETED.
	#
	# The publish lock is a cluster-wide lock on the IMAGE and is taken much later;
	# it says nothing about two builds on one machine sharing this directory. And
	# they cannot merely coexist: the first thing a build does is unmount whatever
	# is at $rootfs and recursively delete the workspace, so a second invocation
	# would tear the filesystem out from under a running first one and erase its
	# tree -- while the first keeps writing through a mountpoint that is gone.
	#
	# THE LOCK FILE IS OUTSIDE THE DIRECTORY IT PROTECTS, because the directory is
	# about to be deleted. A lock inside it is unlinked by the very `rm -rf` it
	# exists to serialize: the holder keeps a lock on a detached inode while the
	# next process creates a new file at the same path and locks that, and both run.
	# That is the failure this project already wrote down about the deployment lock
	# living in a cache directory.
	local lockfile="$WORK.lock"

	exec 9>"$lockfile"

	if ! flock -n 9; then
		echo "another build already holds $lockfile." >&2
		echo "" >&2
		echo "Two builds cannot share a workspace: this one would unmount the filesystem" >&2
		echo "the other is writing through and delete its tree. Wait for it, or run with" >&2
		echo "WORK pointing somewhere else." >&2
		exit 1
	fi

	clear_stale_mount "$rootfs"

	rm -rf "$WORK"
	mkdir -p "$rootfs"
	touch "$WORK/.billet-guest-workspace"

	# THE FILESYSTEM IS CREATED AND MOUNTED BEFORE ANYTHING IS INSTALLED, and the
	# build writes through it.
	#
	# THIS USED TO BE THE LAST STEP: the tree was assembled on the host and then
	# copied in with `mkfs.ext4 -d`. That is a second full copy of the image, and
	# at four gigabytes nobody noticed. At parity size it is the difference between
	# a build that fits on a runner and one that does not -- the tree and the image
	# are each tens of gigabytes, and only one of them has to exist at a time.
	#
	# It also makes the size limit surface WHERE IT IS CAUSED. Filling the
	# filesystem now fails inside the install step that overflowed it, naming that
	# package; the old shape failed at the very end, inside mkfs, describing only a
	# total that was too large for a number chosen an hour earlier.
	rm -f "$img"
	truncate -s "${SIZE_MB}M" "$img"
	mkfs.ext4 -q -F "$img"

	MOUNTED_ROOTFS="$rootfs"
	mount -o loop "$img" "$rootfs"

	echo "=== 1/6 base system ($SUITE) ==="
	# --variant=minbase, then systemd on top: the guest needs an init that can run
	# Docker's unit, and the alternative is hand-writing service supervision.
	#
	# GNU Wget does not race address families. On the reference host it selected an
	# established but black-holed IPv6 connection to archive.ubuntu.com and waited
	# for its 900-second default timeout, while the same object answered immediately
	# over IPv4. Keep IPv6 as a fallback, but prefer IPv4 and bound each attempt so a
	# weekly rebuild cannot spend most of its service timeout on one package.
	local download_bin="$WORK/download-bin"
	mkdir -p "$download_bin"
	cat >"$download_bin/wget" <<'EOF'
#!/bin/sh
exec /usr/bin/wget --prefer-family=IPv4 --timeout=30 --tries=5 "$@"
EOF
	chmod 0755 "$download_bin/wget"
	PATH="$download_bin:$PATH" debootstrap \
		--variant=minbase --include=systemd,systemd-sysv,dbus \
		"$SUITE" "$rootfs" http://archive.ubuntu.com/ubuntu/

	echo "=== 2/6 packages ==="
	# debootstrap writes a one-line legacy source for the base suite. Replace it
	# rather than layering the deb822 source beside it, which asks apt for the same
	# index twice and hides useful warnings in duplicate-target noise.
	rm -f "$rootfs/etc/apt/sources.list"
	cat >"$rootfs/etc/apt/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
Suites: $SUITE $SUITE-updates $SUITE-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF
	# MULTIVERSE AND RESTRICTED ARE NOT OPTIONAL ONCE THE TOOLSET DRIVES THIS.
	# GitHub's own package list includes p7zip-rar, which is in multiverse -- so
	# with main+universe alone the install fails on one package out of seventy-four
	# and takes the whole build with it. Enabling the components is what lets the
	# declaration be installed as written rather than edited down to what happens
	# to be reachable.

	# TWO LISTS, AND THE DISTINCTION IS THE WHOLE POINT OF SPLITTING THEM.
	#
	# BILLET_PACKAGES is what the GUEST MECHANISM needs: Docker for the jobs,
	# iproute2 and netplan for the network the agent reaches the metadata service
	# over, dnsmasq-base for the cache's DNS remap, libicu74 for the .NET runner,
	# e2fsprogs because the host grows this filesystem before boot. None of it is
	# on GitHub's list, because GitHub's image is not a microVM guest -- and none of
	# it may be dropped when the toolset changes.
	#
	# TOOLSET_PACKAGES is what a WORKFLOW expects, taken from GitHub's own
	# declaration and never edited here. Editing it is how the two images diverge
	# silently, which is exactly the gap this work exists to close.
	local billet_packages=(
		ca-certificates curl iproute2 iptables jq git sudo dnsmasq-base
		docker.io docker-buildx docker-compose-v2 e2fsprogs util-linux
		systemd-resolved netplan.io libicu74 zstd rsync build-essential
		python3-pip python3-venv python3-dev
	)

	local github_packages=()
	while IFS= read -r pkg; do
		[ -n "$pkg" ] && github_packages+=("$pkg")
	done < <(toolset_packages)

	if [ "${#github_packages[@]}" -eq 0 ]; then
		echo "the toolset declared no apt packages; refusing to build an image that would" >&2
		echo "silently carry only billet's own dependencies" >&2
		exit 1
	fi

	echo "installing ${#billet_packages[@]} billet packages and ${#github_packages[@]} from github's toolset"

	# INSTALLED IN ONE TRANSACTION so apt resolves the whole set together; two
	# passes can have the second uninstall something the first pulled in.
	#
	# --no-install-recommends, WHICH IS WHERE THIS DIFFERS FROM UPSTREAM ON PURPOSE.
	# runner-images installs each package with recommends on a full cloud image;
	# here every recommended package is permanent size in a file every node
	# downloads and every job clones. The toolset names what a workflow is entitled
	# to find, and that is what gets installed.
	# PASSED AS ARGUMENTS, NOT INTERPOLATED INTO THE PROGRAM TEXT.
	#
	# `${array[*]}` inside a double-quoted `bash -c` string flattens the names into
	# the SOURCE of the inner shell, which then word-splits, glob-expands and
	# interprets them. Today's names are all shell-safe, and nothing enforces that:
	# the digest proves the declaration is the file upstream published, which is a
	# statement about provenance and not about whether its strings are safe shell
	# syntax. A future entry containing a space, a `*`, or a `;` would be split,
	# expanded against the chroot's filesystem, or executed.
	#
	# `bash -s -- "$@"` passes them as argv, where none of that happens.
	chroot "$rootfs" /bin/bash -eux -s -- \
		"${billet_packages[@]}" "${github_packages[@]}" <<'APT'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends "$@"
apt-get clean
rm -rf /var/lib/apt/lists/*
APT

	# break-system-packages, exactly as runner-images writes it on 24.04, so a
	# workflow that `pip install`s against the system python succeeds here as it does
	# on a hosted runner instead of failing PEP 668's externally-managed guard. It is
	# a no-op for setup-python's self-contained toolcache pip, which is not an
	# externally-managed environment.
	install -m 0644 /dev/stdin "$rootfs/etc/pip.conf" <<'PIPCONF'
[global]
break-system-packages = true
PIPCONF

	# Docker 29 made the containerd image store the default for fresh installs, and
	# that store keeps image content outside Docker's data root. The slot-zero cache
	# mounts /var/lib/docker as one independently fenced filesystem, so accepting the
	# package default would publish a healthy but empty volume while pulled images
	# stayed on the disposable root disk. Pin the supported classic backend rather
	# than splitting one cache generation across two independently mounted trees.
	install -d -m 0755 "$rootfs/etc/docker"
	install -m 0644 /dev/stdin "$rootfs/etc/docker/daemon.json" <<'DOCKER'
{
  "features": {
    "containerd-snapshotter": false
  },
  "storage-driver": "overlay2",
  "bip": "172.17.0.1/16"
}
DOCKER

	# WHY THESE SIX, AND WHY THEY ARE NOT OPTIONAL.
	#
	# zstd IS THE ONE THAT LOOKS LIKE IT WORKS. actions/cache chooses its
	# compression by shelling out to `zstd --version`, falls back to gzip when it is
	# absent, AND FOLDS THE CHOSEN TOOL INTO THE CACHE VERSION HASH. Without it,
	# every cache this fleet saves has a different version from one saved on a
	# github-hosted runner: same key, permanent miss, no error and no log line. A
	# cache that silently never hits is worse than no cache, because the workflow
	# still pays to save it.
	#
	# unzip and tar ARE HARD REQUIREMENTS OF THE setup-* FAMILY.
	# @actions/tool-cache's extractZip calls io.which('unzip', true), which THROWS
	# when it is missing -- so any action that downloads a tool fails outright, which
	# is most of them.
	#
	# build-essential IS FOR THE SOURCE BUILD THAT HAS NO WHEEL. node-gyp, a native
	# ruby gem, a pip install with no matching wheel: all fail without a compiler,
	# and the error arrives from inside the package manager rather than from
	# anything that mentions the image.
	#
	# NOT build-essential FOR setup-python, which is folklore. That action needs the
	# distro to match its published manifest -- which is why this image is ubuntu
	# 24.04 and not something smaller -- and a compiler only when it falls back to
	# building from source.
	#
	# THE SYSTEM PYTHON GETS THE SAME PROVISIONING THE HOSTED IMAGE GIVES IT, which a
	# debootstrap of the release does not. runner-images installs
	# `python3 python3-dev python3-pip python3-venv` and, on non-22.04, writes
	# /etc/pip.conf with break-system-packages so pip still installs past PEP 668;
	# python-is-python3 supplies the `python` name. The concrete failure this fixes:
	# setup-python's `cache: pip` runs `pip cache dir` through io.which('pip', true),
	# which THROWS the moment no `pip` -- not `pip3` -- resolves, and this guest's only
	# pip was setup-python's own toolcache entry, on PATH just for the window its
	# addition holds; the hosted image never hit that because a system pip always
	# answered. Match it fully rather than in part, or the system pip resolves for
	# `which` but a workflow that runs `pip install` or `python -m venv` without
	# setup-python fails where a hosted runner succeeds -- a later, more confusing gap
	# than the one it replaces. The toolcache pip stays primary once setup-python
	# prepends its bin, so an install still lands on that interpreter's own newer pip.
	# The /etc/pip.conf write is above, right after the chroot install.
	#
	# wget and rsync are cheap and assumed by enough workflows to be worth the few
	# megabytes.

	echo "=== 3/6 the actions runner ==="
	# A DEDICATED, UNPRIVILEGED ACCOUNT. The runner refuses to run as root outright,
	# and a job that could write outside its own tree is a job that can rewrite the
	# agent that started it.
	chroot "$rootfs" /bin/bash -euxc "
		useradd --create-home --shell /bin/bash runner
		usermod -aG docker runner
		echo 'runner ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/runner
	"

	local tarball="actions-runner-linux-x64-$RUNNER_VERSION.tar.gz"

	# RETRIED, FOR THE REASON THE KERNEL FETCH IS. A sibling download of comparable
	# size died six minutes into a CI run with `curl: (92) HTTP/2 stream 1 was not
	# closed cleanly` and took an hour-long build with it. This one is a couple of
	# hundred megabytes over the same kind of link and had the same absence of
	# retries; it simply had not been unlucky yet.
	#
	# --retry-all-errors is the load-bearing flag: plain --retry covers http statuses
	# and timeouts but NOT curl-level transport faults, which is exactly what 92 is.
	curl -fsSL --http1.1 \
		--connect-timeout 20 --max-time 900 \
		--retry 5 --retry-delay 5 --retry-all-errors \
		-o "$WORK/$tarball" \
		"https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/$tarball"

	# VERIFIED BEFORE IT IS UNPACKED. This is a binary fetched over the network that
	# will execute somebody's CI, so "it downloaded" is not the same as "it is the
	# release it claims to be".
	echo "$RUNNER_SHA256  $WORK/$tarball" | sha256sum -c -

	mkdir -p "$rootfs/home/runner/runner"
	tar -xzf "$WORK/$tarball" -C "$rootfs/home/runner/runner"
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/runner-service.sh" \
		"$rootfs/home/runner/runner/billet-runner-service"
	chroot "$rootfs" chown -R runner:runner /home/runner
	# The Actions cache hook later creates _work/_billet as root. GNU install gives
	# only the final path to -o/-g, so leaving _work absent makes that parent root-owned
	# and Runner.Worker cannot create _work/_temp after the privilege drop.
	chroot "$rootfs" install -d -m 0755 -o runner -g runner /home/runner/runner/_work

	# THE ENVIRONMENT A HOSTED RUNNER EXPORTS, in a file the agent reads.
	#
	# THROUGH A FILE, NOT THE AGENT'S OWN ARRAY, because the agent is baked in a
	# QUOTED heredoc -- deliberately, so nothing in it is interpolated -- which
	# means it cannot carry a value this build computed. Writing them here also
	# makes each one a fact about the image that was actually built rather than a
	# constant restated in a second place, which is how the guest contract nearly
	# drifted before it was read back out of the installed agent.
	#
	# ImageOS IS THE ONE THIRD-PARTY ACTIONS BRANCH ON. GitHub exports it on every
	# hosted runner, and actions that select a prebuilt binary by platform read it;
	# on a runner where it is unset they fall back to building from source, or
	# fail. It is NOT what setup-python uses -- that shells out to `lsb_release -i
	# -r -s` and falls back to /etc/os-release, which is worth stating because the
	# folklore says otherwise and would send the next reader to the wrong place.
	#
	# NOTHING IS EXPORTED FOR SOFTWARE THIS IMAGE DOES NOT CARRY. A JAVA_HOME
	# pointing at a directory that does not exist makes setup-java and every build
	# tool downstream fail in a way that names none of this, while an UNSET one
	# simply makes setup-java install a JDK. So each variable is written by the
	# step that installs the software it describes, never in advance of it.
	#
	# CREATED BEFORE THE TOOLCACHE, WHICH APPENDS TO IT.
	#
	# install_java_toolcache adds a JAVA_HOME_<version>_X64 line per JDK, so this
	# file has to exist first and must never be truncated afterwards. It was
	# created below, with `>`, AFTER the toolcache ran -- which silently discarded
	# every JAVA_HOME the JDKs had just written, leaving five JDKs installed and
	# unfindable. That is the exact failure the toolcache section warns about one
	# directory over, and nothing about a running image would have said so.
	install -m 0644 /dev/stdin "$rootfs/etc/billet-image-env" <<'IMAGEENV'
ImageOS=ubuntu24
ImageVersion=billet
IMAGEENV

	# /proc FOR THE DURATION, because the runtimes this installs need it. See
	# MOUNTED_PROC: two of them locate their own libraries through an $ORIGIN
	# rpath, which glibc resolves by reading /proc/self/exe. The window is kept to
	# the step that needs it rather than the whole build.
	MOUNTED_PROC="$rootfs/proc"
	mount -t proc proc "$rootfs/proc"

	# THE CONTRACT, STATED AT THE CALL. The guest build assembles a filesystem it
	# is not running, so the target root is $rootfs and everything that must see
	# the target's own apt or interpreter goes through chroot. The EC2 build sets
	# BILLET_TC_ROOT to "" and the same functions run directly.
	# x64 BECAUSE THIS SCRIPT REFUSES TO RUN ANYWHERE ELSE. check_host_arch stops a
	# build on a non-x86_64 host, since the pinned runner is linux-x64 -- so the
	# guest's architecture is not a variable here the way it is for an AMI. Stating
	# it at the call rather than defaulting it keeps the installers' refusal of an
	# unset value meaningful.
	BILLET_TC_ROOT="$rootfs" \
		BILLET_TC_ARCH=x64 \
		BILLET_TC_DIR="$rootfs$TOOLCACHE_DIR" \
		BILLET_TC_IN_TARGET="$TOOLCACHE_DIR" \
		BILLET_TC_WORK="$WORK" \
		BILLET_TC_TOOLSET="$TOOLSET_FILE" \
		BILLET_TC_ENV_FILE="$rootfs/etc/billet-image-env" \
		billet_install_toolcache

	# CLOSED AS SOON AS THE STEP THAT NEEDS IT IS DONE. The trap would drop it
	# anyway, but a mount left open across the rest of the build is one more thing
	# every later step has to be correct about -- and the pack step in particular
	# must not be looking at a rootfs with a kernel filesystem inside it.
	unmount_guest_proc

	# WHAT THIS IMAGE ACTUALLY CONTAINS, WRITTEN INTO THE IMAGE.
	#
	# The runner tarball ships no version file of its own -- measured: the release
	# gate reported "no .runner-version in the image; cannot cross-check the
	# manifest", which meant the manifest's runner version was taken entirely on
	# trust. A manifest is free to claim any version; nothing was checking that the
	# claim matched the binary.
	#
	# That gap matters because the version drives the thirty-day expiry check. An
	# image whose manifest says 2.336.0 while the disk carries something older would
	# be judged fresh and would stop being sent jobs on a date derived from the wrong
	# number.
	#
	# Written here rather than derived later, because this is the only point where
	# what-was-downloaded and what-was-installed are the same fact.
	cat >"$rootfs/etc/billet-image" <<IMAGEINFO
RUNNER_VERSION=$RUNNER_VERSION
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGEINFO

	echo "=== 4/6 the agent that reads the registration ==="
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/docker-cache.sh" \
		"$rootfs/usr/local/bin/billet-docker-cache"
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/actions-proxy.py" \
		"$rootfs/usr/local/bin/billet-actions-proxy"
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/dns-upstreams.py" \
		"$rootfs/usr/local/bin/billet-dns-upstreams"
	# THE DOCKER SHIM SITS AHEAD OF THE REAL CLIENT on the job's PATH and points a
	# build's BuildKit cache client at the adapter, which is what lets a workflow
	# that changed only `runs-on` export `type=gha` from a container-driver
	# builder. The runner's PATH puts /usr/local/bin first; the shim finds the
	# real client behind itself.
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/docker-shim.sh" \
		"$rootfs/usr/local/bin/docker"
	install -m 0755 /dev/stdin "$rootfs/usr/local/bin/billet-agent" <<'AGENT'
#!/bin/bash
# Read this microVM's runner registration out of the metadata service and start the
# runner with it.
#
# MMDS V2, WHICH IS WHY THERE IS A TOKEN STEP. Under V1 any process in the guest
# reads the metadata with a bare GET, so a workflow step could take the registration;
# V2 refuses one without a session token. billet configures V2 explicitly because the
# service's own default is V1.
#
# THE REGISTRATION NEVER TOUCHES A DISK AND NEVER TOUCHES A COMMAND LINE. It is read
# into a variable and exported, which is where the runner expects it; writing it to a
# file would leave a live credential for the job that follows to read.
set -euo pipefail

MMDS=169.254.169.254

log() { echo "billet-agent: $*" >&2; }

# THE ROUTE FIRST. The metadata service answers on a link-local address, and a guest
# with an address but no route to it fails in a way that reads like the service is
# down rather than like the guest never asked.
ip route replace "$MMDS/32" dev eth0 2>/dev/null || true

# NOT `[ x ] && { ... }` AS A STATEMENT, ANYWHERE BELOW. Under `set -e` an `&&` list
# whose left side is FALSE returns 1, and a compound command returning 1 outside a
# condition is exactly what `set -e` exits on — so the guard fires when the thing it
# guards against did NOT happen. The first version of this agent used that idiom
# twice: it exited silently at the line before it would have started the runner, and
# systemd still reported `Started billet-agent.service`, because Type=exec only means
# the process was executed. Every branch here is a full if/then/fi for that reason.
token=""

for attempt in $(seq 1 120); do
	if token=$(curl -sf --connect-timeout 2 --max-time 5 -X PUT "http://$MMDS/latest/api/token" \
		-H "X-metadata-token-ttl-seconds: 300" 2>/dev/null); then
		break
	fi

	if [ "$attempt" -ge 120 ]; then
		log "the metadata service never answered"
		exit 1
	fi

	sleep 0.5
done

fetch() { curl -sf --connect-timeout 2 --max-time 5 -H "X-metadata-token: $token" "http://$MMDS/latest/meta-data/billet/$1"; }

# THE CONTRACT FIRST, BEFORE ANYTHING IS READ FROM IT.
#
# This agent is baked into a guest image that is published once and booted for
# months, while billet is upgraded independently — so the two CAN drift, and a
# billet that renamed a key would otherwise hand this script metadata it does not
# recognise. It would then find no registration, start no runner, and leave a microVM
# that booted perfectly and ran nothing.
#
# Refusing out loud is the whole point: the message names both versions, so the
# answer ("republish the image") is in the failure rather than in somebody's memory.
WANT_CONTRACT=10

if ! contract=$(fetch contract); then
	log "this billet did not say which metadata contract it speaks; it is older than this image"
	exit 1
fi

if [ "$contract" != "$WANT_CONTRACT" ]; then
	log "billet speaks metadata contract $contract and this image understands $WANT_CONTRACT"
	log "rebuild and republish the guest image with scripts/build-guest-image.sh"
	exit 1
fi

if ! jit=$(fetch jit-config); then
	log "no registration in the metadata"
	exit 1
fi

if ! name=$(fetch runner-name); then
	name=unknown
fi

# PULL-THROUGH CACHES ARE SITE-LOCAL AND PUBLIC. The Docker daemon can redirect
# Docker Hub through its supported registry-mirrors setting. BuildKit consumes
# all three upstream mappings later through the runner environment; Docker Engine
# has no equivalent per-registry setting for ghcr.io or quay.io.
registry_mirrors_json=""
if candidate=$(fetch registry-mirrors 2>/dev/null); then
	if jq -e '
		def origin:
			type == "string" and
			test("^https://[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[1-9][0-9]{0,4})?$") and
			(if test(":[0-9]+$") then (split(":")[-1] | tonumber) <= 65535 else true end);
		type == "object" and
		keys == ["docker.io", "ghcr.io", "quay.io"] and
		all(.[]; origin) and
		([.[]] | unique | length) == 3
	' >/dev/null 2>&1 <<<"$candidate"; then
		registry_mirrors_json=$candidate
		docker_mirror=$(jq -r '.["docker.io"]' <<<"$registry_mirrors_json")
		# Merge ONLY onto the existing config read successfully. Replacing an
		# unreadable daemon.json with {} would discard the baked-in storage-driver,
		# containerd-snapshotter and bip -- breaking the cache mount and the pinned
		# gateway -- for the sake of one optional setting. On any read failure the
		# file is left untouched and the job pulls directly. (cat in the `if` keeps
		# set -e from exiting the agent on that failure.)
		if daemon_base=$(cat /etc/docker/daemon.json 2>/dev/null) && [ -n "$daemon_base" ] &&
			rendered=$(jq -c --arg mirror "$docker_mirror" \
				'if type == "object" then . + {"registry-mirrors": [$mirror]} else error("not an object") end' \
				<<<"$daemon_base") && printf '%s\n' "$rendered" >/run/billet-docker-daemon.json &&
			install -m 0644 /run/billet-docker-daemon.json /etc/docker/daemon.json; then
			:
		else
			log "the Docker Hub mirror could not be configured; this job will pull directly upstream"
		fi
	else
		log "billet supplied invalid registry-mirror metadata; this job will pull directly upstream"
	fi
fi

# THE DOCKER IMAGE STORE IS ATTACHED BEFORE THE RUNNER, because service containers
# are pulled before the first workflow step and an action is therefore too late.
# Slot zero is reserved for it; ordinary sticky disks use the remaining four. The
# shared helper uses the same node API on Firecracker and EC2.
cache_endpoint=""
cache_token=""
buildkit_cache_mount_limit_bytes=""

if cache_endpoint=$(fetch cache-endpoint 2>/dev/null) &&
	cache_token=$(fetch cache-token 2>/dev/null) &&
	buildkit_cache_mount_limit_bytes=$(fetch buildkit-cache-mount-limit-bytes 2>/dev/null) &&
	[ -n "$cache_endpoint" ] && [ -n "$cache_token" ] &&
	[[ "$buildkit_cache_mount_limit_bytes" =~ ^[1-9][0-9]*$ ]]; then
	export BILLET_CACHE_ENDPOINT="$cache_endpoint"
	export BILLET_CACHE_TOKEN="$cache_token"
	export BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES="$buildkit_cache_mount_limit_bytes"
else
	cache_endpoint=""
	cache_token=""
	buildkit_cache_mount_limit_bytes=""
fi

# TRANSPARENT ACTIONS CACHE REQUESTS USE A NODE-LOCAL TLS TERMINATOR. The proxy
# address carries this guest's cache-session identity and the CA is unique to the
# node, so both arrive through MMDS and live only on this job's ephemeral root disk.
# The bundle includes the distribution roots because SSL_CERT_FILE replaces rather
# than augments that set for clients which honour it. Interception reaches the
# runner and its containers by a DNS remap of the one results origin, not a proxy
# variable; a job-started hook copies the trust bundle into RUNNER_TEMP and
# publishes NODE_EXTRA_CA_CERTS and SSL_CERT_FILE through GITHUB_ENV. The official
# runner translates that mounted path independently for job and action containers.
#
# THE DOCKER GATEWAY IS FIXED so the value written into daemon.json before docker
# starts matches the address the listeners bind after it. It is pinned by "bip" in
# the build-time daemon.json; docker0 carries it once the daemon is up.
docker_gateway=172.17.0.1
actions_proxy=""
actions_ca_path=""
actions_hook_path=""
# THE ONE PORT A CONTAINER-DRIVER BUILDKIT CAN USE. Its own image carries its own
# trust store, which billet cannot populate, so the remapped origin presents it a
# node leaf it refuses and `type=gha` dies with x509. The cache adapter serves
# plaintext HTTP here instead and does the TLS to the node itself. Loopback only:
# the node refuses to mint a signed blob URL naming anything else, so this is
# reachable exactly by a builder given the guest's network namespace.
actions_cache_port=41321
actions_cache_url=""
# Initialized here, not only inside the interception branch: it is read
# unconditionally after Docker starts, and an untrusted job -- which gets no
# interception metadata -- would otherwise hit it unset under `set -u` and die.
container_dns_active=""
if actions_proxy_candidate=$(fetch actions-proxy 2>/dev/null) &&
	actions_ca_candidate=$(fetch actions-ca-pem 2>/dev/null) &&
	[ -n "$actions_proxy_candidate" ] && [ -n "$actions_ca_candidate" ]; then
	actions_ca_dir=/home/runner/runner/_work/_billet
	actions_ca_path="$actions_ca_dir/actions-cache-ca.pem"
	actions_hook_path="$actions_ca_dir/actions-cache-job-started.sh"
	install -d -m 0755 -o runner -g runner "$actions_ca_dir"
	{
		cat /etc/ssl/certs/ca-certificates.crt
		printf '\n%s\n' "$actions_ca_candidate"
	} >"$actions_ca_path"
	chown runner:runner "$actions_ca_path"
	chmod 0444 "$actions_ca_path"
	cat >"$actions_hook_path" <<'ACTIONS_HOOK'
#!/bin/sh
set -eu

# NO PROXY TRAVELS THROUGH THE HOOK, only the CA and the loopback cache
# endpoint's URL. Interception is
# delivered by a DNS remap of the one results origin (see the guest agent), so
# there is no HTTPS_PROXY to publish: publishing one would route every request
# the runner and its job containers make -- action downloads, toolchains,
# artifact blob uploads -- through a single guest relay, which is exactly the
# funnel this design removed. The runner still needs the node's certificate to
# trust the intercepted origin, and job and action containers do not inherit the
# guest trust store, so the CA is copied into RUNNER_TEMP and published where the
# runner mounts it into those containers.
target="$RUNNER_TEMP/billet-actions-cache-ca.pem"
install -m 0444 "$BILLET_ACTIONS_CA_SOURCE" "$target"
{
	printf 'NODE_EXTRA_CA_CERTS=%s\n' "$target"
	printf 'SSL_CERT_FILE=%s\n' "$target"
} >>"$GITHUB_ENV"

# THE ADAPTER'S URL GOES THROUGH GITHUB_ENV BECAUSE THAT IS WHERE A STEP'S
# ENVIRONMENT COMES FROM. The docker shim reads it from the environment of the
# build it fronts, and a workflow that names the adapter itself needs it inside
# `cache-to: type=gha,url_v2=...`, an expression evaluated against the env
# context; the runner's own process environment reaches neither.
# GUARDED, because this hook runs under `set -u` and the adapter is allowed to
# have failed to start: interception is not conditional on it.
if [ -n "${BILLET_ACTIONS_CACHE_URL:-}" ]; then
	printf 'BILLET_ACTIONS_CACHE_URL=%s\n' "$BILLET_ACTIONS_CACHE_URL" >>"$GITHUB_ENV"
fi
ACTIONS_HOOK
	chown runner:runner "$actions_hook_path"
	chmod 0555 "$actions_hook_path"
	actions_proxy=$actions_proxy_candidate

	# THE NODE CA JOINS THE GUEST SYSTEM TRUST STORE, not only the runner's env.
	# The DNS remap captures EVERY process in the guest, including dockerd and the
	# BuildKit it embeds, which resolve the results origin through /etc/hosts and
	# trust only the system store. Without the CA there they would fail the TLS
	# handshake against the node's leaf where before they went direct to GitHub --
	# so `type=gha` cache export/import would break rather than pass through. The
	# runner's own SSL_CERT_FILE and the container hook stay as they are; this adds
	# the daemon-side clients the proxy variable used to leave untouched. (A buildx
	# `docker-container` builder runs BuildKit in its own image with its own store
	# and is not reached by this at all -- that builder is served by the plaintext
	# loopback adapter below, which it must be pointed at explicitly; see
	# docs/actions-cache.md.)
	# Best effort, and deliberately NOT a gate on interception. The runner and the
	# job/action containers trust the node leaf through their own NODE_EXTRA_CA_CERTS
	# bundle, so their cache and artifact traffic is unaffected if this fails. The
	# only clients that depend on the system store are daemon-side ones reaching the
	# remapped origin -- BuildKit's type=gha -- which is already the measured
	# limitation the conformance buildkit-gha lane covers; a failure here degrades
	# that one path to it rather than the whole cache, so it must not disable the remap.
	install -d -m 0755 /usr/local/share/ca-certificates
	if printf '%s\n' "$actions_ca_candidate" \
		>/usr/local/share/ca-certificates/billet-actions-cache.crt &&
		update-ca-certificates >/dev/null 2>&1; then
		:
	else
		log "the node CA could not be added to the system trust store; daemon-side type=gha builds fall to the measured limitation, runner and container caches are unaffected"
	fi

	# CONTAINER DNS IS POINTED AT THE GUEST RESOLVER BEFORE DOCKER STARTS. dockerd
	# reads daemon.json only at start and does not reload "dns" on SIGHUP, so this
	# has to land now -- the same window the registry-mirror merge above uses --
	# not after the daemon is running. The gateway is pinned by "bip", so its
	# address is known here even though docker0 does not exist yet.
	#
	# THE LIST IS FAIL-SAFE ONLY IF IT HAS A REAL UPSTREAM to fall through to, so
	# it is built ONLY when at least one non-stub upstream is found: [gateway,
	# ...upstreams]. If the guest resolver is later down, a container's query is
	# refused on the gateway and falls through to those upstreams unchanged -- a
	# missed cache remap, never broken container DNS. With no real upstream the
	# merge is skipped entirely and containers keep resolving through the host's
	# own resolver as before, rather than being pinned to a resolver-only-of-one.
	# Every step is guarded because this runs under `set -e`: a malformed resolver
	# file must degrade the cache, never abort the agent before the runner starts.
	# billet-dns-upstreams validates and orders the list ([gateway, ...upstreams]),
	# emitting nothing when no real upstream survives -- the one place a value dockerd
	# would reject is kept out of daemon.json. Its filtering is behavior-tested; the
	# guard here just refuses to touch daemon.json unless it produced a usable list.
	upstream_resolv=/run/systemd/resolve/resolv.conf
	if [ ! -s "$upstream_resolv" ]; then
		upstream_resolv=/etc/resolv.conf
	fi
	if dns_json=$(/usr/local/bin/billet-dns-upstreams "$docker_gateway" "$upstream_resolv" 2>/dev/null) &&
		[ -n "$dns_json" ]; then
		# Merge ONLY onto the existing config read successfully -- replacing an
		# unreadable daemon.json with {} would discard the baked-in storage-driver,
		# containerd-snapshotter and bip. On a read failure, leave it untouched and do
		# not activate the container remap. (cat in the `if` keeps set -e from exiting.)
		if daemon_base=$(cat /etc/docker/daemon.json 2>/dev/null) && [ -n "$daemon_base" ] &&
			rendered=$(jq -c --argjson dns "$dns_json" \
				'if type == "object" then . + {"dns": $dns} else error("not an object") end' \
				<<<"$daemon_base" 2>/dev/null) &&
			printf '%s\n' "$rendered" >/run/billet-docker-daemon.json &&
			install -m 0644 /run/billet-docker-daemon.json /etc/docker/daemon.json; then
			container_dns_active=1
		fi
	fi
	if [ -z "$container_dns_active" ]; then
		log "container cache DNS was not configured; containers will use GitHub's cache directly"
	fi
fi

# Docker is deliberately not enabled at boot. Starting it only after the cache
# device is mounted is what makes /var/lib/docker transparent to service
# containers rather than a volume that hides a daemon's already-open files.
if ! /usr/local/bin/billet-docker-cache prepare; then
	exit 1
fi

# INTERCEPTION IS DELIVERED BY A DNS REMAP OF ONE HOST, not a catch-all proxy.
# The results origin resolves to a guest-local transparent passthrough that
# tunnels to the node; every other destination resolves normally and goes direct.
# That routing is the whole point: an HTTPS_PROXY funnels ALL of the runner's
# traffic through one guest relay, and bulk transfers -- action tarballs,
# toolchains, artifact blob uploads -- stall and corrupt through it while small
# cache calls survive. The node still terminates TLS and decides handle-or-splice
# per request, so nothing about the interception itself changes here.
#
# THE FORWARDER BINDS THE DOCKER GATEWAY so the runner and job/service containers
# reach it at one address. The runner is remapped through /etc/hosts; containers
# do not inherit /etc/hosts, so a guest dnsmasq bound to the same gateway answers
# their queries and dockerd is pointed at it. Everything dnsmasq does not remap it
# forwards to the real upstream, so container egress is unaffected.
actions_cache_active=""
if [ -n "$actions_proxy" ] && [ -n "$actions_ca_path" ] && [ -n "$actions_hook_path" ]; then
	# These probes run under `set -o pipefail`, so a missing toolcache directory or a
	# docker0 that is not up would otherwise fail the pipeline and EXIT the agent.
	# `if ! x=$(...); then x=""` contains that AND guarantees the variable is empty on
	# failure, so the guard below skips interception -- a job on GitHub's cache
	# directly, never a dead job.
	if ! python_runtime=$(find /opt/hostedtoolcache/Python -path '*/x64/bin/python' -type f -o \
		-path '*/x64/bin/python' -type l 2>/dev/null | sort -V | tail -1); then
		python_runtime=""
	fi
	# docker0 must carry the pinned gateway, or the listeners would bind an address
	# daemon.json's dns list does not name and containers could not reach them.
	if ! docker_bridge=$(ip -4 -o addr show docker0 2>/dev/null |
		awk 'NR == 1 {split($4, a, "/"); print a[1]}'); then
		docker_bridge=""
	fi
	# Resolve the REAL results origin NOW, while its name still resolves to GitHub --
	# the /etc/hosts remap below repoints it at this listener. These addresses are
	# the passthrough's fail-open path: if the node cannot take a tunnel, the
	# client's TLS is relayed straight to GitHub so its cache call misses but the
	# artifact, log-archive and step traffic sharing this origin keeps working. The
	# gateway is excluded so a pre-existing remap cannot make the fallback a loop.
	#
	# A NON-EMPTY FALLBACK IS REQUIRED to activate: without it a later node outage
	# has nowhere to fail open to and the passthrough would close mid-TLS clients,
	# failing the artifact and log traffic that shares this origin. If resolution
	# yields nothing, interception is not activated and every origin stays direct.
	if ! results_fallback=$(getent ahostsv4 results-receiver.actions.githubusercontent.com 2>/dev/null |
		awk -v gateway="$docker_gateway" '$1 != gateway {print $1}' | sort -u | paste -sd, -); then
		results_fallback=""
	fi
	# SYSTEMD OWNS THE LISTENING SOCKET, via a transient .socket unit paired with the
	# service. PID 1 binds the privileged :443 before dropping the service to runner,
	# so the process needs no CAP_NET_BIND_SERVICE; and the socket outlives a service
	# crash -- new connections queue in its backlog during the ~100ms restart instead
	# of being refused, which is the gap a bare Restart=always leaves. Type=notify
	# makes the unit "active" only once the python has adopted the socket and reached
	# its accept loop, so the readiness gate below means "serving", not "forked".
	if [ -n "$python_runtime" ] && [ "$docker_bridge" = "$docker_gateway" ] && [ -n "$results_fallback" ] &&
		systemd-run --quiet --unit=billet-actions-proxy --collect --uid=runner --gid=runner \
			--property=Type=notify --property=NotifyAccess=main --property=TimeoutStartSec=5s \
			--property=Restart=always --property=RestartSec=100ms \
			--socket-property=ListenStream="$docker_gateway:443" \
			--socket-property=Accept=no --socket-property=FlushPending=no \
			"$python_runtime" /usr/local/bin/billet-actions-proxy \
			--systemd-socket --upstream "$actions_proxy" \
			--fallback-addr "$results_fallback"; then
		# systemd-run started only the SOCKET; the service is activated on demand. The
		# socket accepts a connection before -- or without -- the service adopting the
		# descriptor, so a bare TCP probe would mark interception active over a not-yet
		# -serving (or crash-looping) service and remap DNS at a dead backend. Start the
		# service explicitly instead: with Type=notify, `systemctl start` blocks until
		# the process sent READY=1 (it adopted the socket and reached its accept loop)
		# or TimeoutStartSec elapsed, so its exit status IS the readiness signal.
		if systemctl start billet-actions-proxy.service 2>/dev/null; then
			actions_cache_active=1
		fi
	fi
	if [ -z "$actions_cache_active" ]; then
		log "the Actions cache passthrough did not start (or the origin did not resolve); this job will use GitHub's cache directly"
		systemctl stop billet-actions-proxy.socket billet-actions-proxy.service 2>/dev/null || true
	fi

	# THE PLAINTEXT ADAPTER, for the one client the DNS remap cannot reach. Same
	# script, same tunnel, same fail-open addresses; the difference is that this
	# one terminates nothing and TERMINATES the TLS itself, so BuildKit never
	# meets a certificate it has to trust. It is a SEPARATE unit rather than a
	# second listener in the passthrough, so a crash of one is not a crash of both
	# and the passthrough's socket-activation contract is untouched.
	#
	# BOUND ON THE DOCKER GATEWAY, NOT LOOPBACK. A builder made with buildx's
	# docker-container driver lives in its own network namespace, where loopback
	# is its own and the guest's is out of reach without `network=host`; the
	# gateway is the one address both the guest and every container on the
	# bridge can dial. It is still inside the guest: nothing outside the microVM
	# routes to it, the node mints blob URLs naming it only for the adapter, and
	# the job's containers are the job's own. The `docker` shim on the job's PATH
	# points a build here, so no workflow has to.
	#
	# NOT A GATE ON INTERCEPTION, and what that costs is worth stating exactly. If
	# it does not start, the shim has nothing to point at and a container-driver
	# build fails its `type=gha` step with the same x509 it would have hit without
	# billet -- buildx fills an empty url_v2 from the real results URL, which is
	# DNS-remapped to a certificate the builder cannot verify. Everything else is
	# untouched: the runner's own cache, the artifacts and the logs all keep going
	# through the passthrough. Taking the remap down with it would trade a working
	# cache for a path that fails either way.
	#
	# BILLET_ADAPTER_START_BEGIN — the block between these markers is extracted and
	# EXECUTED against fake service-manager commands by
	# TestTheAgentPublishesTheAdapterURLOnlyWhenItIsServing. Grepping the agent for
	# a unit name proves only that the text is present, which the shutdown line
	# below satisfies on its own.
	if [ -n "$actions_cache_active" ]; then
		if [ -n "$python_runtime" ] &&
			systemd-run --quiet --unit=billet-actions-cache-adapter --collect \
				--uid=runner --gid=runner \
				--property=Type=notify --property=NotifyAccess=main \
				--property=TimeoutStartSec=5s \
				--property=Restart=always --property=RestartSec=100ms \
				--socket-property=ListenStream="$docker_gateway:$actions_cache_port" \
				--socket-property=Accept=no --socket-property=FlushPending=no \
				"$python_runtime" /usr/local/bin/billet-actions-proxy \
				--mode cache-adapter --systemd-socket --upstream "$actions_proxy" \
				--fallback-addr "$results_fallback" --ca-file "$actions_ca_path" &&
			systemctl start billet-actions-cache-adapter.service 2>/dev/null; then
			actions_cache_url="http://$docker_gateway:$actions_cache_port/"
		else
			log "the BuildKit cache adapter did not start; a container-driver build will fail"
			log "its type=gha step exactly as it does without billet, and nothing else is affected"
			systemctl stop billet-actions-cache-adapter.socket \
				billet-actions-cache-adapter.service 2>/dev/null || true
		fi
	fi
	# BILLET_ADAPTER_START_END
fi

# THE RUNNER RESOLVES THE RESULTS ORIGIN THROUGH /etc/hosts, and only that one
# name. Every other host is untouched, so the runner's action, toolchain and
# artifact-blob traffic resolves normally and never reaches the passthrough.
if [ -n "$actions_cache_active" ]; then
	printf '%s results-receiver.actions.githubusercontent.com\n' "$docker_gateway" >>/etc/hosts
fi

# CONTAINERS DO NOT INHERIT /etc/hosts, so a guest dnsmasq answers for them at the
# gateway dockerd's dns list already names. It ALWAYS forwards; it adds the results
# remap ONLY when the passthrough is up, so if the passthrough never started a
# container resolves the origin to real GitHub and goes direct (a miss) rather than
# to a dead listener. It runs whenever container DNS was configured -- which happens
# only when a real upstream was found -- so a resolver outage falls through to those
# upstreams rather than breaking container DNS.
if [ -n "$container_dns_active" ]; then
	upstream_resolv=/run/systemd/resolve/resolv.conf
	if [ ! -s "$upstream_resolv" ]; then
		upstream_resolv=/etc/resolv.conf
	fi
	# --resolv-file names the upstreams dnsmasq forwards to; do NOT also pass
	# --no-resolv, which would win and leave it with none, failing every query it
	# does not remap. --conf-file=/dev/null keeps it to these arguments alone, and
	# -u root avoids dropping to a dnsmasq user that dnsmasq-base does not create.
	dnsmasq_args=(--keep-in-foreground --no-daemon --conf-file=/dev/null -u root
		--listen-address="$docker_gateway" --bind-interfaces
		--resolv-file="$upstream_resolv")
	if [ -n "$actions_cache_active" ]; then
		dnsmasq_args+=(--address="/results-receiver.actions.githubusercontent.com/$docker_gateway")
	fi
	if ! systemd-run --quiet --unit=billet-cache-dns --collect \
		--property=Restart=always --property=RestartSec=100ms \
		/usr/sbin/dnsmasq "${dnsmasq_args[@]}"; then
		log "the container cache resolver did not start; containers will use GitHub's cache directly"
	fi
fi

# THE COMMAND ARRIVES AS JSON IN A STRING, and both halves of that are deliberate.
#
# JSON, because a tier's command is an argv, and word-splitting it here would be
# billet guessing at somebody's quoting.
#
# In a STRING, because the metadata service cannot hand over anything else. A plain
# GET is answered in IMDS format, which renders a JSON string or lists the keys of a
# JSON object — and nothing else. An array comes back 501, "Cannot retrieve value. The
# value has an unsupported type." billet sent one as a real array once: the guest
# reached this exact line, got the 501, and stopped. Everything before it had worked,
# so what an operator saw was a microVM that booted perfectly and ran no job.
#
# `Accept: application/json` would fetch an array correctly today, and is deliberately
# NOT used: setting `imds_compat` on the service makes firecracker ignore that header,
# which would make this a guest that stops working because of a change on the host.
cmd=()

if ! raw=$(fetch command); then
	log "no command in the metadata; billet may be sending it in a form the service "
	log "cannot serve — only strings and objects can be fetched in IMDS format"
	exit 1
fi

# ONE BASE64 LINE PER ARGUMENT, so an argument may contain anything an argument is
# allowed to contain. Reading `jq -r '.[]'` directly is newline-delimited, which
# silently splits an argument containing a newline into two — billet quietly editing
# somebody's argv, which is the exact thing carrying this as JSON exists to prevent.
# Measured: `["/bin/sh","-c","echo one\ntwo","tail arg"]` came back as five arguments.
#
# BILLET_AGENT_DECODE_BEGIN — the block between these markers is extracted verbatim
# and exercised by TestTheGuestAgentReconstructsAnArgvExactly. Reading `raw` and
# leaving `cmd` is the whole contract; keep it that way or the test will say so.

# AND JQ'S STATUS IS CHECKED BEFORE ANYTHING IS BUILT FROM ITS OUTPUT, because a
# `while read` fed by process substitution cannot see it — neither `set -e` nor
# `pipefail` reaches across that redirect. A jq that emitted three arguments and then
# failed would hand the runner a TRUNCATED argv, which is worse than no argv at all:
# `sh -c 'rm -rf x' extra` and `sh -c 'rm -rf x'` are different commands and only one
# of them was asked for.
#
# --slurp SO THAT "ONE ARGV" MEANS ONE. Without it jq reads a STREAM of documents and
# `-e` reports only the last one's result, so `["/bin/printf","%s"] ["extra"]` passed
# validation, encoded the records of BOTH, and produced a command nobody sent.
#
# AND NO ARGUMENT MAY CONTAIN A NUL, because a command substitution cannot hold one:
# bash drops it, so an argument carrying one would arrive SHORTER than it was sent --
# changed silently, which is the one outcome this whole path exists to prevent. billet
# refuses these before sending; the guest refuses them again because the argv it runs
# should depend on what it can actually carry, not on the sender being careful.
#
# THIS CATCHES THE JSON ESCAPE AND NOT A LITERAL NUL BYTE, and that limit is deliberate
# rather than overlooked. `raw` was read with a command substitution too, so bash has
# already dropped any literal NUL before this line runs; catching those would mean
# never letting the metadata touch a shell variable at all, which is a different agent.
#
# It is not worth being a different agent. A literal NUL can only arrive if something
# other than billet is writing this microVM's metadata, and whatever can do that can
# simply write `["/bin/whatever"]` instead — the NUL wins it nothing it did not already
# have. So this refuses what a WRONG billet could send, which is the threat that is
# real, and does not pretend to defend against a REPLACED one, which it cannot.
if ! printf '%s' "$raw" | jq -e --slurp '
	length == 1 and (.[0] | type == "array" and length > 0
		and all(.[]; type == "string" and (contains("\u0000") | not)))' >/dev/null; then
	log "the command in the metadata is not a single non-empty array of NUL-free strings"
	exit 1
fi

# EVERY RECORD CARRIES A BYTE IN FRONT, SO NO RECORD IS EVER AN EMPTY LINE.
#
# `$()` strips ALL trailing newlines, and an EMPTY argument encodes to an empty line —
# so a command whose last argument was empty simply lost it, and `sh -c '…' arg ''`
# reached the guest as `sh -c '…' arg`. That is a different command: `$#` is 1 instead
# of 2, and a script that tests its argument count takes a different branch. A constant
# byte in front means the final record always has content for `$()` to keep.
if ! encoded=$(printf '%s' "$raw" | jq -r --slurp '.[0][] | @base64 | "x" + .'); then
	log "the command in the metadata could not be encoded for transfer"
	exit 1
fi

while IFS= read -r line; do
	# THE SENTINEL EXISTS BECAUSE $() STRIPS TRAILING NEWLINES and an argument is
	# allowed to end in one. Append a byte inside the substitution and take it off
	# outside — and do both in ONE step, because a decode helper that RETURNED the
	# value would have it stripped a second time by the substitution that called it.
	#
	# `&&` RATHER THAN `;`, so the status is base64's. With a `;` the substitution
	# reports the status of the final `printf`, which is always 0 — so a decoder that
	# failed would contribute an empty argument and `set -e` would never see it.
	if ! decoded=$(printf '%s' "${line#x}" | base64 -d && printf X); then
		log "an argument in the command could not be decoded"
		exit 1
	fi

	cmd+=("${decoded%X}")
done <<<"$encoded"

# AND THE COUNT IS PROVED RATHER THAN ASSUMED.
#
# Every framing bug this decode has had was a silent one: an argument split in two, an
# argument dropped, a truncated argv from a failure nothing observed. Each of them
# changes the NUMBER of arguments, and the number is something the metadata states
# independently — so comparing them turns the whole class into a loud failure at the
# one moment somebody can still act on it.
want=$(printf '%s' "$raw" | jq --slurp '.[0] | length')

# AND `want` IS PROVED TO BE A NUMBER FIRST. `[ x -ne y ]` on a non-number is an
# ERROR, not a false -- and an error inside an `if` condition is simply a branch not
# taken, so the guard would wave through exactly the input it was added to catch.
case "$want" in
	'' | *[!0-9]*)
		log "the command in the metadata does not have a countable number of arguments"
		exit 1
		;;
esac

if [ "${#cmd[@]}" -ne "$want" ]; then
	log "the command has $want arguments and ${#cmd[@]} came back; refusing to run a"
	log "command that is not the one billet sent"
	exit 1
fi

# BILLET_AGENT_DECODE_END

if [ "${#cmd[@]}" -eq 0 ]; then
	log "the command in the metadata is empty"
	exit 1
fi

log "starting $name with ${#cmd[@]} argument(s)"

export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit"

cd /home/runner/runner
# RUNNER_TOOL_CACHE IS PASSED THROUGH THE exec, not left to the environment.
#
# /etc/environment is read by PAM for login sessions and does NOT apply to systemd
# services, and this agent IS one -- so setting it there alone would leave every
# job looking in the runner's default _work/_tool, finding nothing, and downloading
# a runtime the image already contains. Nothing would report that: the job would
# simply be slower.
#
# setpriv does not create a login environment either. HOME, USER and LOGNAME are
# part of the runner-account contract, not conveniences: actions/setup-go can
# install a toolchain without them and then Go refuses to start because it has no
# user cache directory. The EC2 image entrypoint establishes the same three values.
runner_env=(
	"ACTIONS_RUNNER_INPUT_JITCONFIG=$ACTIONS_RUNNER_INPUT_JITCONFIG"
	"ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true"
	"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE:-}"
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	"HOME=/home/runner"
	"USER=runner"
	"LOGNAME=runner"
	"RUNNER_TOOL_CACHE=/opt/hostedtoolcache"
	"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache"
)

# WHAT THE IMAGE SAYS IT IS, added to what the job will see.
#
# THIS ARRAY IS THE JOB'S WHOLE ENVIRONMENT -- it is passed through `env -i`, so a
# variable absent here does not exist for the job whatever /etc/environment says.
# The build writes /etc/billet-image-env with the values a hosted runner exports
# (ImageOS and friends), and they have to be read in HERE to reach a job at all.
#
# READ AS DATA, NOT SOURCED. `source` on a file would execute it, and while this
# one is written by the build, a shell that executes a config file is a shape
# worth not having. Only NAME=VALUE lines are taken, and a malformed line is
# skipped rather than ending the launch: an image that lost this file should run
# jobs that download their own toolchains, not refuse to run jobs at all.
# THE PATH IS A DEFAULTED VARIABLE so a test can drive this exact code against a
# fixture. Grepping the agent for the assignment proves only that the text is
# present, which is satisfied by dead code -- the seam is what lets the block be
# EXECUTED and its effect on the array observed.
IMAGE_ENV_FILE="${IMAGE_ENV_FILE:-/etc/billet-image-env}"

if [ -r "$IMAGE_ENV_FILE" ]; then
	while IFS= read -r line; do
		case "$line" in
			[A-Za-z_]*=*) runner_env+=("$line") ;;
		esac
	done <"$IMAGE_ENV_FILE"
fi
if [ -n "$cache_endpoint" ] && [ -n "$cache_token" ]; then
	runner_env+=("BILLET_CACHE_ENDPOINT=$cache_endpoint" "BILLET_CACHE_TOKEN=$cache_token"
		"BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES=$buildkit_cache_mount_limit_bytes")
fi
if [ -n "$actions_cache_active" ] && [ -n "$actions_ca_path" ] && [ -n "$actions_hook_path" ]; then
	# NO HTTPS_PROXY. Interception reaches the runner by a DNS remap of the one
	# results origin, so only that host's traffic is redirected and every other
	# request -- action downloads, toolchains, artifact blob uploads -- resolves
	# normally and goes direct. The runner still needs the node's CA to trust the
	# intercepted origin, and the job-started hook publishes it into containers.
	runner_env+=("NODE_EXTRA_CA_CERTS=$actions_ca_path" "SSL_CERT_FILE=$actions_ca_path"
		"BILLET_ACTIONS_CA_SOURCE=$actions_ca_path"
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=$actions_hook_path")
	# ONLY WHEN THE ADAPTER IS ACTUALLY SERVING. The docker shim, and a workflow
	# that writes `url_v2=${{ env.BILLET_ACTIONS_CACHE_URL }}`, point BuildKit
	# wherever this says, so publishing it for a listener that never started
	# would point a build at a refused connection instead of leaving it on
	# GitHub's cache.
	#
	# BILLET_ADAPTER_ENV_BEGIN — extracted and executed with the startup block
	# above by TestTheAgentPublishesTheAdapterURLOnlyWhenItIsServing, because a
	# listener that serves and a job that is told about it are two facts and only
	# the pair of them is the feature.
	if [ -n "$actions_cache_url" ]; then
		runner_env+=("BILLET_ACTIONS_CACHE_URL=$actions_cache_url")
	fi
	# BILLET_ADAPTER_ENV_END
fi
if [ -n "$registry_mirrors_json" ]; then
	runner_env+=("BILLET_REGISTRY_MIRRORS_JSON=$registry_mirrors_json")
fi

set +e
# BILLET_AGENT_LAUNCH_BEGIN
setpriv --reuid=runner --regid=runner --init-groups --inh-caps=-all -- \
	env -i "${runner_env[@]}" "${cmd[@]}"
job_status=$?
# BILLET_AGENT_LAUNCH_END
set -e

# Stop the socket too, or a late connection would re-activate the service.
systemctl stop billet-actions-proxy.socket billet-actions-proxy.service 2>/dev/null || true
systemctl stop billet-actions-cache-adapter.socket \
	billet-actions-cache-adapter.service 2>/dev/null || true

/usr/local/bin/billet-docker-cache complete "$job_status"

# GitHub has already recorded every recognized one-job result. Preserve the
# runner service's exit contract after using its richer code as a cache gate.
service_status=$(/usr/local/bin/billet-docker-cache service-status "$job_status") ||
	service_status=$job_status
exit "$service_status"
AGENT

	install -m 0644 /dev/stdin "$rootfs/etc/systemd/system/billet-agent.service" <<'UNIT'
[Unit]
Description=billet: start the GitHub Actions runner from the metadata service
# AFTER THE NETWORK, because the registration is read over it. A guest that started
# this first would exhaust its retries before eth0 had an address.
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
ExecStart=/usr/local/bin/billet-agent
# ONE JOB, ONE GUEST. The runner exits when its job is done and the microVM is
# destroyed with it, so a restart would register a second runner against a
# registration that has already been consumed.
Restart=no
# BOTH NAMES, SAME VALUE, which is what github's own image does. The toolkit reads
# only RUNNER_TOOL_CACHE; the runner itself also honours AGENT_TOOLSDIRECTORY, an
# azure-pipelines inheritance -- and an image that sets one but not the other
# behaves differently depending on which layer resolves the path first.
Environment=RUNNER_TOOL_CACHE=/opt/hostedtoolcache
Environment=AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache
# journal+console, NOT journal ALONE, AND THIS IS A DEBUGGABILITY DECISION.
#
# A microVM has no console anybody normally reads and no way in: if the agent
# refuses its metadata, or cannot reach the service, the explanation lands in a
# journal inside a guest that is about to be destroyed. What an operator sees is a
# VM that started and ran nothing, with the reason already deleted.
#
# Sending it to the console costs nothing in production -- billet passes no
# console= to the guest, so there is nowhere for it to go -- and it is the entire
# difference between a boot test that can read the agent's verdict and one that
# can only observe that systemd executed something. Type=exec reports Started for
# a process that exits immediately, which the agent itself carries a paragraph
# about, so "Started billet-agent.service" is not evidence of anything.
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=multi-user.target
UNIT

	echo "=== 5/6 boot configuration ==="
	install -m 0644 /dev/stdin "$rootfs/etc/systemd/network/10-eth0.network" <<'NET'
[Match]
Name=eth0

[Network]
# DHCP, because the address belongs to the bridge rather than to billet. A
# deployment whose bridge hands out no addresses has to say so another way, and
# `billet check` proves the bridge exists rather than that it serves.
DHCP=yes

[DHCPv4]
# KEY THE LEASE ON THE MAC, NOT A DUID. networkd's default client identifier is a
# DUID derived from /etc/machine-id, and the debootstrapped image bakes ONE
# machine-id into every clone -- so with a DUID two guests present the same client
# id and dnsmasq hands them the same address, which is the collision that stalled
# large downloads. The firecracker backend now gives each guest a stable MAC
# derived from its tap (unique among live guests, reused only after one exits), so
# keying DHCP on the MAC makes each live guest's lease unique AND lets a later guest
# reusing that tap renew the same address instead of consuming another -- bounding
# pool use by concurrency rather than by launch count, which a per-boot-unique DUID
# would not.
ClientIdentifier=mac

# THE METADATA SERVICE IS LINK-LOCAL and is not on the bridge's subnet, so it needs a
# route of its own. The agent adds one too; having it here means the guest can reach
# the service before anything has run.
[Route]
Destination=169.254.169.254/32
Scope=link
NET

	chroot "$rootfs" /bin/bash -euxc '
		systemctl disable docker.service docker.socket 2>/dev/null || true
		systemctl enable systemd-networkd systemd-resolved billet-agent
		# A CONSOLE THAT GOES NOWHERE COSTS BOOT TIME. billet passes no console= to
		# the guest, so a getty on ttyS0 would spin against a device nothing reads.
		systemctl mask getty@tty1.service serial-getty@ttyS0.service
		systemctl mask systemd-resolved-monitor.service 2>/dev/null || true
		printf "RUNNER_TOOL_CACHE=/opt/hostedtoolcache\nAGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n" >>/etc/environment
		echo billet-guest >/etc/hostname
		printf "127.0.0.1 localhost\n127.0.1.1 billet-guest\n" >/etc/hosts
		# ROOT CANNOT LOG IN. Nothing should be logging into a guest that exists for
		# one job, and an account with no password is not the same as a locked one.
		passwd -l root
	'

	echo "=== 6/6 filesystem ==="

	# MEASURED WHILE IT IS STILL MOUNTED, because that is the only moment the used
	# figure is readable at all. An unmounted image file reports its allocated
	# size, which says nothing about how full it is.
	#
	# THIS MARGIN IS FOR THE BUILD, NOT FOR THE JOB, and the distinction is worth
	# stating because the obvious reading is wrong. A job does NOT run in whatever
	# is left here: the backend clones this image, resizes the clone to the tier's
	# `disk` and runs resize2fs on it before the guest boots, so the space a job
	# gets is the tier's number. Sizing this image for a job's working set would
	# make every generation carry space the clone is going to add anyway.
	#
	# What the margin protects is the build itself. ext4 needs room to complete
	# metadata operations, and a filesystem written to the last block fails in the
	# middle of an install step rather than reporting that it is full.
	local used_mb free_mb
	used_mb=$(df -BM --output=used "$rootfs" | tail -1 | tr -dc '0-9')
	free_mb=$(df -BM --output=avail "$rootfs" | tail -1 | tr -dc '0-9')

	echo "contents: ${used_mb}M used, ${free_mb}M free of ${SIZE_MB}M"

	if [ "$free_mb" -lt "$MIN_FREE_MB" ]; then
		echo "" >&2
		echo "this image has ${free_mb}M free and the build needs at least ${MIN_FREE_MB}M." >&2
		echo "" >&2
		echo "The contents measured ${used_mb}M. Raise SIZE_MB to at least" >&2
		echo "$((used_mb + MIN_FREE_MB))M, or install less." >&2
		echo "" >&2
		echo "Refusing here rather than publishing: a filesystem written to its last" >&2
		echo "block fails inside whichever install step overflows it next time, with a" >&2
		echo "message about that package rather than about this number." >&2
		exit 1
	fi

	# READ WHILE THE IMAGE IS STILL MOUNTED. Everything below describes the image
	# from files INSIDE it, and after the unmount `$rootfs` is an empty directory
	# on the host -- so a read that happens after it finds nothing and the build
	# stops with "could not read the guest contract", which blames the agent for
	# the ordering of this function. Moving the filesystem creation to the start of
	# the build turned every read of the finished tree into a read through a
	# mountpoint, and this is the one that had not moved with it.
	local contract
	contract=$(read_guest_contract "$rootfs")

	unmount_rootfs

	echo "built $img ($(du -h "$img" | cut -f1))"

	# WHAT WAS ACTUALLY BUILT, WRITTEN WHERE SOMETHING ELSE CAN READ IT.
	#
	# RUNNER_VERSION may have arrived empty and been resolved from the pinned file
	# a hundred lines above, so the caller's environment does not necessarily say
	# what this image contains — only this process knows. A publisher describing
	# the image from its own inputs would put the REQUESTED version in the manifest
	# and the INSTALLED one on the disk, and those differ exactly when the request
	# was blank, which is the normal scheduled case.
	#
	# THE CONTRACT IS READ BACK OUT OF THE AGENT THAT WAS INSTALLED, not restated
	# here. The agent is embedded in a QUOTED heredoc — deliberately, so nothing in
	# it is interpolated — which means its `WANT_CONTRACT=` is a literal the outer
	# script cannot see. Restating it here would create a second copy that drifts
	# silently, and the drift is invisible in the worst way: the manifest would
	# advertise a contract the image does not speak, a node would accept the image
	# on that basis, and the guests would boot and never report.
	cat >"$WORK/build-info.env" <<INFO
RUNNER_VERSION=$RUNNER_VERSION
GUEST_CONTRACT=$contract
ARCH=$(uname -m)
IMAGE_NAME=$IMAGE_NAME
IMAGE_FILE=$img
INFO

	echo "recorded $WORK/build-info.env"

	if [ "$PUBLISH" != "yes" ]; then
		echo "PUBLISH=no, so it was not written to ceph"
		return
	fi

	# THE MANUAL PATH PASSES THE SAME CONTENTS GATE AS THE RELEASE WORKFLOW. This
	# script is the documented custom and air-gapped publisher, so leaving the gate
	# only in GitHub Actions would let the path an operator actually runs publish an
	# image that the automated path refuses.
	"$SCRIPT_DIR/check-guest-image.sh" "$img"

	publish "$img"
}

# THE CLUSTER-WIDE PUBLISH LOCK, held by whatever is about to write the image.
#
# IT LIVES HERE RATHER THAN IN THE SCHEDULED WRAPPER, because this is the script that
# writes. A lock in the wrapper protected the timer's path and left the documented
# normal use -- running this by hand -- writing into the same head image with no
# coordination at all, which is exactly the corruption it was added to prevent.
#
# A dedicated 1MB image rather than a lock on the golden image itself: mapping an
# image takes an automatic exclusive-lock on it (measured -- the head carries an
# `auto <id>` locker while mapped), so locking the thing being written collides with
# the write. It is created with `layering` alone because Ceph documents the
# `exclusive-lock` FEATURE as incompatible with these advisory lock commands.
#
# MEASURED SEMANTICS: `lock add` returns 0 when taken and 16 when anyone holds it,
# including the same cookie, so it is not re-entrant; `lock rm` returns 0, or 2 when
# there was nothing to release; and the lock is NOT a lease -- it outlives the process
# that took it, and breaking it fences nothing.
LOCK_IMAGE="${LOCK_IMAGE:-$IMAGE_POOL/.publish-lock}"
LOCK_COOKIE="billet-build-$(hostname -s 2>/dev/null || echo unknown)-$$-$(date -u +%s)"

# STALE_AFTER is when a held lock stops being believed.
#
# BECAUSE A LEAKED LOCK IS OTHERWISE PERMANENT, and that is the failure this bound
# exists for rather than a tidiness setting. Bash does not run an EXIT trap when it is
# killed by an untrapped signal, so a systemd timeout, a `kill`, or a power loss
# leaves the lock held by a process that no longer exists -- and since a refusal never
# breaks a lock, EVERY later build on EVERY node refuses too. Forever. The fleet then
# stops being rebuilt, and thirty days after a runner release it stops being sent
# jobs, which is precisely the outage this whole mechanism exists to prevent.
#
# Six hours is chosen against the unit that runs this: TimeoutStartSec is two, so no
# scheduled build can still be alive at six, and a hand-run build that has taken six
# hours has failed in some other way. Breaking one that IS alive would put two writers
# on one image, so the bound is deliberately far past any real run.
STALE_AFTER="${STALE_AFTER:-21600}"

take_publish_lock() {
	rbd --id "$CEPH_USER" create "$LOCK_IMAGE" --size 1 --image-feature layering \
		>/dev/null 2>&1 || true

	if rbd --id "$CEPH_USER" lock add "$LOCK_IMAGE" "$LOCK_COOKIE" >/dev/null 2>&1; then
		install_publish_traps

		echo "holding the cluster publish lock as $LOCK_COOKIE"

		return 0
	fi

	local held age
	held=$(rbd --id "$CEPH_USER" lock ls "$LOCK_IMAGE" --format json 2>/dev/null || echo '[]')
	age=$(printf '%s' "$held" | jq -r --argjson now "$(date -u +%s)" \
		'.[0].id // "" | capture("-(?<t>[0-9]+)$") | ($now - (.t | tonumber))' 2>/dev/null || echo "")

	if [ -n "$age" ] && [ "$age" -gt "$STALE_AFTER" ] 2>/dev/null; then
		local id locker
		id=$(printf '%s' "$held" | jq -r '.[0].id')
		locker=$(printf '%s' "$held" | jq -r '.[0].locker')

		echo "the publish lock has been held by $id for ${age}s, which is longer than any" >&2
		echo "build can run; breaking it and taking it" >&2

		rbd --id "$CEPH_USER" lock rm "$LOCK_IMAGE" "$id" "$locker" >/dev/null 2>&1 || true

		if rbd --id "$CEPH_USER" lock add "$LOCK_IMAGE" "$LOCK_COOKIE" >/dev/null 2>&1; then
			install_publish_traps

			return 0
		fi
	fi

	local holder
	holder=$(printf '%s' "$held" |
		jq -r '.[0] | "\(.id) (client \(.locker) at \(.address))"' 2>/dev/null || true)

	echo "another node is already publishing to $IMAGE_POOL/$IMAGE_NAME: ${holder:-unknown holder}." >&2
	echo "This build is stopping rather than writing the same image concurrently. If that" >&2
	echo "holder is gone and this persists, clear it with:" >&2
	echo "  rbd --id $CEPH_USER lock rm $LOCK_IMAGE '<id>' '<locker>'" >&2

	exit 1
}

# ONE HANDLER FOR EVERYTHING THIS HAS TO UNDO, and one place that installs it.
#
# THE BUG THIS REPLACES LEAKED THE LOCK ON EVERY SUCCESSFUL PUBLISH. take_publish_lock
# installed `trap release_publish_lock EXIT`, and then publish() installed
# `trap 'unmap_image "$dev"' EXIT` -- which REPLACES it, because bash keeps one
# action per signal -- and finally ran `trap - EXIT`, removing that too.
# release_publish_lock was never called explicitly, so the lock survived every
# normal run. It is not a lease, so every publisher on every node then refused for
# six hours, and the operator's only clue was a message about a holder that had
# finished successfully hours earlier.
#
# Two traps for one signal is the trap, so to speak: there is now one handler, it
# does everything, and nothing is allowed to install a second.
publish_cleanup() {
	local status=$?

	if [ -n "${MAPPED_DEV:-}" ]; then
		unmap_image "$MAPPED_DEV"
		MAPPED_DEV=""
	fi

	release_publish_lock

	return "$status"
}

# A SIGNAL HANDLER THAT DOES NOT EXIT LETS THE SCRIPT CARRY ON WITHOUT THE LOCK.
#
# A bash TERM or INT trap does not terminate the shell by itself. The previous
# version returned from release_publish_lock and execution resumed -- so a build
# that was signalled mid-publish would release the lock, keep writing the image,
# and let a second publisher take the lock and write it too. Concurrent writers,
# which is the one thing this lock exists to prevent, reached by way of the
# cleanup.
#
# Re-raising with the default handler is what makes the exit status honest to
# whatever is watching, which for the scheduled path is systemd.
install_publish_traps() {
	trap publish_cleanup EXIT

	trap 'publish_cleanup; trap - TERM; kill -TERM $$' TERM
	trap 'publish_cleanup; trap - INT; kill -INT $$' INT
}

release_publish_lock() {
	local locker
	locker=$(rbd --id "$CEPH_USER" lock ls "$LOCK_IMAGE" --format json 2>/dev/null |
		jq -r --arg c "$LOCK_COOKIE" '.[] | select(.id == $c) | .locker' 2>/dev/null || true)

	if [ -n "$locker" ]; then
		rbd --id "$CEPH_USER" lock rm "$LOCK_IMAGE" "$LOCK_COOKIE" "$locker" >/dev/null 2>&1 || true
	fi
}

# unmap_image releases a mapping on the way out, however this script is leaving.
#
# Best-effort by design: it runs on the failure path, where the useful message is the
# one about what actually went wrong rather than a second one about the cleanup.
unmap_image() {
	if [ -n "${1:-}" ]; then
		rbd --id "$CEPH_USER" device unmap "$1" 2>/dev/null || true
	fi
}

# publish writes the image into the pool as a NEW generation.
#
# A NEW SNAPSHOT EVERY TIME, never a moved one. A generation is what running jobs hold
# clones of, and clone v2 lets a parent be removed while its children live — so
# rewriting one in place would change the filesystem underneath a job that is already
# reading it.
publish() {
	local img="$1" gen dev
	gen="g$(date -u +%Y%m%d%H%M%S)"

	# BEFORE THE FIRST WRITE, which is what must not overlap. The build up to here
	# happens in a per-machine workspace and coordinates with nothing.
	take_publish_lock

	local rbd=(rbd --id "$CEPH_USER")

	local want=$((SIZE_MB + 512))

	if ! "${rbd[@]}" -p "$IMAGE_POOL" info "$IMAGE_NAME" >/dev/null 2>&1; then
		"${rbd[@]}" -p "$IMAGE_POOL" create "$IMAGE_NAME" --size "${want}M" --object-size 4M
	else
		# GROWN IF IT HAS TO BE, because an image that already exists was sized for
		# whatever the last generation needed. Writing a larger filesystem into it
		# fails partway through with `No space left on device` — a corrupt image with
		# a successful-looking build behind it, since the write is the only step that
		# would have said so.
		#
		# EXISTING SNAPSHOTS KEEP THEIR OWN SIZE, so growing the head does not touch a
		# generation a running job holds a clone of.
		local have
		have=$("${rbd[@]}" -p "$IMAGE_POOL" info "$IMAGE_NAME" --format json | jq -r '.size / 1048576 | floor')

		if [ "$have" -lt "$want" ]; then
			echo "growing $IMAGE_POOL/$IMAGE_NAME from ${have}M to ${want}M"
			"${rbd[@]}" -p "$IMAGE_POOL" resize "$IMAGE_NAME" --size "${want}M"
		fi
	fi

	dev=$("${rbd[@]}" device map "$IMAGE_POOL/$IMAGE_NAME")

	# EXIT, NOT RETURN. A RETURN trap fires when a function RETURNS, and `set -e`
	# aborting the script is not a return — so the one case this trap exists for, a
	# failed write partway through, was exactly the case it did not fire in. The
	# golden image then stayed mapped on the build host, and the next run's `device
	# map` added a second mapping of the same image rather than failing, which is how
	# a build host ends up with a dozen of them.
	# RECORDED, NOT TRAPPED. Installing a second EXIT trap here is what silently
	# discarded the lock release; publish_cleanup unmaps whatever this names.
	MAPPED_DEV="$dev"

	# gnudd, NOT dd: Ubuntu 26.04's uutils coreutils does not implement
	# `iflag=direct`, which is the same class of difference that broke `cephadm
	# bootstrap` on this host. See docs/adr-003-ceph-rbd.md.
	local ddbin=dd
	command -v gnudd >/dev/null 2>&1 && ddbin=gnudd

	"$ddbin" if="$img" of="$dev" bs=4M conv=fsync status=progress

	"${rbd[@]}" device unmap "$dev"
	dev=""
	MAPPED_DEV=""

	"${rbd[@]}" -p "$IMAGE_POOL" snap create "$IMAGE_NAME@$gen"

	# WHAT THIS IMAGE ACTUALLY INSTALLED, recorded where anything with cluster access
	# can read it.
	#
	# THE ALTERNATIVE WAS A LIE WAITING TO HAPPEN. The pinned runner version is
	# compiled into the billet binary, so it says what a build WOULD install rather
	# than what the running fleet HAS -- and the moment a scheduled rebuild takes up a
	# newer release, an alarm reading the compiled-in value reports an expiry that is
	# not happening, or misses one that is. The image is the only thing that knows.
	# KEYED BY GENERATION, because a tier boots a generation rather than the head.
	#
	# A single `billet.runner_version` described the LAST BUILD, which is not what any
	# job runs: generations are immutable and promotion is a deliberate act, so a
	# fleet can sit on last month's generation while the head advances every week. An
	# alarm reading the head then reports the newest build as though it were the
	# fleet, says everything is current, and stays green right through the expiry it
	# exists to catch. It is also written BEFORE verification, so a generation that
	# fails to boot would have advanced it too.
	#
	# Per generation, the value describes exactly the thing a tier can name.
	"${rbd[@]}" -p "$IMAGE_POOL" image-meta set "$IMAGE_NAME" "billet.runner_version.$gen" \
		"$RUNNER_VERSION"

	# The head keys stay as a record of the most recent build. Nothing reads them for
	# a verdict; they are there so `image-meta list` says what happened last.
	"${rbd[@]}" -p "$IMAGE_POOL" image-meta set "$IMAGE_NAME" billet.last_build_runner \
		"$RUNNER_VERSION"
	"${rbd[@]}" -p "$IMAGE_POOL" image-meta set "$IMAGE_NAME" billet.last_build_generation "$gen"

	echo
	echo "published $IMAGE_POOL/$IMAGE_NAME@$gen"
	echo
	echo "Put it in a tier:"
	echo
	echo "  - label: your-label"
	echo "    provider: firecracker"
	echo "    image: $IMAGE_NAME@$gen"
	echo
	echo "A generation is immutable: running jobs hold clones of it, and clone v2 lets"
	echo "this one be removed later while those clones keep reading it correctly."
}

main "$@"
