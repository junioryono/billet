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

# THE PAYLOAD THE AGENT WILL REFUSE, IN THE SHAPE BILLET ACTUALLY SERVES.
#
# The nesting is load-bearing and this got it wrong: the agent fetches
# /latest/meta-data/billet/contract, so the data store has to be nested under
# `latest` and `meta-data`. Served flat, the fetch 404s and the agent reports "this
# billet did not say which metadata contract it speaks" -- which reads as a broken
# image and was a broken test.
#
# Worth noting that this is precisely the class of failure the boot gate exists to
# catch: a host serving a shape the guest does not expect. The test made the same
# mistake a buggy billet would, and the guest caught it.
#
# The shape is duplicated from metadata() in internal/provider/firecracker; a Go
# test asserts the two agree, because nothing else can.
api PUT /mmds "$(jq -n --arg c "$REFUSED_CONTRACT" \
	'{latest:{"meta-data":{billet:{contract:$c}}}}')"

echo "booting"
api PUT /actions '{"action_type":"InstanceStart"}'

# THREE THINGS ARE WATCHED FOR, NOT ONE, so a failure says which half of the boot
# worked. The first version waited only for the agent's verdict, and when that did
# not appear it reported that the guest "never came up" -- while the console showed
# it reaching multi-user.target. Docker starting here is itself a failure: the agent
# has not accepted its metadata or mounted the cache-backed image store yet.
# The old test required Docker to start and therefore proved the wrong ordering. Its
# message sent the reader after the image.
#
# Firecracker exits 0 on some guest-side failures, so none of this waits on the
# process.
saw_multiuser=0
docker_started=0
saw_agent=0

for _ in $(seq 1 "$BOOT_TIMEOUT"); do
	grep -q "Reached target.*[Mm]ulti-[Uu]ser" "$CONSOLE" 2>/dev/null && saw_multiuser=1
	grep -q "Started.*docker.service" "$CONSOLE" 2>/dev/null && docker_started=1
	grep -q "metadata contract $REFUSED_CONTRACT" "$CONSOLE" 2>/dev/null && saw_agent=1

	[ "$saw_agent" -eq 1 ] && break

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

report() {
	if [ "$1" -eq 1 ]; then
		echo "  ok    $2"
	else
		echo "  FAIL  $2" >&2
	fi
}

report "$saw_multiuser" "systemd reached multi-user.target"
report "$saw_agent" "the agent fetched its metadata and refused contract $REFUSED_CONTRACT"
if [ "$docker_started" -eq 0 ]; then
	echo "  ok    Docker stayed stopped before the agent mounted its image store"
else
	echo "  FAIL  Docker started before the agent mounted its image store" >&2
fi

echo

if [ "$saw_multiuser" -ne 1 ] || [ "$saw_agent" -ne 1 ] || [ "$docker_started" -ne 0 ]; then
	echo "this image did not boot into a working guest." >&2
	echo >&2

	if [ "$saw_multiuser" -eq 1 ] && [ "$saw_agent" -ne 1 ]; then
		echo "systemd came up but the agent never reported its verdict. Either it could not" >&2
		echo "reach the metadata service, or its output is not reaching the console --" >&2
		echo "billet-agent.service must set StandardOutput=journal+console, or its messages" >&2
		echo "go to a journal inside a guest that is about to be destroyed." >&2
	fi

	exit 1
fi

echo "the agent's refusal proves the whole chain:"
echo "  - the kernel booted this filesystem"
echo "  - systemd reached its target while Docker stayed behind the agent's cache mount"
echo "  - the network came up and the route to the metadata service works"
echo "  - MMDS answered and the agent parsed what it got"
echo
echo "BOOT VERIFIED"
