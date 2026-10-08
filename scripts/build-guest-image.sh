#!/usr/bin/env bash
#
# Build the guest image a Firecracker microVM boots, and publish it to Ceph.
#
# A SCRIPT RATHER THAN A `billet` SUBCOMMAND, unlike `billet ami`. That one has to
# drive an API — launch a builder instance, wait, snapshot it — which is a program.
# This one boots a filesystem under systemd-nspawn on the machine it is already on
# and runs GitHub's own build scripts in it, and a Go
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

# ROOT'S HOME AND NO CALLER'S XDG DIRECTORIES. chroot keeps the environment, so
# every installer run inside the image writes its first-run state wherever these
# point, and a CI runner's point at /home/runner: an image built there shipped
# /home/runner/.config/NuGet owned by root, and every job whose tool creates
# ~/.config/<tool> failed with EACCES. HOME alone did not stop it; PowerShell's
# NuGet provider, which the toolcache's module installs reach, prefers
# XDG_CONFIG_HOME to HOME, which fits but was not measured. The ownership pass
# before step 6 is what holds whichever it was.
export HOME=/root
unset XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_RUNTIME_DIR

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

if [ -z "${RUNNER_VERSION:-}" ] && [ ! -r "$PINNED_RUNNER_FILE" ]; then
	echo "cannot read the pinned runner version at $PINNED_RUNNER_FILE, and RUNNER_VERSION" >&2
	echo "was not set; run this from a checkout, or say which release to install" >&2
	exit 1
fi

RUNNER_VERSION="${RUNNER_VERSION:-$(awk "NR==1{print \$1}" "$PINNED_RUNNER_FILE")}"
SUITE="${SUITE:-noble}"
# 81920MB, MEASURED: the runner-images build reported `contents: 53779M used,
# 22176M free of 81920M` on the image builder on 2026-09-28, the same within a few
# megabytes on three consecutive builds. That is GitHub's whole image, the Android
# SDK included (ADR-005, #209), plus billet's layer.
#
# THE DECLARED SIZE IS NOT THE USABLE SIZE: 53779 + 22176 is 75955, not 81920,
# because ext4's journal, inode tables and the 5% reserved-blocks default take
# about 7.3% off the top. A margin computed by subtracting content from the
# declaration overstates itself by about six gigabytes here.
#
# THE GUARD BELOW CANNOT CATCH AN OVERFLOW DURING AN INSTALL. It compares free
# space after the build, so a build that fills the filesystem part way fails in
# the step that overflowed, naming what it was writing. It still catches a build
# that finishes with no margin left, and it is what reports the measurement.
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
SIZE_MB="${SIZE_MB:-81920}"

# MIN_FREE_MB is the build's own margin, checked against a MEASUREMENT.
#
# `SIZE_MB` is not derivable from anything in this script -- it depends on what
# the install steps actually put on disk, which is a fact about a build rather
# than about a configuration. So the build measures what it used, reports it, and
# refuses if the margin is gone, naming the number to raise SIZE_MB to. That is
# what keeps this file's constant a recorded measurement instead of a guess that
# happened to work, which is the rule the billet-config skill states about byte sizes.
MIN_FREE_MB="${MIN_FREE_MB:-512}"
IMAGE_POOL="${IMAGE_POOL:-billet-images}"
IMAGE_NAME="${IMAGE_NAME:-ubuntu-2404-x64}"
CEPH_USER="${CEPH_USER:-billet}"
WORK_DEFAULT=/var/tmp/billet-guest
WORK="${WORK:-$WORK_DEFAULT}"
PUBLISH="${PUBLISH:-yes}"

# HOW THE IMAGE IS BUILT (#250): from Ubuntu's own cloud root filesystem, running
# GitHub's runner-images template in it under systemd-nspawn, so parity with a
# hosted runner holds by construction and the difference is one reviewed file
# (scripts/runner-images/differences.tsv).

# PINNED, NOT "LATEST", for the reason internal/ops/images/ami.go gives about the AMI:
# an image is a thing you reproduce, and a build that silently tracked the newest
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
# install_container_hook vendors GitHub's reference docker container hook into the
# image, verified against internal/guestassets/container-hook.pin, and installs
# billet's wrapper in front of it.
#
# THE PIN IS ONE LINE, VERSION AND SHA256, for the reason the runner's is: a
# checksum is only true of its version. The zip carries one file, index.js, which
# lands as upstream.js beside the wrapper the runner is pointed at.
install_container_hook() {
	local rootfs=$1
	local pin="$SCRIPT_DIR/../internal/guestassets/container-hook.pin"
	local version sha256 zip
	version=$(awk 'NR==1{print $1}' "$pin")
	sha256=$(awk 'NR==1{print $2}' "$pin")
	if [ -z "$version" ] || [ -z "$sha256" ]; then
		echo "cannot read the container hook version and checksum from $pin" >&2
		exit 1
	fi
	zip="actions-runner-hooks-docker-$version.zip"
	curl -fsSL --http1.1 \
		--connect-timeout 20 --max-time 300 \
		--retry 5 --retry-delay 5 --retry-all-errors \
		-o "$WORK/$zip" \
		"https://github.com/actions/runner-container-hooks/releases/download/v$version/$zip"
	echo "$sha256  $WORK/$zip" | sha256sum -c -
	install -d -m 0755 "$rootfs/usr/local/lib/billet/container-hook"
	unzip -p "$WORK/$zip" index.js >"$rootfs/usr/local/lib/billet/container-hook/upstream.js"
	chmod 0644 "$rootfs/usr/local/lib/billet/container-hook/upstream.js"
	install -m 0644 "$SCRIPT_DIR/../internal/guestassets/container-hook.js" \
		"$rootfs/usr/local/lib/billet/container-hook/index.js"
	printf '%s\n' "$version" >"$rootfs/usr/local/lib/billet/container-hook/VERSION"
}

# install_guest_billet builds billet from this tree into the image, with the newest
# go the image's toolcache carries, and installs Bazel's credential helper beside
# it. The go command's own GOTOOLCHAIN rule fetches (and verifies) a newer
# toolchain when go.mod asks for one the toolcache lacks.
install_guest_billet() {
	local rootfs=$1
	local candidate go="" newest=""
	for candidate in "$rootfs$TOOLCACHE_DIR"/go/*/x64/bin/go; do
		[ -x "$candidate" ] || continue
		local version=${candidate#"$rootfs$TOOLCACHE_DIR"/go/}
		version=${version%%/*}
		if [ -z "$newest" ] || [ "$(printf '%s\n%s\n' "$newest" "$version" | sort -V | tail -n 1)" = "$version" ]; then
			newest=$version
			go=$candidate
		fi
	done
	if [ -z "$go" ]; then
		echo "the image's toolcache carries no go to build billet with; the go and bazel" >&2
		echo "  build caches need billet inside the guest" >&2
		exit 1
	fi
	install -d -m 0755 "$rootfs/opt/billet/bin"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=readonly \
		GOPATH="$WORK/go" GOCACHE="$WORK/go-build" \
		"$go" -C "$SCRIPT_DIR/.." build -trimpath -o "$rootfs/opt/billet/bin/billet" ./cmd/billet
	chmod 0755 "$rootfs/opt/billet/bin/billet"
	install -m 0755 /dev/stdin "$rootfs/opt/billet/bin/bazel-credential-helper" <<'HELPER'
#!/bin/sh
exec /opt/billet/bin/billet cache credential-helper "$@"
HELPER
	echo "guest billet: built with go $newest"
}

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

# MOUNTED_ROOTFS is the mountpoint this build is currently writing through, or
# empty. The trap reads it, so it must be set BEFORE the mount and cleared AFTER
# the unmount rather than around them.
MOUNTED_ROOTFS=""

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

	# NOT WHILE A MACHINE MAY STILL HOLD IT. A booted image that could not be proved
	# stopped keeps its mount: unmounting would detach the filesystem under it and
	# leave it writing to an image the next run deletes through. The next run
	# stops the machine before it clears the workspace.
	if [ -n "$RUNNER_IMAGES_STUCK" ]; then
		echo "leaving $MOUNTED_ROOTFS mounted: the machine booted on it could not be proved stopped" >&2
		return 0
	fi

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

	stop_runner_images
	unmount_rootfs

	trap - "$signal"
	kill -s "$signal" $$
}

# THE ORDER IS IN THE TRAP, NOT INSIDE EITHER FUNCTION. The machine booted on the
# rootfs is stopped before the rootfs is unmounted, and unmount_rootfs refuses when
# that stop could not be proved; the sequence is stated once, here.
trap 'stop_runner_images; unmount_rootfs' EXIT
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

# RUNNER_IMAGES_BOOTED is the root filesystem booted under systemd-nspawn, or
# empty. POWERED OFF BEFORE ANY UNMOUNT, because a booted machine holds the
# filesystem and every mount inside it. RUNNER_IMAGES_STUCK is set when a stop
# could not be proved, and keeps the filesystem mounted from then on.
RUNNER_IMAGES_BOOTED=""
RUNNER_IMAGES_STUCK=""

# stop_runner_images powers off the booted image, if there is one, and says
# whether it could prove it did.
stop_runner_images() {
	[ -n "$RUNNER_IMAGES_BOOTED" ] || return 0

	if ! BILLET_RI_ROOTFS="$RUNNER_IMAGES_BOOTED" "$SCRIPT_DIR/runner-images-nspawn.sh" stop; then
		RUNNER_IMAGES_STUCK=1
		echo "the machine booted on $RUNNER_IMAGES_BOOTED could not be proved stopped" >&2
		return 1
	fi
	RUNNER_IMAGES_BOOTED=""
}

# runner_images_machine names this workspace's machine, so builds in two
# workspaces on one host never share (or stop) one. Twelve characters, so the
# machine's link is exactly ve-<name> (see runner-images-nspawn.sh).
runner_images_machine() {
	local digest
	# CHECKED ON ITS OWN, not inside printf's argument, whose status would hide a
	# failed hash and name the machine "bri-".
	digest=$(printf '%s' "$WORK" | sha256sum) || return 1
	digest=${digest:0:8}
	if ! [[ $digest =~ ^[0-9a-f]{8}$ ]]; then
		echo "could not derive this workspace's machine name from its digest" >&2
		return 1
	fi
	printf 'bri-%s\n' "$digest"
}

# recover_runner_images stops every machine that may still hold this workspace's
# filesystem: each registered machine whose root is this rootfs, through the unit
# systemd says holds it, and this workspace's own machine, whose forwarding rules
# only its own name removes. It fails when the machines cannot be listed, a
# property cannot be read, or any of them cannot be proved stopped, because the
# caller clears the workspace next. There is no record to trust: a machine is
# found by what it holds, not by what a file says its name was.
recover_runner_images() {
	local rootfs="$1" driver="$SCRIPT_DIR/runner-images-nspawn.sh" listed rest line name root unit own
	# NO machinectl, NO MACHINE, but only while systemd-nspawn is absent too: they
	# ship in one package, and nspawn without machinectl cannot be asked.
	if ! command -v machinectl >/dev/null 2>&1; then
		if command -v systemd-nspawn >/dev/null 2>&1; then
			echo "systemd-nspawn is installed and machinectl is not, so this host's machines cannot be listed" >&2
			return 1
		fi
		return 0
	fi
	if ! listed=$(machinectl list --no-legend --no-pager 2>&1); then
		echo "could not list this host's machines: $listed" >&2
		return 1
	fi
	# SPLIT IN THE SHELL, line by line from the captured listing, with no parser
	# or read that could fail into a shorter list.
	rest=$listed$'\n'
	while [ -n "$rest" ]; do
		line=${rest%%$'\n'*}
		rest=${rest#*$'\n'}
		line=${line#"${line%%[![:space:]]*}"}
		name=${line%%[[:space:]]*}
		[ -n "$name" ] || continue
		# A MACHINE GONE BEFORE ITS ROOT IS READ may have left its processes in a
		# unit this never learned, so it is not known to be stopped; and an empty
		# root is no evidence that it is elsewhere. machinectl's own diagnostic
		# goes to stderr, never into the value compared.
		if ! root=$(machinectl show --property=RootDirectory --value "$name") || [ -z "$root" ]; then
			echo "could not read machine $name's root" >&2
			return 1
		fi
		[ "$root" = "$rootfs" ] || continue
		if ! unit=$(machinectl show --property=Unit --value "$name" 2>&1) || [ -z "$unit" ]; then
			echo "could not read the unit holding machine $name: $unit" >&2
			return 1
		fi
		BILLET_RI_MACHINE="$name" BILLET_RI_UNIT="$unit" BILLET_RI_ROOTFS="$rootfs" "$driver" stop ||
			return 1
	done
	own=$(runner_images_machine) || return 1
	BILLET_RI_MACHINE="$own" BILLET_RI_ROOTFS="$rootfs" "$driver" stop
}

# install_cloud_base unpacks Ubuntu's 24.04 cloud root filesystem, pinned in
# scripts/runner-images/base.pin, into the mounted image and makes it a billet
# guest's base: the filesystem GitHub's template starts from on Azure, without
# Azure.
install_cloud_base() {
	local rootfs="$1" release file sum
	read -r release file sum <"$SCRIPT_DIR/runner-images/base.pin"
	if [ -z "$sum" ]; then
		echo "scripts/runner-images/base.pin names no release, file and sha256" >&2
		exit 1
	fi

	local tarball="$WORK/$file"
	curl -fsSL --http1.1 \
		--connect-timeout 20 --max-time 900 \
		--retry 5 --retry-delay 5 --retry-all-errors \
		-o "$tarball" \
		"https://cloud-images.ubuntu.com/releases/noble/$release/$file"

	# VERIFIED BEFORE IT IS UNPACKED, against the sum pinned from a SHA256SUMS
	# whose signature was checked when the pin was written.
	echo "$sum  $tarball" | sha256sum -c -

	tar --numeric-owner --xattrs --xattrs-include='*' -xJpf "$tarball" -C "$rootfs"
	rm -f "$tarball"

	# THE CLOUD IMAGE MOUNTS / BY LABEL, and this filesystem has none: the kernel
	# mounts the root from its command line.
	printf '# the root filesystem is mounted by the kernel from its command line\n' \
		>"$rootfs/etc/fstab"

	# NO CLOUD-INIT: a guest takes its registration from billet's metadata agent,
	# and cloud-init would wait at every boot for a datasource that is not there.
	touch "$rootfs/etc/cloud/cloud-init.disabled"

	write_apt_sources "$rootfs" \
		"https://archive.ubuntu.com/ubuntu/ https://mirrors.edge.kernel.org/ubuntu/"
	rm -f "$rootfs/etc/apt/sources.list"
	install -m 0644 /dev/stdin "$rootfs/etc/apt/apt.conf.d/90billet-fetch" <<'APTCONF'
Acquire::Retries "3";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APTCONF
}

# write_apt_sources writes the image's one deb822 source for $SUITE from URIS.
write_apt_sources() {
	local rootfs="$1" uris="$2"
	cat >"$rootfs/etc/apt/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: $uris
Suites: $SUITE $SUITE-updates $SUITE-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF
}

# run_runner_images_build boots the base under systemd-nspawn, runs GitHub's
# template in it through scripts/run-runner-images.sh, and powers it off; then
# adds what the guest mechanism needs that GitHub's image has no reason to carry.
run_runner_images_build() {
	local rootfs="$1" driver="$SCRIPT_DIR/runner-images-nspawn.sh"

	# THE IMAGE'S OWN RESOLVER COMES BACK AFTER THE BOOT. nspawn replaces
	# /etc/resolv.conf with the builder's uplink servers for the build, and an image
	# shipping that file would resolve through the builder's DNS wherever it runs.
	cp -a "$rootfs/etc/resolv.conf" "$WORK/resolv.conf.image"

	RUNNER_IMAGES_BOOTED="$rootfs"
	BILLET_RI_ROOTFS="$rootfs" "$driver" start

	BILLET_RI_ROOTFS="$rootfs" \
		BILLET_RI_TARGET="$driver" \
		BILLET_RI_IMAGE_VERSION="$(date -u +%Y%m%d).billet" \
		BILLET_RI_OUT="$WORK/runner-images" \
		"$SCRIPT_DIR/run-runner-images.sh"

	# WHAT THE GUEST MECHANISM NEEDS THAT GITHUB'S IMAGE DOES NOT CARRY: dnsmasq-base
	# for the cache's DNS remap, networkd, resolved and netplan for the network the
	# agent reaches the metadata service over, libicu74 for the .NET runner,
	# e2fsprogs because the host grows this filesystem, and the two a hosted runner
	# has that CI jobs here installed every run. apt skips any GitHub already put in.
	# IN THE BOOTED MACHINE, where the network is: a chroot has no resolver once
	# the image's own is back.
	BILLET_RI_ROOTFS="$rootfs" "$driver" exec /bin/bash -euxc '
		apt-get update -qq
		apt-get install -y --no-install-recommends "$@"
		apt-get clean
		rm -rf /var/lib/apt/lists/*' bash \
		dnsmasq-base systemd-resolved netplan.io libicu74 e2fsprogs apparmor python3-apt

	stop_runner_images

	rm -f "$rootfs/etc/resolv.conf"
	cp -a "$WORK/resolv.conf.image" "$rootfs/etc/resolv.conf"

	# ONE MACHINE ID PER GUEST: the boot above wrote one, and a clone that carried
	# it would share it with every other guest of this generation.
	: >"$rootfs/etc/machine-id"
}

# write_image_env writes /etc/billet-image-env, the environment the agent hands
# the job, from the NAME=VALUE lines GitHub's scripts wrote to /etc/environment:
# ImageOS, ImageVersion, the JAVA_HOME and GOROOT lines, ANDROID_HOME, the tool
# cache and GitHub's PATH, as a hosted runner exports them.
#
# THE AGENT PASSES EACH LINE AS IT IS, as the job's whole environment, so each
# line is made a plain NAME=VALUE here by pam-environment.awk, the reader the build's own steps
# use; and $HOME, which GitHub writes into PATH and XDG_CONFIG_HOME for a login
# shell to expand, is the runner's home, which nothing downstream expands.
write_image_env() {
	local rootfs="$1"
	awk -f "$SCRIPT_DIR/runner-images/pam-environment.awk" "$rootfs/etc/environment" |
		sed -e 's#\$HOME#/home/runner#g' -e 's#\${HOME}#/home/runner#g' \
		>"$rootfs/etc/billet-image-env.tmp"
	if ! grep -q '^ImageOS=' "$rootfs/etc/billet-image-env.tmp"; then
		echo "GitHub's build wrote no ImageOS to /etc/environment; refusing an image whose jobs" >&2
		echo "would not see the variables a hosted runner exports" >&2
		exit 1
	fi
	chmod 0644 "$rootfs/etc/billet-image-env.tmp"
	mv "$rootfs/etc/billet-image-env.tmp" "$rootfs/etc/billet-image-env"
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
	# systemd-nspawn, machinectl and systemd-run BOOT THE IMAGE the scripts run in;
	# curl, sha256sum and xz fetch and verify its base.
	local tools=(mkfs.ext4 e2fsck chroot jq flock mountpoint
		systemd-nspawn machinectl systemd-run curl sha256sum xz)
	for t in "${tools[@]}"; do
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
		echo "missing: ${missing[*]} (apt-get install systemd-container xz-utils e2fsprogs ceph-common)" >&2
		exit 1
	fi

	# THE GUEST'S ARCHITECTURE IS THE HOST'S, AND THE RUNNER IS PINNED TO x64.
	# The base filesystem and the machine that runs GitHub's scripts are the host's
	# architecture, so on an arm64 machine this would build an arm64 userspace and
	# drop an x86-64 runner into it.
	# That combination boots perfectly and fails at the moment the agent execs the
	# runner, with an executable-format error inside a guest nobody has a console for
	# -- the exact shape of failure this whole image is built to make impossible.
	local arch
	arch=$(uname -m)

	if [ "$arch" != "x86_64" ]; then
		echo "this builds an x86-64 guest (the pinned runner is linux-x64) and this host is" >&2
		echo "$arch; build it on an x86-64 machine, or teach this script to select the" >&2
		echo "runner and base filesystem architecture together" >&2
		exit 1
	fi
}


main() {
	need_root
	need_tools

	# BEFORE ANYTHING IS BUILT, because this file decides what goes in.
	verify_toolset

	# ONE SPELLING PER DIRECTORY, before anything is derived from it: a trailing
	# slash or a `..` would otherwise give one workspace two locks and two machine
	# names, and a second build would clear the first one's workspace under it.
	WORK=$(realpath -m -- "$WORK")

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
	# THE DEFAULT AS SPELLED, NOT AS RESOLVED: a symlink planted at the default
	# path resolves somewhere else, and that somewhere must prove it is a workspace
	# like any other override before it is deleted.
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

	# A MACHINE AN EARLIER RUN LEFT BOOTED ON THIS WORKSPACE GOES FIRST, WHATEVER
	# THIS RUN BUILDS: its filesystem is about to be unmounted and deleted, which
	# must not happen under a machine still writing to it. Its name is this
	# workspace's.
	BILLET_RI_MACHINE=$(runner_images_machine)
	export BILLET_RI_MACHINE
	if ! recover_runner_images "$rootfs"; then
		echo "an earlier build's machine on $rootfs could not be stopped; refusing to clear" >&2
		echo "the workspace under it" >&2
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

	echo "=== 1/6 base system (Ubuntu's cloud root filesystem) ==="
	install_cloud_base "$rootfs"

	echo "=== 2/6 GitHub's runner-images build ==="
	run_runner_images_build "$rootfs"


	echo "=== 3/6 the actions runner ==="
	# A DEDICATED, UNPRIVILEGED ACCOUNT. The runner refuses to run as root outright,
	# and a job that could write outside its own tree is a job that can rewrite the
	# agent that started it.
	chroot "$rootfs" /bin/bash -euxc "
		# THE ACCOUNT MAY EXIST ALREADY: the runner-images build creates it with
		# GitHub's uid for the step GitHub runs unprivileged.
		id runner >/dev/null 2>&1 || useradd --create-home --shell /bin/bash runner
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
	# GITHUB'S SCRIPTS WROTE THE HOSTED ENVIRONMENT TO /etc/environment in the
	# runner-images build, and installed the toolcache; the agent passes the job
	# exactly what /etc/billet-image-env names, so it is that file's lines.
	write_image_env "$rootfs"

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
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/exec-env.sh" \
		"$rootfs/usr/local/bin/billet-exec-env"
	# THE DOCKER SHIM SITS AHEAD OF THE REAL CLIENT on the job's PATH and points a
	# build's BuildKit cache client at the adapter, which is what lets a workflow
	# that changed only `runs-on` export `type=gha` from a container-driver
	# builder. Installed twice: /usr/local/bin/docker is first on the runner's own
	# PATH, and /opt/billet/bin/docker is what the container hook bind-mounts into
	# a job container and the job hook prepends to GITHUB_PATH, so the shim is
	# first wherever a step runs and finds the image's client behind itself.
	install -m 0755 "$SCRIPT_DIR/../internal/guestassets/docker-shim.sh" \
		"$rootfs/usr/local/bin/docker"
	install -D -m 0755 "$SCRIPT_DIR/../internal/guestassets/docker-shim.sh" \
		"$rootfs/opt/billet/bin/docker"

	# BILLET ITSELF, for the build caches: it is the go command's GOCACHEPROG and
	# Bazel's credential helper on a tier that enables them, and nothing starts it
	# otherwise. Built here from this very tree, with the newest go the toolcache
	# step just installed, because a hosted build runner has had its own go
	# removed to make room; the go command verifies every module against go.sum.
	install_guest_billet "$rootfs"

	# THE CONTAINER HOOK: GitHub's reference docker hook, pinned by version and
	# sha256 the way the runner is, behind a wrapper that adds one mount. The
	# runner runs it with its own node, so the guest needs no node of its own.
	install_container_hook "$rootfs"
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

# THE LONGEST TOKEN THE SERVICE ISSUES (21600 seconds). The agent reads the
# runner's registration only after the Docker image store is attached, which can
# take minutes, and a five-minute token expired in that wait on 2026-10-03: every
# later read failed, the agent exited with "no command in the metadata", and the
# runner never connected (#320).
for attempt in $(seq 1 120); do
	if token=$(curl -sf --connect-timeout 2 --max-time 5 -X PUT "http://$MMDS/latest/api/token" \
		-H "X-metadata-token-ttl-seconds: 21600" 2>/dev/null); then
		break
	fi

	if [ "$attempt" -ge 120 ]; then
		log "the metadata service never answered"
		exit 1
	fi

	sleep 0.5
done

# A READ THAT GOT NO ANSWER IS ASKED AGAIN; AN ANSWER IS FINAL. On a loaded host the
# service can miss a five-second window, and on 2026-10-03 one missed read of the
# contract made a guest refuse as "older than this image": the agent exited, the
# runner never connected, and GitHub showed it offline. A timeout or a refused
# connection says nothing about the key, so the read is repeated for fetch_within
# seconds; an HTTP answer is the service's word and is never retried, except a 200
# whose body did not arrive whole. fetch returns 0 with the value, 3 when the service
# says the key does not exist, and 1 when it could not be read, so a caller can tell
# "billet did not send it" from "could not ask".
#
# ONE BUDGET FOR THE AGENT, NOT ONE PER KEY. A read that spends it leaves
# fetch_gave_up behind, and every later read is asked once until the service answers
# again; otherwise each of the half-dozen keys read after it would wait its own five
# minutes on a service that is gone. It is a file because fetch runs in a subshell,
# and it is removed here because a restarted agent is owed a whole budget again.
fetch_within=300
fetch_gave_up=/run/billet-metadata-unanswered
rm -f "$fetch_gave_up"

# BILLET_AGENT_FETCH_BEGIN
fetch() {
	local answer code got deadline
	deadline=$((SECONDS + fetch_within))
	if [ -e "$fetch_gave_up" ]; then
		deadline=$SECONDS
	fi
	while :; do
		got=0
		answer=$(curl -s --connect-timeout 2 --max-time 5 -w '\n%{http_code}' \
			-H "X-metadata-token: $token" "http://$MMDS/latest/meta-data/billet/$1") || got=$?
		code=${answer##*$'\n'}
		if [ "$code" = 200 ] && [ "$got" -eq 0 ]; then
			rm -f "$fetch_gave_up"
			printf '%s' "${answer%$'\n'*}"
			return 0
		fi
		if [ "$code" = 404 ]; then
			rm -f "$fetch_gave_up"
			return 3
		fi
		if [[ "$code" =~ ^[1-9][0-9][0-9]$ ]] && [ "$code" != 200 ]; then
			rm -f "$fetch_gave_up"
			log "the metadata service answered $code for $1"
			return 1
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			: >"$fetch_gave_up"
			log "could not read $1 from the metadata service in ${fetch_within}s"
			return 1
		fi
		sleep 1
	done
}
# BILLET_AGENT_FETCH_END

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

contract_read=0
contract=$(fetch contract) || contract_read=$?

if [ "$contract_read" -eq 3 ]; then
	log "this billet did not say which metadata contract it speaks; it is older than this image"
	exit 1
fi

if [ "$contract_read" -ne 0 ]; then
	log "could not read the metadata contract; the guest cannot start a runner without it"
	exit 1
fi

if [ "$contract" != "$WANT_CONTRACT" ]; then
	log "billet speaks metadata contract $contract and this image understands $WANT_CONTRACT"
	log "rebuild and republish the guest image with scripts/build-guest-image.sh"
	exit 1
fi

jit_read=0
jit=$(fetch jit-config) || jit_read=$?

if [ "$jit_read" -eq 3 ]; then
	log "no registration in the metadata"
	exit 1
fi

if [ "$jit_read" -ne 0 ]; then
	log "could not read the registration from the metadata"
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

# THE BUILD CACHES THE TIER ENABLES, a comma-separated list the node derives from
# the tier's cache block. Absent on an older node, and meaningless without a cache
# session, so either leaves every build tool as the image ships it.
guest_caches=""
if [ -n "$cache_endpoint" ]; then
	guest_caches=$(fetch guest-caches 2>/dev/null) || guest_caches=""
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
# ONE BUNDLE, EVERY VARIABLE A CLIENT HONOURS. Node reads NODE_EXTRA_CA_CERTS;
# OpenSSL, Go, rustls-native-certs and curl read SSL_CERT_FILE; Python's
# requests reads certifi's bundle and IGNORES the system store unless
# REQUESTS_CA_BUNDLE says otherwise, and curl prefers CURL_CA_BUNDLE where it is
# set. The bundle carries the distribution roots too, so none of these replaces
# the trust a client had with the trust it needs.
{
	printf 'NODE_EXTRA_CA_CERTS=%s\n' "$target"
	printf 'SSL_CERT_FILE=%s\n' "$target"
	printf 'REQUESTS_CA_BUNDLE=%s\n' "$target"
	printf 'CURL_CA_BUNDLE=%s\n' "$target"
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

# THE SHIM'S DIRECTORY GOES FIRST ON EVERY STEP'S PATH, on the host and inside a
# job container (where the container hook has bind-mounted it), so a build run
# by whatever docker client an image carries is fronted by the shim.
# GUARDED like the URL above: the runner sets GITHUB_PATH for a job hook, and a
# harness that runs this script without one must not turn that into a failure.
if [ -n "${GITHUB_PATH:-}" ]; then
	printf '%s\n' /opt/billet/bin >>"$GITHUB_PATH"
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
	# docs/operating/actions-cache.md.)
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
	# THE SYSTEM INTERPRETER, NEVER ONE FROM THE TOOLCACHE. A job may free disk by
	# deleting $AGENT_TOOLSDIRECTORY, as the guest-image build itself does and as
	# the common disk-reclaiming actions do; with the passthrough running from a
	# toolcache Python, its next restart or lazy import failed, the remapped origin
	# refused every connection, and the job's artifact upload failed with
	# ECONNREFUSED after a two-hour build (2026-10-04).
	python_runtime=""
	if [ -x /usr/bin/python3 ]; then
		python_runtime=/usr/bin/python3
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
# THE BUILD CACHES REACH THE NODE THROUGH A RELAY THAT WAITS OUT A NODE RESTART
# (#374). A node handing over leaves this guest running while its cache listener
# is gone until the next process starts, and a `git fetch` sent to the node's own
# address in that gap is refused and fails the job: nothing in Git falls back.
# The relay listens on the docker gateway, where the job and its containers can
# both reach it, and splices each connection to the node, redialling while the
# node is away. Everything that names the cache after this point (the runner's
# BILLET_CACHE_ENDPOINT, the git rewrite and its credential helper, bazelrc, the
# Go helper) names the relay. The Docker image store was attached above, before
# Docker, and keeps the node's own address. Only a plain-HTTP endpoint: an https
# one names the node in its certificate, so it is left direct, and so is any
# endpoint whose relay does not start.
cache_relay_port=41322
# BILLET_CACHE_RELAY_BEGIN
if [ -n "$cache_endpoint" ] && [ "${cache_endpoint%%://*}" = http ]; then
	if ! relay_bridge=$(ip -4 -o addr show docker0 2>/dev/null |
		awk 'NR == 1 {split($4, a, "/"); print a[1]}'); then
		relay_bridge=""
	fi
	if [ -x /usr/bin/python3 ] && [ "$relay_bridge" = "$docker_gateway" ] &&
		systemd-run --quiet --unit=billet-cache-relay --collect --uid=runner --gid=runner \
			--property=Type=notify --property=NotifyAccess=main --property=TimeoutStartSec=5s \
			--property=Restart=always --property=RestartSec=100ms \
			--socket-property=ListenStream="$docker_gateway:$cache_relay_port" \
			--socket-property=Accept=no --socket-property=FlushPending=no \
			/usr/bin/python3 /usr/local/bin/billet-actions-proxy \
			--mode node-relay --systemd-socket --upstream "${cache_endpoint%/}" &&
		systemctl start billet-cache-relay.service 2>/dev/null; then
		cache_endpoint="http://$docker_gateway:$cache_relay_port"
	else
		log "the cache relay did not start; the build caches name the node directly and a node restart during this job fails a git fetch that lands in it"
		systemctl stop billet-cache-relay.socket billet-cache-relay.service 2>/dev/null || true
	fi
fi
# BILLET_CACHE_RELAY_END

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
	# A UTF-8 LOCALE, as GitHub's Ubuntu runners export: without one the job runs in
	# C, and tools that normalise or print Unicode fail or mangle it.
	"LANG=C.UTF-8"
	"RUNNER_TOOL_CACHE=/opt/hostedtoolcache"
	"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache"
)

# WHAT THE IMAGE SAYS IT IS, added to what the job will see.
#
# THIS ARRAY IS THE JOB'S WHOLE ENVIRONMENT -- the launch starts from `env -i`, so
# a variable absent here does not exist for the job whatever /etc/environment says.
# The build writes /etc/billet-image-env with the values a hosted runner exports
# (ImageOS and friends), and they have to be read in HERE to reach a job at all.
#
# READ AS DATA, NOT SOURCED. `source` on a file would execute it, and while this
# one is written by the build, a shell that executes a config file is a shape
# worth not having. Only lines whose name is a variable's are taken, and any other
# line is skipped rather than ending the launch: an image that lost this file
# should run jobs that download their own toolchains, not refuse to run jobs at
# all, and billet-exec-env refuses a name that is not a variable's.
# THE PATH IS A DEFAULTED VARIABLE so a test can drive this exact code against a
# fixture. Grepping the agent for the assignment proves only that the text is
# present, which is satisfied by dead code -- the seam is what lets the block be
# EXECUTED and its effect on the array observed.
IMAGE_ENV_FILE="${IMAGE_ENV_FILE:-/etc/billet-image-env}"

if [ -r "$IMAGE_ENV_FILE" ]; then
	while IFS= read -r line; do
		if [[ "$line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] && [[ "$line" != OPTIND=* ]]; then
			runner_env+=("$line")
		fi
	done <"$IMAGE_ENV_FILE"
fi
# BILLET_CACHE_ENV_BEGIN
if [ -n "$cache_endpoint" ] && [ -n "$cache_token" ]; then
	runner_env+=("BILLET_CACHE_ENDPOINT=$cache_endpoint" "BILLET_CACHE_TOKEN=$cache_token"
		"BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES=$buildkit_cache_mount_limit_bytes")
fi
# BILLET_CACHE_ENV_END
# THE BUILD CACHES, each configured only when the node offered it AND this image
# carries billet to serve it; an image without the binary builds cold rather than
# naming a GOCACHEPROG the go command cannot start, which fails every build. The
# seams are defaulted variables so a test executes this block against fixtures.
GUEST_BILLET="${GUEST_BILLET:-/opt/billet/bin/billet}"
BAZELRC_FILE="${BAZELRC_FILE:-/etc/bazel.bazelrc}"
GITCONFIG_FILE="${GITCONFIG_FILE:-/etc/gitconfig}"
# BILLET_GUEST_CACHES_BEGIN
guest_go=""
guest_go_tests=""
guest_bazel=""
guest_git=""
if [ -n "$cache_endpoint" ] && [ -n "$cache_token" ] && [ -x "$GUEST_BILLET" ]; then
	IFS=, read -r -a requested_caches <<<"$guest_caches"
	for cache in ${requested_caches[@]+"${requested_caches[@]}"}; do
		case "$cache" in
			go) guest_go=1 ;;
			go-test-results) guest_go_tests=1 ;;
			bazel) guest_bazel=1 ;;
			git) guest_git=1 ;;
			*) log "this image does not know the guest cache \"$cache\"; ignoring it" ;;
		esac
	done
fi
if [ -n "$guest_go" ]; then
	# THE ONLY WAY OUT IS GOCACHEPROG= (EMPTY) IN A WORKFLOW: the go command
	# prefers GOCACHEPROG to GOCACHE, so setting GOCACHE alone changes nothing.
	runner_env+=("GOCACHEPROG=$GUEST_BILLET cache gocacheprog")
	# TEST RESULTS ARE SHARED ONLY WHEN THE TIER SAYS SO. Without it every go
	# test runs; GOFLAGS applies -count=1 to the commands that know it and no
	# other.
	if [ -z "$guest_go_tests" ]; then
		runner_env+=("GOFLAGS=-count=1")
	fi
fi
if [ -n "$guest_bazel" ]; then
	# THE BEARER IS NOT IN THIS FILE. Bazel asks the credential helper, which
	# answers from the runner's environment and only for the node's own host.
	bazel_host=${cache_endpoint#*://}
	bazel_host=${bazel_host%%/*}
	bazel_host=${bazel_host%%:*}
	{
		printf '%s\n' "build --remote_cache=${cache_endpoint%/}/v1/cas/bazel"
		printf '%s\n' "build --credential_helper=$bazel_host=${GUEST_BILLET%/*}/bazel-credential-helper"
	} >>"$BAZELRC_FILE"
fi
if [ -n "$guest_git" ]; then
	# FETCHES FROM github.com GO THROUGH THE NODE; PUSHES DO NOT. The rewrite
	# drops the header actions/checkout scopes to github.com, so the credential
	# helper hands it back to the node, which asks GitHub before serving a byte.
	# Only https://github.com/ is rewritten: SSH and every other host are
	# untouched.
	git_origin=${cache_endpoint%/}
	{
		printf '%s\n' "[url \"$git_origin/v1/git/github.com/\"]"
		printf '\t%s\n' "insteadOf = https://github.com/"
		printf '%s\n' '[url "https://github.com/"]'
		printf '\t%s\n' "pushInsteadOf = https://github.com/"
		printf '%s\n' "[credential \"$git_origin\"]"
		printf '\t%s\n' "helper = $GUEST_BILLET cache git-credential"
	} >>"$GITCONFIG_FILE"
fi
# BILLET_GUEST_CACHES_END
if [ -n "$actions_cache_active" ] && [ -n "$actions_ca_path" ] && [ -n "$actions_hook_path" ]; then
	# NO HTTPS_PROXY. Interception reaches the runner by a DNS remap of the one
	# results origin, so only that host's traffic is redirected and every other
	# request -- action downloads, toolchains, artifact blob uploads -- resolves
	# normally and goes direct. The runner still needs the node's CA to trust the
	# intercepted origin, and the job-started hook publishes it into containers.
	runner_env+=("NODE_EXTRA_CA_CERTS=$actions_ca_path" "SSL_CERT_FILE=$actions_ca_path"
		"BILLET_ACTIONS_CA_SOURCE=$actions_ca_path"
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=$actions_hook_path")
	# THE DOCKER SHIM GOES INTO JOB CONTAINERS ONLY WHERE INTERCEPTION IS: a tier
	# without it has nothing for the shim to point at.
	runner_env+=("BILLET_CONTAINER_SHIM=1")
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
# THE CONTAINER HOOK, ONLY WHERE IT HAS SOMETHING TO ADD: the docker shim under
# interception, the Go cache helper under the go cache. Every other tier keeps
# the runner's built-in docker path; this one runs GitHub's reference of it plus
# those mounts.
if { [ -n "$actions_cache_active" ] && [ -n "$actions_ca_path" ] && [ -n "$actions_hook_path" ]; } ||
	[ -n "$guest_go" ]; then
	runner_env+=("ACTIONS_RUNNER_CONTAINER_HOOKS=/usr/local/lib/billet/container-hook/index.js")
fi

# THE ENVIRONMENT CROSSES ON DESCRIPTOR 3, NEVER IN AN ARGUMENT LIST (#352). It
# carries the registration and the cache bearer, and setpriv's and env's argv are
# readable by every process in the guest until each execs the next. The here-
# document reaches billet-exec-env through a pipe or an unlinked file, and printf
# is a builtin, so no process on the way to the runner carries a value in argv.
# A value holding a newline cannot travel one line per variable, so it refuses
# the launch, naming the variable and never the value, and the steps below run
# as for a failed job. The path is a defaulted variable so a test executes this
# stanza against the real helper.
EXEC_ENV="${EXEC_ENV:-/usr/local/bin/billet-exec-env}"
set +e
# BILLET_AGENT_LAUNCH_BEGIN
launch_refused=""
for entry in "${runner_env[@]}"; do
	if [[ "$entry" == *$'\n'* ]]; then
		log "${entry%%=*} holds a newline, which cannot reach the runner; refusing the launch"
		launch_refused=1
	fi
done
# SERIALIZED AND CHECKED BEFORE THE LAUNCH, so the terminator the here-document
# appends can never follow a stream that was cut short.
if [ -z "$launch_refused" ] && ! runner_env_stream=$(printf '%s\n' "${runner_env[@]}"); then
	log "the runner's environment could not be written out; refusing the launch"
	launch_refused=1
fi
if [ -n "$launch_refused" ]; then
	job_status=1
else
	setpriv --reuid=runner --regid=runner --init-groups --inh-caps=-all -- \
		env -i "$EXEC_ENV" "${cmd[@]}" 3<<BILLET_RUNNER_ENV
$runner_env_stream
end
BILLET_RUNNER_ENV
	job_status=$?
fi
# BILLET_AGENT_LAUNCH_END
set -e

# Stop the socket too, or a late connection would re-activate the service.
systemctl stop billet-actions-proxy.socket billet-actions-proxy.service 2>/dev/null || true
systemctl stop billet-actions-cache-adapter.socket \
	billet-actions-cache-adapter.service 2>/dev/null || true
systemctl stop billet-cache-relay.socket billet-cache-relay.service 2>/dev/null || true

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
# DUID derived from /etc/machine-id, and every clone of one image starts from the
# same filesystem -- so with a DUID two guests can present the same client
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
		grep -q "^RUNNER_TOOL_CACHE=" /etc/environment ||
			printf "RUNNER_TOOL_CACHE=/opt/hostedtoolcache\nAGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n" >>/etc/environment
		echo billet-guest >/etc/hostname
		printf "127.0.0.1 localhost\n127.0.1.1 billet-guest\n::1     localhost ip6-localhost ip6-loopback\n" >/etc/hosts
		# ROOT CANNOT LOG IN. Nothing should be logging into a guest that exists for
		# one job, and an account with no password is not the same as a locked one.
		passwd -l root
	'

	# THE RUNNER'S HOME IS HANDED TO IT LAST, after every step that installs into
	# the image. HOME=/root alone was not enough: a build with it still left
	# /home/runner/.config/NuGet to root (2026-09-21, from v0.12.3; the gate
	# refused it), written during the toolcache step. Whatever writes there,
	# nothing after this does, and check-guest-image.sh proves it.
	chroot "$rootfs" chown -R runner:runner /home/runner

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

	# CHECKED AFTER THE LAST UNMOUNT, which is what makes the image growable. A node
	# grows every clone with resize2fs before boot, and resize2fs refuses a
	# filesystem whose last check is older than its last mount: mkfs stamps the
	# check time, the build mounts it a moment later, and when that moment crossed
	# a second boundary every launch failed with "Please run 'e2fsck -f' first"
	# (2026-09-26). A forced check stamps a newer time and proves the filesystem.
	# e2fsck exits 0 when clean and 1 when it corrected something; 4 and above
	# mean it could not, and that image must not be published.
	local fsck_status=0
	e2fsck -f -y "$img" >&2 || fsck_status=$?
	if [ "$fsck_status" -ge 4 ]; then
		echo "e2fsck could not make $img consistent (exit $fsck_status); refusing to publish it" >&2
		exit 1
	fi

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
	# bootstrap` on this host. See docs/reference/decisions/adr-003-ceph-rbd.md.
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
