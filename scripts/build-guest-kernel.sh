#!/usr/bin/env bash
#
# Build the guest kernel a Firecracker microVM boots.
#
# RESCUED INTO THE REPOSITORY, and the reason is the point: this recipe existed only
# as a file in /tmp on one machine. The kernel every guest boots is 47MB of build
# output whose inputs were a version number, a base config and a list of options
# somebody typed once -- and one reboot of that machine would have made it
# unreproducible. A build artifact nobody can rebuild is a machine you cannot replace.
#
# IT IS A MATCHED PAIR WITH THE ROOTFS. scripts/build-guest-image.sh installs Docker
# into a userspace that expects overlayfs, the full netfilter set and cgroup v2 to be
# in the KERNEL -- and a microVM has no initramfs, so anything built as a module is
# the same as absent. Changing one of these two without the other produces a guest
# that boots and then fails in the middle of somebody's job.
#
# THE BASE IS FIRECRACKER'S OWN CI CONFIG, because it is known to boot under the VMM,
# plus everything moby's check-config.sh reports missing. That check is the test: the
# build ends by running scripts/check-guest-kernel-config.sh, which requires every
# option in scripts/kernel/required-builtins.txt to be =y and requires the vendored
# checker to EXIT ZERO -- not to print a particular sentence. Firecracker's CI kernel
# alone is NOT adequate -- measured, it was missing IP_NF_RAW, IP6_NF_RAW, XT_MARK,
# NET_CLS_CGROUP and the nftables modules, which surfaces as service containers
# failing strangely rather than as a boot failure.
#
# Usage: KERNEL_VERSION=6.1.155 OUT=/srv/billet sudo -E bash scripts/build-guest-kernel.sh

# WHERE IT WORKS AND WHERE IT LANDS, both overridable, because the original hard-coded
# one developer's home directory -- which is half of why it was never in the repo.
# RESOLVED BEFORE ANYTHING CHANGES DIRECTORY. ${BASH_SOURCE[0]} is the path as it
# was invoked, which is usually relative -- and this script cds into the work
# directory and then into the kernel tree, so resolving it later finds nothing.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

WORK="${WORK:-/var/tmp/billet-kernel}"
OUT="${OUT:-$WORK}"

mkdir -p "$WORK" "$OUT"
# Build the guest kernel billet ships: Firecracker's CI config (known to boot under
# the VMM) plus everything moby's check-config.sh says Docker needs.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq build-essential flex bison libelf-dev libssl-dev bc dwarves cpio >/dev/null

cd "$WORK"
V="${KERNEL_VERSION:-6.1.155}"

# THE CHECKSUM OF THE KERNEL THIS BUILDS, taken from kernel.org's sha256sums.asc.
#
# PINNED RATHER THAN FETCHED ALONGSIDE THE TARBALL, because a checksum downloaded
# over the same connection from the same host proves only that the two agree. This
# is the kernel every guest boots, so it is worth a line in the repository.
KERNEL_SHA256="${KERNEL_SHA256:-}"

if [ -z "$KERNEL_SHA256" ]; then
  case "$V" in
    6.1.155) KERNEL_SHA256=c29387aeee085fbcbd91236224b9df805063bac43615e75cea2c6b29604a5c73 ;;
    *)
      echo "no pinned checksum for kernel $V. Take it from" >&2
      echo "https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc and either add it" >&2
      echo "here or pass KERNEL_SHA256; this refuses to build an unverified kernel." >&2
      exit 1
      ;;
  esac
fi

[ -d "linux-$V" ] || {
  # MEASURED FAILURE, AND WHY EACH FLAG IS HERE. The first CI run of this died six
  # minutes in with `curl: (92) HTTP/2 stream 1 was not closed cleanly:
  # PROTOCOL_ERROR` -- a transient fault partway through a 140MB transfer that took
  # the whole build with it, because the original invocation had no retries at all.
  #
  # --http1.1 because that error IS an HTTP/2 framing fault; there is nothing to
  #   gain from multiplexing one large download and it removes the whole class.
  # --retry-all-errors because plain --retry covers http statuses and timeouts but
  #   NOT curl-level transport faults like 92, so it would not have retried this.
  # -f so an error page is a failure rather than a few hundred bytes of html saved
  #   under the name of a kernel tarball, which tar then reports as a corrupt
  #   archive -- an error nobody connects to the http status nobody printed.
  # -L because kernel.org redirects to its CDN.
  curl -fL -sS --http1.1 \
    --connect-timeout 20 --max-time 900 \
    --retry 5 --retry-delay 5 --retry-all-errors \
    -o "linux-$V.tar.xz" \
    "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$V.tar.xz"

  # CHECKED BEFORE IT IS UNPACKED. A truncated download that happens to end on a
  # frame boundary unpacks far enough to start a build, and the build then fails
  # somewhere deep in the tree with an error about a missing header.
  echo "$KERNEL_SHA256  linux-$V.tar.xz" | sha256sum -c -

  tar xf "linux-$V.tar.xz"
}
cd "linux-$V"

# THE BASE CONFIG COMES FROM THE REPOSITORY, not from the work directory.
#
# THIS IS THE GAP THAT MADE THE SCRIPT UNRUNNABLE ANYWHERE BUT ONE MACHINE. It was
# rescued from /tmp on the host it was written on, where a base config had been put
# in place by hand months earlier -- so the line that read it worked there and
# nowhere else. The first CI run got as far as `cp: cannot stat
# '/var/tmp/billet-kernel/vmlinux-6.1.155.config'`, which is the whole story.
#
# The committed config is the RESULT of a previous run of this script, which is
# exactly what makes it the right base: it already carries Firecracker's CI config
# plus every option added below, so `make olddefconfig` over it is a no-op and the
# `--enable` lines that follow are idempotent. They stay because they document what
# this kernel needs and why, and because they are what regenerates the config from
# a bare upstream one when the version moves.
#
# BASE_CONFIG is overridable for exactly that case: pointing it at a fresh
# Firecracker CI config is how a version bump is done.
BASE_CONFIG="${BASE_CONFIG:-$REPO_ROOT/scripts/guest-kernel.config}"

if [ ! -r "$BASE_CONFIG" ]; then
  echo "cannot read the base kernel config at $BASE_CONFIG." >&2
  echo "Set BASE_CONFIG to a config to start from -- for a version bump that is" >&2
  echo "Firecracker's CI config for the new version." >&2
  exit 1
fi

echo "base config: $BASE_CONFIG"
cp "$BASE_CONFIG" .config

# EVERYTHING check-config.sh REPORTED MISSING, plus the rest of what a CI job needs.
# Built IN rather than as modules: a microVM boots one kernel and has no initramfs to
# load modules from, so `=m` is the same as absent here.
#
# THE LIST IS A FILE BECAUSE TWO THINGS READ IT. It used to be written out here, and
# the gate that checks the finished kernel would then have needed its own copy -- two
# lists in two places, where an option dropped from one is still "checked" by the
# other. scripts/check-guest-kernel-config.sh requires exactly what this enables.
#
# `|| true` STAYS: an option a kernel version has renamed or removed should surface
# as the gate's message naming it, after the build, rather than as `scripts/config`
# exiting non-zero with nothing said about which option or why it matters.
REQUIRED_BUILTINS="${REQUIRED_BUILTINS:-$REPO_ROOT/scripts/kernel/required-builtins.txt}"

if [ ! -r "$REQUIRED_BUILTINS" ]; then
  echo "cannot read $REQUIRED_BUILTINS, which lists what this kernel must carry." >&2
  echo "Run this from a checkout; the list is part of the repository." >&2
  exit 1
fi

while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    '' | '#'*) continue ;;
  esac

  opt=$(printf '%s' "$line" | tr -d '[:space:]')
  [ -n "$opt" ] || continue

  ./scripts/config --enable "$opt" || true
done <"$REQUIRED_BUILTINS"

# APPARMOR: Ubuntu's userspace expects it, and Docker's default profile needs it.
./scripts/config --enable SECURITY_APPARMOR
./scripts/config --set-str LSM "lockdown,yama,apparmor,bpf"
./scripts/config --enable DEFAULT_SECURITY_APPARMOR || true

make olddefconfig >/dev/null
echo "=== building on $(nproc) cores ==="

# THE LOG IS SHOWN WHEN THE BUILD FAILS. Redirecting it to a file and nothing else
# meant a failed kernel build printed one line -- the exit status -- and the
# thousands of lines saying why were left in a file on a runner that is about to be
# destroyed. Unreadable exactly when it is needed.
if ! time make -j"$(nproc)" vmlinux >/tmp/kbuild.log 2>&1; then
  echo "the kernel build failed; the last 60 lines of the log follow" >&2
  tail -60 /tmp/kbuild.log >&2

  exit 1
fi

ls -la vmlinux
cp vmlinux "$OUT/vmlinux-billet"
cp .config "$OUT/vmlinux-billet.config"

# THE CHECK IS THE TEST, and it is the reason the option list above exists:
# Firecracker's CI kernel alone was missing IP_NF_RAW, IP6_NF_RAW, XT_MARK,
# NET_CLS_CGROUP and the nftables modules, which surfaces as service containers
# failing strangely rather than as a boot failure.
#
# FATAL, AND THE CHECKER IS IN THE REPOSITORY. Both halves of that sentence were
# wrong before. The checker was expected to be a file somebody had happened to drop
# beside the work directory, which no release run ever did -- so every published
# kernel printed "skipped" and shipped -- and the branch that would have run it
# ended in `| grep -Ei missing | head -30 || echo "NOTHING MISSING"`, which under
# pipefail turns the CHECKER's failure into the reassuring sentence and exits 0.
# Measured with CONFIG_VETH removed: fifteen "missing" lines, then NOTHING MISSING,
# then a successful build.
#
# THE GATE IS A SCRIPT OF ITS OWN because the workflow has to run it on a cache HIT
# as well, where this script never executes -- actions/cache saves in its post step
# even when a later step failed, so a kernel that failed here can come back later
# and skip the check entirely if the only gate is the one inside the build.
#
# THE CONFIG IS NAMED BY ITS REAL PATH. This used to cd to $WORK and name the file
# relatively, which worked only when OUT and WORK were the same directory -- the
# default. In CI they differ, and the check read a file that was not there.
echo "=== check-config against the built kernel ==="

"$REPO_ROOT/scripts/check-guest-kernel-config.sh" "$OUT/vmlinux-billet.config"

echo "=== BUILD DONE ==="
