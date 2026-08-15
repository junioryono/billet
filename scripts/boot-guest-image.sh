#!/usr/bin/env bash
#
# Boot a built guest image under Firecracker and prove it came up.
#
# WHAT THIS CATCHES THAT check-guest-image.sh CANNOT. That one reads the
# filesystem: it can tell you the agent is installed and the units are enabled. It
# cannot tell you the kernel boots the userspace it was built for, that systemd
# reaches its target, that the network comes up, or that the guest can reach the
# metadata service. Those are integration failures, and they are the ones that
# produce a microVM which starts and then does nothing.
#
# HOW IT TESTS THE AGENT WITHOUT ANY CREDENTIALS. The obvious approach — boot it,
# let it register — needs a control plane and a GitHub App, neither of which exists
# in CI. So MMDS is served a contract the agent is REQUIRED to refuse. The agent
# fetches it, compares it against its own, declines, and says so on the console.
# That single refusal proves the entire chain: the kernel booted, systemd started,
# the network configured, the route to 169.254.169.254 works, MMDS answered, and
# the agent ran and parsed what it got. A pass is a specific sentence, not the
# absence of a crash.
#
# FIRECRACKER EXITS 0 ON SOME GUEST-SIDE FAILURES, so the exit status is not the
# test. The console is.
set -euo pipefail

KERNEL="${KERNEL:?KERNEL must name the guest kernel}"
ROOTFS="${ROOTFS:?ROOTFS must name the raw ext4 image}"

FIRECRACKER="${FIRECRACKER:-firecracker}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-90}"
VCPUS="${VCPUS:-2}"
MEM_MIB="${MEM_MIB:-1024}"

# A CONTRACT NO BILLET SPEAKS. The agent compares for equality and refuses
# anything it does not recognise, so this is guaranteed to be refused by every
# version of the image -- which is what makes the expected output stable.
REFUSED_CONTRACT="${REFUSED_CONTRACT:-999}"

if [ "$(id -u)" -ne 0 ]; then
	echo "this creates a tap device and opens /dev/kvm; run it as root" >&2
	exit 2
fi

# ASSERTED, LOUDLY, RATHER THAN DISCOVERED AS A CONFUSING FAILURE. Firecracker has
# no software-emulation fallback: without KVM it does not run slowly, it does not
# run. KVM is present on x86-64 GitHub-hosted runners but GitHub documents it only
# as an Android-emulator feature, never as a general guarantee -- so this is an
# undocumented capability that could regress, and when it does the message should
# say so rather than blaming the image.
if [ ! -c /dev/kvm ]; then
	echo "/dev/kvm is not present on this machine, and firecracker has no emulation" >&2
	echo "fallback. This is not a problem with the image." >&2
	exit 2
fi

WORK=$(mktemp -d)
TAP="fcboot$$"
CONSOLE="$WORK/console.log"
SOCK="$WORK/fc.sock"
FCPID=""

cleanup() {
	if [ -n "$FCPID" ] && kill -0 "$FCPID" 2>/dev/null; then
		kill -9 "$FCPID" 2>/dev/null || true
		wait "$FCPID" 2>/dev/null || true
	fi

	if [ -r "$WORK/dnsmasq.pid" ]; then
		kill "$(cat "$WORK/dnsmasq.pid")" 2>/dev/null || true
	fi

	ip link del "$TAP" 2>/dev/null || true
	rm -rf "$WORK"
}

trap cleanup EXIT

# A COPY, BECAUSE THE GUEST WRITES TO ITS ROOT DISK. Booting the artifact directly
# would modify it -- and the digest in the manifest was computed over the bytes as
# published, so a boot test that mutated them would invalidate the very thing it
# was verifying.
echo "copying the image so the boot cannot modify the artifact"
cp --reflink=auto "$ROOTFS" "$WORK/rootfs.ext4"

ip tuntap add "$TAP" mode tap
ip addr add 172.31.99.1/24 dev "$TAP"
ip link set "$TAP" up

# A DHCP SERVER, BECAUSE THE GUEST EXPECTS ONE. /etc/systemd/network/10-eth0.network
# sets DHCP=yes -- deliberately, since in production the address belongs to the
# bridge rather than to billet -- so a tap with no server leaves eth0 with no
# address at all. The link-local route to the metadata service is then unusable:
# the kernel has no source address to send from, MMDS never answers, and the agent
# times out after sixty seconds reporting that the metadata service never answered.
#
# That failure looks exactly like a broken image and is not one, which is why this
# serves DHCP rather than working around it. It also makes the test faithful: this
# is the same shape the guest meets on a real bridge.
#
# --port=0 DISABLES DNS. Without it dnsmasq binds 53 and collides with the
# resolver already running on a github runner, and the collision is reported as a
# dnsmasq failure that reads like a problem with this test.
if ! command -v dnsmasq >/dev/null 2>&1; then
	echo "dnsmasq is not installed, and the guest gets its address by dhcp" >&2
	exit 2
fi

dnsmasq \
	--interface="$TAP" --bind-interfaces \
	--dhcp-range=172.31.99.10,172.31.99.20,1h \
	--port=0 \
	--pid-file="$WORK/dnsmasq.pid" \
	--log-facility="$WORK/dnsmasq.log"

"$FIRECRACKER" --api-sock "$SOCK" >"$CONSOLE" 2>&1 &
FCPID=$!

# WAIT FOR THE SOCKET RATHER THAN SLEEPING. A fixed sleep is either too short on a
# loaded runner or wasted time on an idle one, and the failure mode of too-short is
# a confusing connection refused that reads like firecracker crashed.
for _ in $(seq 1 100); do
	[ -S "$SOCK" ] && break
	sleep 0.1
done

if [ ! -S "$SOCK" ]; then
	echo "firecracker never created its api socket; its output follows" >&2
	cat "$CONSOLE" >&2
	exit 1
fi

api() {
	local method="$1" path="$2" body="${3:-}"

	if [ -n "$body" ]; then
		curl -sS --unix-socket "$SOCK" -X "$method" "http://localhost$path" \
			-H "Content-Type: application/json" -d "$body"
	else
		curl -sS --unix-socket "$SOCK" -X "$method" "http://localhost$path"
	fi
}

api PUT /machine-config "{\"vcpu_count\":$VCPUS,\"mem_size_mib\":$MEM_MIB}"

# console=ttyS0 IS THE WHOLE POINT: the console is the only channel this test
# reads. reboot=k and panic=1 make a panicking guest exit rather than hang until
# the timeout, which turns a crash into a fast, legible failure.
api PUT /boot-source "$(jq -n --arg k "$KERNEL" \
	'{kernel_image_path:$k, boot_args:"console=ttyS0 reboot=k panic=1 pci=off"}')"

api PUT /drives/rootfs "$(jq -n --arg p "$WORK/rootfs.ext4" \
	'{drive_id:"rootfs", path_on_host:$p, is_root_device:true, is_read_only:false}')"

api PUT /network-interfaces/eth0 "$(jq -n --arg t "$TAP" \
	'{iface_id:"eth0", host_dev_name:$t}')"

api PUT /mmds/config "$(jq -n '{version:"V2", network_interfaces:["eth0"]}')"

# THE PAYLOAD THE AGENT WILL REFUSE. Its shape matters as much as its contents:
# the agent reads the contract FIRST, before anything else, so a payload carrying
# only a contract is enough to drive it to the refusal.
api PUT /mmds "$(jq -n --arg c "$REFUSED_CONTRACT" '{billet:{contract:$c}}')"

echo "booting"
api PUT /actions '{"action_type":"InstanceStart"}'

# THE PASS CONDITION IS A SENTENCE ON THE CONSOLE, polled with a hard bound.
# Firecracker exits 0 on some guest-side failures, so waiting on the process would
# report success for a guest that never started.
found=""

for _ in $(seq 1 "$BOOT_TIMEOUT"); do
	if grep -q "metadata contract $REFUSED_CONTRACT" "$CONSOLE" 2>/dev/null; then
		found=agent

		break
	fi

	if ! kill -0 "$FCPID" 2>/dev/null; then
		break
	fi

	sleep 1
done

echo
echo "=== console ==="
cat "$CONSOLE"
echo "=== end console ==="
echo

if [ "$found" != "agent" ]; then
	echo "the guest never reported refusing contract $REFUSED_CONTRACT within ${BOOT_TIMEOUT}s." >&2
	echo "That sentence is what proves the kernel booted, systemd started, the network" >&2
	echo "came up, MMDS was reachable and the agent ran. Its absence means one of those" >&2
	echo "did not happen; the console above says which." >&2

	exit 1
fi

echo "the agent fetched its metadata and refused contract $REFUSED_CONTRACT, which proves:"
echo "  - the kernel booted this filesystem"
echo "  - systemd reached the point of starting the agent"
echo "  - the network came up and the route to the metadata service works"
echo "  - MMDS answered and the agent parsed what it got"
echo

# DOCKER IS CHECKED SEPARATELY AND IS NOT FATAL HERE. The agent refuses early, so
# it may exit before docker has finished starting -- absence at this instant is not
# evidence of a broken docker, and failing on it would make this test flaky for a
# reason that has nothing to do with the image.
if grep -qi "docker" "$CONSOLE" 2>/dev/null; then
	echo "note: docker appears in the console"
else
	echo "note: docker did not appear in the console before the agent refused; this test"
	echo "      does not wait for it, and check-guest-image.sh asserts it is installed"
	echo "      and enabled"
fi

echo
echo "BOOT VERIFIED"
