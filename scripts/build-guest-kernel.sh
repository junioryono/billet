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
# build ends by running it, and "NOTHING MISSING" is the pass condition. Firecracker's
# CI kernel alone is NOT adequate -- measured, it was missing IP_NF_RAW, IP6_NF_RAW,
# XT_MARK, NET_CLS_CGROUP and the nftables modules, which surfaces as service
# containers failing strangely rather than as a boot failure.
#
# Usage: KERNEL_VERSION=6.1.155 OUT=/srv/billet sudo -E bash scripts/build-guest-kernel.sh

# WHERE IT WORKS AND WHERE IT LANDS, both overridable, because the original hard-coded
# one developer's home directory -- which is half of why it was never in the repo.
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
cp "$WORK/vmlinux-$V.config" .config

# EVERYTHING check-config.sh REPORTED MISSING, plus the rest of what a CI job needs.
# Built IN rather than as modules: a microVM boots one kernel and has no initramfs to
# load modules from, so `=m` is the same as absent here.
for opt in \
  IP_NF_RAW IP6_NF_RAW NETFILTER_XT_MARK NET_CLS_CGROUP \
  NF_TABLES NF_TABLES_INET NFT_CT NFT_FIB_IPV4 NFT_FIB_IPV6 NFT_MASQ NFT_NAT \
  NFT_REJECT NFT_COMPAT NETFILTER_XT_MATCH_IPVS \
  IP_SCTP IP_VS IP_VS_NFCT IP_VS_PROTO_TCP IP_VS_PROTO_UDP IP_VS_RR \
  OVERLAY_FS EXT4_FS XFS_FS BTRFS_FS \
  VETH BRIDGE BRIDGE_NETFILTER MACVLAN IPVLAN DUMMY \
  NAMESPACES NET_NS PID_NS IPC_NS UTS_NS USER_NS CGROUPS CGROUP_CPUACCT \
  CGROUP_DEVICE CGROUP_FREEZER CGROUP_SCHED CGROUP_PIDS CGROUP_BPF \
  CPUSETS MEMCG BLK_CGROUP BLK_DEV_THROTTLING \
  KEYS POSIX_MQUEUE SECCOMP SECCOMP_FILTER \
  BPF_SYSCALL CGROUP_NET_PRIO FAIR_GROUP_SCHED CFS_BANDWIDTH \
  NETFILTER_XT_MATCH_ADDRTYPE NETFILTER_XT_MATCH_CONNTRACK \
  NETFILTER_XT_MATCH_COMMENT NETFILTER_XT_TARGET_REDIRECT \
  IP_NF_NAT IP6_NF_NAT NF_NAT NF_CONNTRACK \
  VIRTIO VIRTIO_MMIO VIRTIO_BLK VIRTIO_NET VIRTIO_PCI_LEGACY \
  ; do
  ./scripts/config --enable "$opt" || true
done

# APPARMOR: Ubuntu's userspace expects it, and Docker's default profile needs it.
./scripts/config --enable SECURITY_APPARMOR
./scripts/config --set-str LSM "lockdown,yama,apparmor,bpf"
./scripts/config --enable DEFAULT_SECURITY_APPARMOR || true

make olddefconfig >/dev/null
echo "=== building on $(nproc) cores ==="
time make -j"$(nproc)" vmlinux >/tmp/kbuild.log 2>&1
ls -la vmlinux
cp vmlinux "$OUT/vmlinux-billet"
cp .config "$OUT/vmlinux-billet.config"
echo "=== check-config against the built kernel ==="
cd "$WORK"
./check-config.sh vmlinux-billet.config 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | grep -Ei "missing" | head -30 || echo "NOTHING MISSING"
echo "=== BUILD DONE ==="
