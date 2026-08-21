#!/usr/bin/env bash
#
# Check a built guest image before anybody publishes it.
#
# WHY THIS IS A GATE AND NOT A REPORT. An image that reaches a release is one every
# deployment pulls, and a bad one is not a local problem: it fans out to everybody
# on the next refresh, and the thing that would rebuild it is itself a guest
# booting the image. The cheapest moment to catch it is before the upload, so this
# runs between packing and publishing and its exit status decides whether the
# release happens.
#
# WHAT IT CAN AND CANNOT SEE. This inspects the filesystem: that the runner is
# installed and is the version the manifest claims, that Docker and the agent are
# there, that the units which have to start are enabled. It does NOT boot anything,
# so it cannot see an integration failure -- a kernel missing an option the
# userspace needs, systemd failing to bring the network up, Docker refusing to
# start. Booting is what `billet images verify` does, and the two are complementary
# rather than alternatives.
#
# It is worth having anyway because the failures it DOES catch are the silent ones:
# a build step that failed in a way `set -e` did not notice, a tarball that
# unpacked to the wrong place, an agent that was never installed. Those produce an
# image that boots perfectly and then does nothing, which is the hardest kind to
# diagnose from the outside.
set -euo pipefail

IMAGE="${1:-}"
MANIFEST="${2:-}"

if [ -z "$IMAGE" ]; then
	echo "usage: check-guest-image.sh <rootfs.img> [manifest.json]" >&2
	exit 2
fi

if [ ! -r "$IMAGE" ]; then
	echo "cannot read $IMAGE" >&2
	exit 2
fi

if [ "$(id -u)" -ne 0 ]; then
	echo "this mounts a filesystem image; run it as root" >&2
	exit 2
fi

MNT=$(mktemp -d)
FAILED=0

# UNMOUNTED ON EVERY PATH. A loop mount left behind holds the image file open, and
# the next step in a build is usually the one that wants to upload or delete it.
cleanup() {
	if mountpoint -q "$MNT" 2>/dev/null; then
		umount "$MNT" || true
	fi

	rmdir "$MNT" 2>/dev/null || true
}

trap cleanup EXIT

# READ ONLY, because nothing here should be able to change the artifact it is
# checking -- and because a mount that replays a journal has modified the image,
# which changes the digest the manifest published.
mount -o ro,loop "$IMAGE" "$MNT"

fail() {
	echo "  FAIL  $*" >&2
	FAILED=1
}

pass() { echo "  ok    $*"; }

# --- the runner ------------------------------------------------------------

RUNNER_DIR="$MNT/home/runner/runner"
RUNNER_WORK="$RUNNER_DIR/_work"

if [ -x "$RUNNER_DIR/run.sh" ]; then
	pass "the actions runner is installed"
else
	fail "no runner at /home/runner/runner/run.sh; every job would fail to start"
fi

if [ -x "$RUNNER_DIR/billet-runner-service" ] &&
	grep -Fq '100 | 101 | 102 | 103 | 104 | 105' "$RUNNER_DIR/billet-runner-service"; then
	pass "the runner service preserves authoritative one-job results"
else
	fail "the runner service wrapper is absent or masks the one-job result codes"
fi

runner_ids=$(awk -F: '$1 == "runner" {print $3 ":" $4}' "$MNT/etc/passwd")
work_ids=""
if [ -d "$RUNNER_WORK" ]; then
	work_ids=$(stat -c '%u:%g' "$RUNNER_WORK")
fi
if [ -n "$runner_ids" ] && [ "$work_ids" = "$runner_ids" ]; then
	pass "the runner work directory belongs to the runner account"
else
	fail "the runner work directory belongs to ${work_ids:-no account}, not
        ${runner_ids:-the missing runner account}; the runner cannot initialize a job"
fi

AGENT="$MNT/usr/local/bin/billet-agent"

# SYSTEMD DOES NOT CREATE A LOGIN ENVIRONMENT. The agent runs as root and drops
# privileges with setpriv, so the account switch alone supplies neither a writable
# home nor the ordinary account identity variables. Tool installers can complete
# and then fail when the tool first asks for its per-user cache.
runner_environment=1
for assignment in '"HOME=/home/runner"' '"USER=runner"' '"LOGNAME=runner"' \
	'"ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true"'; do
	if ! grep -Eq "^[[:space:]]*${assignment}$" "$AGENT" 2>/dev/null; then
		runner_environment=0
	fi
done
if ! grep -Fq 'env -i "${runner_env[@]}" "${cmd[@]}"' "$AGENT" 2>/dev/null; then
	runner_environment=0
fi
if ! grep -Fq '"ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE=${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE:-}"' "$AGENT" 2>/dev/null; then
	runner_environment=0
fi
if [ "$runner_environment" -eq 1 ]; then
	pass "the guest agent establishes the runner account environment"
else
	fail "the guest agent does not launch jobs with HOME=/home/runner, USER=runner and
        LOGNAME=runner, or does not request the authoritative one-job result; setup
        actions can lose their user cache and Docker writes cannot be gated safely"
fi

DOCKER_CACHE="$MNT/usr/local/bin/billet-docker-cache"
if [ -x "$DOCKER_CACHE" ] && grep -Fq '[ "$status" = 100 ]' "$DOCKER_CACHE" &&
	grep -Fq '/v1/docker-store' "$DOCKER_CACHE" &&
	grep -Fq '/v1/docker-store/ready' "$DOCKER_CACHE"; then
	pass "the Docker image store is mounted and prepares only a clean runner result"
else
	fail "the Docker cache helper is absent or can prepare a failed runner result for
	        host-side publication"
fi

DOCKER_DAEMON="$MNT/etc/docker/daemon.json"
if [ -r "$DOCKER_DAEMON" ] && jq -e '
	.features["containerd-snapshotter"] == false and
	.["storage-driver"] == "overlay2" and
	.bip == "172.17.0.1/16"
' "$DOCKER_DAEMON" >/dev/null; then
	pass "Docker keeps pulled images inside the cache-backed data root on a pinned gateway"
else
	fail "Docker is not pinned to overlay2 under /var/lib/docker; fresh Docker 29
        installations keep image content in /var/lib/containerd and bypass the cache"
fi

# READ FROM WHAT THE BUILD RECORDED, not from the runner tarball. The tarball
# ships no version file of its own -- this gate reported exactly that on its first
# real run -- so the build writes /etc/billet-image at the moment it unpacks the
# runner, which is the only point where what-was-downloaded and what-was-installed
# are the same fact.
if [ -r "$MNT/etc/billet-image" ]; then
	installed=$(sed -n 's/^RUNNER_VERSION=\(.*\)$/\1/p' "$MNT/etc/billet-image" |
		tr -d '[:space:]')

	if [ -z "$installed" ]; then
		fail "/etc/billet-image records no runner version"
	else
		pass "runner version $installed"
	fi

	if [ -n "$MANIFEST" ] && [ -r "$MANIFEST" ]; then
		claimed=$(jq -r '.runner_version' "$MANIFEST")

		if [ "$installed" != "$claimed" ]; then
			fail "the manifest claims runner $claimed and the image carries $installed; the
        thirty-day expiry is judged from the manifest, so the fleet would stop being
        sent jobs on a date derived from the wrong number"
		else
			pass "the manifest agrees with the image"
		fi
	fi
else
	fail "no /etc/billet-image, so nothing records which runner this image carries and
        the manifest's claim cannot be checked against the disk"
fi

# --- docker ----------------------------------------------------------------

if [ -x "$MNT/usr/bin/dockerd" ] || [ -x "$MNT/usr/sbin/dockerd" ]; then
	pass "dockerd is installed"
else
	fail "no dockerd; every container step would fail"
fi

if [ -x "$MNT/usr/bin/docker" ]; then
	pass "the docker client is installed"
else
	fail "no docker client"
fi

buildx_plugin=""
for candidate in \
	usr/local/lib/docker/cli-plugins/docker-buildx \
	usr/local/libexec/docker/cli-plugins/docker-buildx \
	usr/lib/docker/cli-plugins/docker-buildx \
	usr/libexec/docker/cli-plugins/docker-buildx; do
	if [ -x "$MNT/$candidate" ]; then
		buildx_plugin="/$candidate"
		break
	fi
done
if [ -n "$buildx_plugin" ]; then
	pass "the Docker Buildx CLI plugin is installed at $buildx_plugin"
else
	fail "no Docker Buildx CLI plugin; workflows using docker buildx would fail"
fi

compose_plugin=""
for candidate in \
	usr/local/lib/docker/cli-plugins/docker-compose \
	usr/local/libexec/docker/cli-plugins/docker-compose \
	usr/lib/docker/cli-plugins/docker-compose \
	usr/libexec/docker/cli-plugins/docker-compose; do
	if [ -x "$MNT/$candidate" ]; then
		compose_plugin="/$candidate"
		break
	fi
done
if [ -n "$compose_plugin" ]; then
	pass "the Docker Compose CLI plugin is installed at $compose_plugin"
else
	fail "no Docker Compose CLI plugin; workflows using docker compose would fail"
fi

# --- what workflows assume is there ----------------------------------------

# CHECKED HERE BECAUSE THE FAILURE IS OTHERWISE INVISIBLE (#66).
#
# zstd is the one that matters most and the one a human would never notice
# missing: actions/cache picks its compression by shelling out to it, falls back
# to gzip when it is absent, and folds the choice into the CACHE VERSION HASH. An
# image without it produces caches that can never match one from a github-hosted
# runner -- same key, permanent miss, no error anywhere. Nothing about a running
# job says so, which is exactly why it belongs in a gate.
#
# unzip and tar throw from inside @actions/tool-cache rather than reporting
# anything about the image, so a missing one surfaces as "this action is broken".
for tool in zstd unzip zip tar wget rsync gcc make; do
	found=""

	for dir in usr/bin usr/sbin bin sbin usr/local/bin; do
		if [ -x "$MNT/$dir/$tool" ]; then
			found="/$dir/$tool"

			break
		fi
	done

	if [ -n "$found" ]; then
		pass "$tool is installed"
	else
		fail "no $tool. Workflows assume it: without zstd every actions/cache entry this
        fleet writes has a different version hash from a github-hosted one and can never
        be restored, and without unzip or tar the setup-* actions throw from inside
        @actions/tool-cache"
	fi
done

# A `pip` -- not just `pip3` -- must resolve on the default PATH, the way the
# github-hosted image ships one. setup-python's `cache: pip` runs `pip cache dir`
# through io.which('pip', true), which throws the instant no `pip` answers; the
# hosted image's system pip is the backstop this guest was missing, so the lookup
# no longer depends on the toolcache entry's PATH addition holding.
#
# Each check runs AS THE RUNNER ACCOUNT (setpriv), under a scrubbed environment
# (env -i) carrying only the runner's effective PATH -- not sbin -- and its HOME.
# That is the context setup-python actually runs in: a root chroot inheriting the
# builder's env would let PYTHONHOME or PIP_CONFIG_FILE mask a real fault. Running
# `--version` proves the shebang chain rather than a stat a symlinked build host
# would satisfy. It is deliberately NOT `pip cache dir`: this image is mounted
# read-only (a mount that replayed a journal would modify what it checks), and pip
# disables -- and errors -- its cache when it cannot create the directory, so
# `pip cache dir` would test the mount's writability, not the image. The cache
# directory is written at job time from a writable home; the image property is that
# pip resolves and runs, which `--version` establishes.
runner_pip=(setpriv --reuid=runner --regid=runner --clear-groups
	/usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/home/runner)
for tool in pip pip3 python; do
	if chroot "$MNT" "${runner_pip[@]}" "$tool" --version >/dev/null 2>&1; then
		pass "$tool resolves and runs on the runner's default PATH"
	else
		fail "no working $tool on the runner's default PATH. setup-python's cache: pip
        runs io.which('pip', true), which throws when no pip -- not pip3 -- resolves,
        exactly as it did before python3-pip and python-is-python3 were installed"
	fi
done

# The hosted image writes break-system-packages so a `pip install` against the
# system python survives PEP 668; without it the system pip resolves but refuses to
# install, which fails later and more confusingly than an absent pip. The builder
# owns the exact file, so compare it BYTE FOR BYTE: a second section's override (an
# [install] break-system-packages = false) or the token under an unrelated section
# would leave `pip install` refusing while a line-match passed. cmp, not command
# substitution, so an embedded NUL or a stray trailing newline cannot slip past.
if cmp -s "$MNT/etc/pip.conf" <(printf '%s\n' '[global]' 'break-system-packages = true'); then
	pass "the system pip may install past PEP 668, as it does on a hosted runner"
else
	fail "/etc/pip.conf is not the expected two-line break-system-packages file; a
        workflow that pip installs against the system python may fail here where a
        hosted runner succeeds"
fi

# --- the toolcache ---------------------------------------------------------

# EVERY ONE OF THESE FAILS SILENTLY, WHICH IS WHY THEY ARE ASSERTED (#66).
#
# A toolcache entry that is subtly wrong does not break a job. `setup-node` simply
# does not find it, downloads a couple of hundred megabytes, and succeeds -- so the
# only symptom is that jobs are as slow as they were before the toolcache existed,
# and nothing anywhere says why. Four separate mistakes produce exactly that:
#
#   - the marker file missing, or written INSIDE the arch directory rather than
#     beside it, which is where tool-cache stats for it
#   - a version directory that is not a full semver, which the range resolver skips
#     entirely rather than matching loosely
#   - `x86_64` instead of `x64`
#   - `python` instead of `Python`, which the actions spell inconsistently
TOOLCACHE="$MNT/opt/hostedtoolcache"

if [ ! -d "$TOOLCACHE" ]; then
	fail "no toolcache at /opt/hostedtoolcache; every job would download a runtime the
        image was supposed to contain, and nothing would report it"
else
	toolcache_entries=0

	for tool in node go Python; do
		for versiondir in "$TOOLCACHE/$tool"/*; do
			[ -d "$versiondir" ] || continue

			version=$(basename "$versiondir")

			# A FULL SEMVER OR IT IS INVISIBLE. tool-cache keeps only directories that
			# parse as an explicit version when resolving a range, so `20` is skipped
			# rather than matched by `node-version: 20`.
			case "$version" in
				[0-9]*.[0-9]*.[0-9]*) ;;
				*)
					fail "$tool/$version is not a full semver, so a workflow asking for a
        range will never match it"
					continue
					;;
			esac

			if [ ! -d "$versiondir/x64" ]; then
				fail "$tool/$version has no x64 directory"
				continue
			fi

			# THE MARKER IS A SIBLING, and its absence makes the entry invisible however
			# complete it is.
			if [ ! -f "$versiondir/x64.complete" ]; then
				fail "$tool/$version has no x64.complete marker beside its arch directory,
        so tool-cache treats it as a half-finished download and ignores it"
				continue
			fi

			pass "toolcache $tool $version"
			toolcache_entries=$((toolcache_entries + 1))
		done
	done

	if [ "$toolcache_entries" -eq 0 ]; then
		fail "the toolcache directory exists and holds nothing usable"
	fi

	# THE RUNTIME ANSWERS TO ITS OWN DIRECTORY NAME.
	#
	# setup-node runs `node --version` after finding a cached entry and FAILS THE JOB
	# if it does not exactly equal the version directory it came from. A typo in that
	# name therefore ships a green image whose every node job dies, so the same check
	# happens here where it is cheap.
	for versiondir in "$TOOLCACHE/node"/*; do
		[ -d "$versiondir/x64" ] || continue

		version=$(basename "$versiondir")
		reported=$(chroot "$MNT" "/opt/hostedtoolcache/node/$version/x64/bin/node" --version 2>/dev/null || true)

		if [ "$reported" = "v$version" ]; then
			pass "node $version reports itself as $reported"
		else
			fail "node in $version reports \"$reported\"; setup-node compares exactly this
        against the directory name and fails the job when they differ"
		fi
	done

	# PYTHON'S ENTRY POINTS ARE MADE BY ITS setup.sh, WHICH THIS IMAGE DOES NOT RUN.
	# The tarball ships python3.12 and nothing called `python`, so an image that
	# skipped recreating them has an interpreter nobody can invoke by the name they
	# type.
	for versiondir in "$TOOLCACHE/Python"/*; do
		[ -d "$versiondir/x64" ] || continue

		version=$(basename "$versiondir")

		if [ ! -e "$versiondir/x64/bin/python" ] || [ ! -e "$versiondir/x64/python" ]; then
			fail "python $version is missing bin/python or the root python symlink, which
        its setup.sh creates and this build has to recreate"
			continue
		fi

		# BOTH PIP SURFACES, OR IT IS A GREEN IMAGE WHOSE EVERY PIP JOB DIES.
		# setup-python's `cache: pip` execs a bare `pip`; workflows run `python -m pip`.
		# ensurepip -- skipped once, which shipped an interpreter with neither -- is what
		# makes both exist, so prove both by running them under the mounted image.
		if [ -x "$versiondir/x64/bin/pip" ] &&
			chroot "$MNT" "/opt/hostedtoolcache/Python/$version/x64/bin/pip" --version >/dev/null 2>&1 &&
			chroot "$MNT" "/opt/hostedtoolcache/Python/$version/x64/bin/python" -m pip --version >/dev/null 2>&1; then
			pass "python $version has a working pip executable and module"
		else
			fail "python $version has no working pip; setup-python's cache: pip and
        python -m pip both fail on it, as when ensurepip was skipped"
		fi
	done
fi

# --- billet's agent --------------------------------------------------------

AGENT="$MNT/usr/local/bin/billet-agent"

if [ -x "$AGENT" ]; then
	pass "the billet agent is installed"

	# THE CONTRACT THE IMAGE ACTUALLY SPEAKS, read out of the installed agent.
	# A manifest is free to claim any number; this is the one that will run.
	contract=$(sed -n 's/^WANT_CONTRACT=\([0-9][0-9]*\)$/\1/p' "$AGENT" | head -1)

	if [ -z "$contract" ]; then
		fail "the agent declares no contract version, so nothing can tell whether it speaks
        to this billet"
	else
		pass "the agent speaks contract $contract"

		if [ -n "$MANIFEST" ] && [ -r "$MANIFEST" ]; then
			claimed=$(jq -r '.guest_contract' "$MANIFEST")

			if [ "$contract" != "$claimed" ]; then
				fail "the manifest advertises contract $claimed and the agent speaks $contract;
        a node would accept this image on the manifest's word and then get microVMs
        that boot and never report"
			else
				pass "the manifest's contract matches the agent's"
			fi
		fi
	fi
else
	fail "no billet agent; microVMs would boot and never register"
fi

ACTIONS_PROXY="$MNT/usr/local/bin/billet-actions-proxy"
if [ -x "$ACTIONS_PROXY" ] &&
	grep -Fq 'results-receiver.actions.githubusercontent.com:443' "$ACTIONS_PROXY" &&
	grep -Fq 'def node_tunnel' "$ACTIONS_PROXY" &&
	grep -Fq 'def systemd_listener' "$ACTIONS_PROXY"; then
	pass "the guest carries the transparent Actions results passthrough"
else
	fail "the guest has no Actions results passthrough; only the one results origin is
        DNS-remapped to it, so without it interception silently degrades to GitHub's cache"
fi

DNS_UPSTREAMS="$MNT/usr/local/bin/billet-dns-upstreams"
if [ -x "$DNS_UPSTREAMS" ] && grep -Fq 'billet-dns-upstreams <gateway-ip> <resolv-file>' "$DNS_UPSTREAMS"; then
	pass "the guest carries the container DNS upstream filter"
else
	fail "the guest has no container DNS upstream filter; an unvalidated resolver value
        could reach daemon.json and stop Docker from starting, turning a lost cache
        remap into a dead job"
fi

# --- service ordering -----------------------------------------------------

# ENABLED IS A SYMLINK ON DISK, which is exactly why this can be checked without
# booting. The agent must start, while Docker service and socket activation must
# stay disabled: the agent mounts the cache at /var/lib/docker before starting
# the daemon. Letting either Docker unit autostart opens the daemon on the root
# filesystem first and makes the later cache mount unsafe.
WANTS="$MNT/etc/systemd/system/multi-user.target.wants"
DOCKER_SOCKET="$MNT/etc/systemd/system/sockets.target.wants/docker.socket"

if [ -L "$WANTS/docker.service" ] || [ -e "$WANTS/docker.service" ] ||
	[ -L "$DOCKER_SOCKET" ] || [ -e "$DOCKER_SOCKET" ]; then
	fail "Docker service or socket activation is enabled; it can start before the billet
        agent mounts the image store"
else
	pass "Docker waits for the billet agent to mount its image store"
fi

if [ -L "$WANTS/billet-agent.service" ] || [ -e "$WANTS/billet-agent.service" ]; then
	pass "billet-agent.service is enabled"
else
	fail "billet-agent.service is installed but NOT enabled; the runner would never start"
fi

# systemd-networkd is wanted by a different target.
if [ -e "$MNT/etc/systemd/system/dbus-org.freedesktop.network1.service" ] ||
	[ -e "$MNT/etc/systemd/system/sockets.target.wants/systemd-networkd.socket" ] ||
	[ -e "$MNT/etc/systemd/system/multi-user.target.wants/systemd-networkd.service" ]; then
	pass "systemd-networkd is enabled"
else
	fail "systemd-networkd is not enabled; the guest would have no network and could not
        reach the metadata service"
fi

# --- the guest's own network unit ------------------------------------------

if [ -r "$MNT/etc/systemd/network/10-eth0.network" ]; then
	pass "the guest network unit is present"
else
	fail "no /etc/systemd/network/10-eth0.network; the guest would not configure eth0"
fi

# --- root is locked --------------------------------------------------------

# NOT COSMETIC. An account with no password is not the same as a locked one, and
# this image exists to run other people's code.
if grep -qE '^root:[!*]' "$MNT/etc/shadow" 2>/dev/null; then
	pass "root cannot log in"
else
	fail "the root account is not locked"
fi

echo

if [ "$FAILED" -ne 0 ]; then
	echo "this image is NOT fit to publish" >&2

	exit 1
fi

echo "the image looks fit to publish"
echo
echo "NOTE: this checked the filesystem's contents, not that it boots. Run"
echo "\`billet images verify\` against a cluster for that; the two catch"
echo "different things and neither replaces the other."
