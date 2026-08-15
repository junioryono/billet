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

# ONE PIN FOR EVERY IMAGE BILLET BUILDS, read from the file the Go code embeds.
# It was a shell default here and a constant in `billet ami`, so bumping the runner
# was two edits in two languages -- and doing one of them leaves a fleet where one
# backend is current and the other is not, found on the day GitHub stops queueing to
# the stale half.
PINNED_RUNNER_FILE="$(dirname "$0")/../internal/runnerrelease/pinned.txt"

if [ -z "${RUNNER_VERSION:-}" ] && [ ! -r "$PINNED_RUNNER_FILE" ]; then
	echo "cannot read the pinned runner version at $PINNED_RUNNER_FILE, and RUNNER_VERSION" >&2
	echo "was not set; run this from a checkout, or say which release to install" >&2
	exit 1
fi

RUNNER_VERSION="${RUNNER_VERSION:-$(awk "NR==1{print \$1}" "$PINNED_RUNNER_FILE")}"
SUITE="${SUITE:-noble}"
# 4096MB AGAINST ROUGHLY 2.6GB USED once the toolcache is in (#66): the base system
# and runner are about 1.4GB, and node, go and python together add about 1.2GB.
#
# GROWING THIS IS NOT FREE. The head image is resized to fit, and every generation
# already published keeps its own size -- so a larger number here costs disk on
# every node from the next publish onward, and a number too small fails the build
# partway through mkfs with a message about space rather than about the toolcache.
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

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "this builds a filesystem and maps a block device; run it as root" >&2
		exit 1
	fi
}

need_tools() {
	local missing=()
	for t in debootstrap mkfs.ext4 chroot jq; do
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

# TOOLCACHE_DIR IS NOT A CHOICE (#66).
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

# install_toolcache bakes language runtimes in so `setup-*` is a PATH change.
#
# WITHOUT THIS, EVERY JOB PAYS A DOWNLOAD PER LANGUAGE. GitHub's image ships these
# pre-extracted and `setup-node` finds them in about a second; on an image without
# them the same step fetches a couple of hundred megabytes, every job, every time.
# That is the difference between "works" and "as fast as github's".
#
# LATEST RESOLVED AT BUILD TIME, NOT PINNED. A toolcache is a cache, not a
# contract: a workflow asking for `node-version: 20` does not care which patch it
# finds, and pinning would buy a reproducibility nobody wants at the cost of
# shipping stale runtimes. Every download is still checksum-verified against what
# the vendor published for the version resolved, because this image runs other
# people's code.
install_toolcache() {
	local rootfs="$1"
	local tc="$rootfs$TOOLCACHE_DIR"

	mkdir -p "$tc"

	# PYTHON'S EXTENSION MODULES LINK LIBRARIES THE TARBALL DOES NOT BUNDLE.
	# Without these, the interpreter starts and `import sqlite3` fails with a
	# loader error naming a .so, which reads as a broken workflow rather than a
	# missing package.
	chroot "$rootfs" /bin/bash -euxc '
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -qq
		apt-get install -y --no-install-recommends \
			libsqlite3-0 libreadline8t64 libgdbm6t64 libgdbm-compat4t64 \
			libbz2-1.0 liblzma5 libffi8 libuuid1 libncursesw6
		apt-get clean
		rm -rf /var/lib/apt/lists/*
	'

	install_node_toolcache "$tc"
	install_go_toolcache "$tc"
	install_python_toolcache "$tc"

	# READ AND WRITTEN BY EVERY JOB, which runs as the unprivileged runner account.
	chmod -R 0777 "$tc"

	echo "toolcache: $(du -sh "$tc" | cut -f1)"
}

# fetch_verified downloads a file and checks it against a digest.
fetch_verified() {
	local url="$1" out="$2" want="$3"

	curl -fL -sS --http1.1 --connect-timeout 20 --max-time 900 \
		--retry 5 --retry-delay 5 --retry-all-errors -o "$out" "$url"

	echo "$want  $out" | sha256sum -c - >/dev/null
}

# install_node_toolcache bakes in the two newest LTS lines.
#
# THE VERSION DIRECTORY MUST BE A FULL SEMVER. @actions/tool-cache resolves a range
# by listing the version directories and keeping only those that parse as explicit
# semver, so a directory named `20` is invisible to `node-version: 20` -- it does
# not match loosely, it is skipped entirely.
install_node_toolcache() {
	local tc="$1"

	local versions
	versions=$(curl -fsSL --retry 3 --retry-all-errors https://nodejs.org/dist/index.json |
		jq -r '[.[] | select(.lts != false)] | group_by(.version | split(".")[0])
			| map(max_by(.version)) | sort_by(.date) | reverse | .[0:2] | .[].version')

	local v
	for v in $versions; do
		local bare="${v#v}"
		local file="node-$v-linux-x64.tar.gz"
		local want

		# THE CHECKSUM COMES FROM THE RELEASE ITSELF, published beside the tarball.
		want=$(curl -fsSL --retry 3 --retry-all-errors "https://nodejs.org/dist/$v/SHASUMS256.txt" |
			awk -v f="$file" '$2 == f {print $1}')

		if [ -z "$want" ]; then
			echo "no published checksum for node $v; refusing to bake an unverified runtime" >&2
			exit 1
		fi

		fetch_verified "https://nodejs.org/dist/$v/$file" "$WORK/node.tgz" "$want"

		local dir="$tc/node/$bare/x64"
		mkdir -p "$dir"

		# STRIPPED, because setup-node adds `<dir>/bin` to PATH and the tarball has
		# everything under a `node-vX-linux-x64/` component.
		tar -xzf "$WORK/node.tgz" -C "$dir" --strip-components=1
		rm -f "$WORK/node.tgz"

		# THE MARKER IS A SIBLING OF THE ARCH DIRECTORY, not inside it, and its
		# absence makes the entry invisible however complete it is: tool-cache stats
		# `<version>/<arch>.complete` and treats a missing one as a half-extracted
		# download.
		touch "$tc/node/$bare/x64.complete"

		echo "toolcache: node $bare"
	done
}

install_go_toolcache() {
	local tc="$1"

	local meta
	meta=$(curl -fsSL --retry 3 --retry-all-errors 'https://go.dev/dl/?mode=json')

	local version want
	version=$(printf '%s' "$meta" | jq -r '.[0].version')
	want=$(printf '%s' "$meta" | jq -r --arg v "$version" \
		'.[0].files[] | select(.filename == ($v + ".linux-amd64.tar.gz")) | .sha256')

	if [ -z "$want" ] || [ "$want" = "null" ]; then
		echo "go published no checksum for $version; refusing to bake an unverified runtime" >&2
		exit 1
	fi

	fetch_verified "https://go.dev/dl/$version.linux-amd64.tar.gz" "$WORK/go.tgz" "$want"

	# THE DIRECTORY IS A BARE SEMVER WITH NO `go` PREFIX. setup-go coerces its
	# version through semver before looking, so `go1.26.6` on disk is never found.
	local bare="${version#go}"
	local dir="$tc/go/$bare/x64"

	mkdir -p "$dir"
	tar -xzf "$WORK/go.tgz" -C "$dir" --strip-components=1
	rm -f "$WORK/go.tgz"
	touch "$tc/go/$bare/x64.complete"

	echo "toolcache: go $bare"
}

# install_python_toolcache bakes in the two newest stable minors.
#
# THE TOOL NAME IS CAPITALISED. setup-python looks for `Python`, while setup-node
# and setup-go look for `node` and `go` -- an inconsistency in the actions
# themselves, and one that fails silently on a case-sensitive filesystem: the
# directory exists, nothing finds it, and the job downloads a runtime anyway.
install_python_toolcache() {
	local tc="$1"

	local manifest
	manifest=$(curl -fsSL --retry 3 --retry-all-errors \
		https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json)

	# STABLE ONLY, AND NOT THE FREE-THREADED BUILD. The free-threaded interpreter is
	# published as a separate arch (`x64-freethreaded`) beside the ordinary one, and
	# baking it in would give workflows an interpreter with different semantics and
	# nothing in the logs to explain why.
	local versions
	versions=$(printf '%s' "$manifest" | jq -r '
		[.[] | select(.stable == true)
		     | select(any(.files[]; .platform == "linux"
		                        and .platform_version == "24.04"
		                        and .arch == "x64"))]
		| group_by(.version | split(".")[0:2] | join("."))
		| map(max_by(.version | split(".") | map(tonumber)))
		| sort_by(.version | split(".") | map(tonumber)) | reverse | .[0:2] | .[].version')

	local v
	for v in $versions; do
		local url
		url=$(printf '%s' "$manifest" | jq -r --arg v "$v" '
			.[] | select(.version == $v) | .files[]
			| select(.platform == "linux" and .platform_version == "24.04" and .arch == "x64")
			| .download_url')

		fetch_verified "$url" "$WORK/python.tgz" \
			"$(curl -fsSL --retry 3 --retry-all-errors "$url" | sha256sum | cut -d" " -f1)"

		local dir="$tc/Python/$v/x64"
		mkdir -p "$dir"

		# NOT STRIPPED. This tarball's root already holds bin/, lib/ and setup.sh.
		tar -xzf "$WORK/python.tgz" -C "$dir"
		rm -f "$WORK/python.tgz"

		# WHAT setup.sh WOULD HAVE DONE, minus the parts that need a network.
		#
		# The tarball ships NO `python` or `pip` entry point -- only `python3.12` and
		# friends -- because its setup script creates them. Skipping it leaves an
		# interpreter that works when addressed by full version and is missing every
		# name a workflow actually types.
		local minor="${v%.*}"

		ln -sf "./bin/python$minor" "$dir/python"
		ln -sf "python$minor" "$dir/bin/python"
		ln -sf "python$minor" "$dir/bin/python${minor/./}"

		rm -f "$dir/setup.sh" "$dir/build_output.txt" "$dir/tools_structure.txt"

		touch "$tc/Python/$v/x64.complete"

		echo "toolcache: python $v"
	done
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
			docker.io systemd-resolved netplan.io libicu74 \
			unzip zip zstd tar wget rsync build-essential
		apt-get clean
		rm -rf /var/lib/apt/lists/*
	'

	# WHY THESE SIX, AND WHY THEY ARE NOT OPTIONAL (#66).
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
	chroot "$rootfs" chown -R runner:runner /home/runner

	install_toolcache "$rootfs"

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
# RUNNER_TOOL_CACHE IS PASSED THROUGH THE exec, not left to the environment.
#
# /etc/environment is read by PAM for login sessions and does NOT apply to systemd
# services, and this agent IS one -- so setting it there alone would leave every
# job looking in the runner's default _work/_tool, finding nothing, and downloading
# a runtime the image already contains. Nothing would report that: the job would
# simply be slower.
#
# setpriv does not preserve it either, which is why it is named here explicitly.
exec setpriv --reuid=runner --regid=runner --init-groups --inh-caps=-all -- \
	env ACTIONS_RUNNER_INPUT_JITCONFIG="$ACTIONS_RUNNER_INPUT_JITCONFIG" \
	RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
	AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache "${cmd[@]}"
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
		printf "RUNNER_TOOL_CACHE=/opt/hostedtoolcache\nAGENT_TOOLSDIRECTORY=/opt/hostedtoolcache\n" >>/etc/environment
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
	local contract
	contract=$(sed -n 's/^WANT_CONTRACT=\([0-9][0-9]*\)$/\1/p' \
		"$rootfs/usr/local/bin/billet-agent" | head -1)

	if [ -z "$contract" ]; then
		echo "could not read the guest contract out of the agent that was just installed;" >&2
		echo "refusing to describe an image whose protocol version is unknown" >&2
		exit 1
	fi

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
