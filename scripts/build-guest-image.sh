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

RUNNER_VERSION="${RUNNER_VERSION:-2.328.0}"
SUITE="${SUITE:-noble}"
SIZE_MB="${SIZE_MB:-4096}"
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
RUNNER_SHA256="${RUNNER_SHA256:-01066fad3a2893e63e6ca880ae3a1fad5bf9329d60e77ee15f2b97c148c3cd4e}"

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "this builds a filesystem and maps a block device; run it as root" >&2
		exit 1
	fi
}

need_tools() {
	local missing=()
	for t in debootstrap mkfs.ext4 rbd chroot jq; do
		command -v "$t" >/dev/null 2>&1 || missing+=("$t")
	done

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

	rm -rf "$WORK"
	mkdir -p "$rootfs"
	touch "$WORK/.billet-guest-workspace"

	echo "=== 1/6 base system ($SUITE) ==="
	# --variant=minbase, then systemd on top: the guest needs an init that can run
	# Docker's unit, and the alternative is hand-writing service supervision.
	debootstrap --variant=minbase --include=systemd,systemd-sysv,dbus \
		"$SUITE" "$rootfs" http://archive.ubuntu.com/ubuntu/

	echo "=== 2/6 packages ==="
	cat >"$rootfs/etc/apt/sources.list.d/ubuntu.sources" <<EOF
Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
Suites: $SUITE $SUITE-updates $SUITE-security
Components: main universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF

	chroot "$rootfs" /bin/bash -euxc '
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -qq
		apt-get install -y --no-install-recommends \
			ca-certificates curl iproute2 iptables jq git sudo \
			docker.io systemd-resolved netplan.io libicu74
		apt-get clean
		rm -rf /var/lib/apt/lists/*
	'

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
	curl -fsSL -o "$WORK/$tarball" \
		"https://github.com/actions/runner/releases/download/v$RUNNER_VERSION/$tarball"

	# VERIFIED BEFORE IT IS UNPACKED. This is a binary fetched over the network that
	# will execute somebody's CI, so "it downloaded" is not the same as "it is the
	# release it claims to be".
	echo "$RUNNER_SHA256  $WORK/$tarball" | sha256sum -c -

	mkdir -p "$rootfs/home/runner/runner"
	tar -xzf "$WORK/$tarball" -C "$rootfs/home/runner/runner"
	chroot "$rootfs" chown -R runner:runner /home/runner

	echo "=== 4/6 the agent that reads the registration ==="
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
	if token=$(curl -sf -X PUT "http://$MMDS/latest/api/token" \
		-H "X-metadata-token-ttl-seconds: 300" 2>/dev/null); then
		break
	fi

	if [ "$attempt" -ge 120 ]; then
		log "the metadata service never answered"
		exit 1
	fi

	sleep 0.5
done

fetch() { curl -sf -H "X-metadata-token: $token" "http://$MMDS/latest/meta-data/billet/$1"; }

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
WANT_CONTRACT=2

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
exec setpriv --reuid=runner --regid=runner --init-groups --inh-caps=-all -- \
	env ACTIONS_RUNNER_INPUT_JITCONFIG="$ACTIONS_RUNNER_INPUT_JITCONFIG" "${cmd[@]}"
AGENT

	install -m 0644 /dev/stdin "$rootfs/etc/systemd/system/billet-agent.service" <<'UNIT'
[Unit]
Description=billet: start the GitHub Actions runner from the metadata service
# AFTER THE NETWORK, because the registration is read over it. A guest that started
# this first would exhaust its retries before eth0 had an address.
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=exec
ExecStart=/usr/local/bin/billet-agent
# ONE JOB, ONE GUEST. The runner exits when its job is done and the microVM is
# destroyed with it, so a restart would register a second runner against a
# registration that has already been consumed.
Restart=no
StandardOutput=journal
StandardError=journal

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

# THE METADATA SERVICE IS LINK-LOCAL and is not on the bridge's subnet, so it needs a
# route of its own. The agent adds one too; having it here means the guest can reach
# the service before anything has run.
[Route]
Destination=169.254.169.254/32
Scope=link
NET

	chroot "$rootfs" /bin/bash -euxc '
		systemctl enable systemd-networkd systemd-resolved docker billet-agent
		# A CONSOLE THAT GOES NOWHERE COSTS BOOT TIME. billet passes no console= to
		# the guest, so a getty on ttyS0 would spin against a device nothing reads.
		systemctl mask getty@tty1.service serial-getty@ttyS0.service
		systemctl mask systemd-resolved-monitor.service 2>/dev/null || true
		echo billet-guest >/etc/hostname
		printf "127.0.0.1 localhost\n127.0.1.1 billet-guest\n" >/etc/hosts
		# ROOT CANNOT LOG IN. Nothing should be logging into a guest that exists for
		# one job, and an account with no password is not the same as a locked one.
		passwd -l root
	'

	echo "=== 6/6 filesystem ==="
	rm -f "$img"
	truncate -s "${SIZE_MB}M" "$img"
	mkfs.ext4 -q -F -d "$rootfs" "$img"

	echo "built $img ($(du -h "$img" | cut -f1))"

	if [ "$PUBLISH" != "yes" ]; then
		echo "PUBLISH=no, so it was not written to ceph"
		return
	fi

	publish "$img"
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
	trap 'unmap_image "$dev"' EXIT

	# gnudd, NOT dd: Ubuntu 26.04's uutils coreutils does not implement
	# `iflag=direct`, which is the same class of difference that broke `cephadm
	# bootstrap` on this host. See docs/adr-003-ceph-rbd.md.
	local ddbin=dd
	command -v gnudd >/dev/null 2>&1 && ddbin=gnudd

	"$ddbin" if="$img" of="$dev" bs=4M conv=fsync status=progress

	"${rbd[@]}" device unmap "$dev"
	dev=""
	trap - EXIT

	"${rbd[@]}" -p "$IMAGE_POOL" snap create "$IMAGE_NAME@$gen"

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
