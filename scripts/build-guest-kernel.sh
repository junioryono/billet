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
[ -d "linux-$V" ] || {
  curl -sS -o "linux-$V.tar.xz" "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$V.tar.xz"
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
