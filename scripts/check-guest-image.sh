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

# GITHUB'S DECLARATION IS WHAT THIS GATE CHECKS AGAINST, and it is the same file
# the build installs from. A gate holding its own copy of the expected contents
# is a second list to keep in step with the first, and the failure when they
# drift is a gate that passes an image missing exactly what it stopped checking.
TOOLSET_FILE="${TOOLSET_FILE:-$(cd "$(dirname "$0")/.." && pwd)/internal/runnerimages/toolset-2404.json}"

# THE TOOLS BILLET BAKES, IN ONE PLACE. This list was written out twice -- once
# for the structural loop and once for the coverage loop -- so adding a seventh
# installer meant remembering both, and forgetting one left the gate silently
# skipping exactly the tool that had just been added. That is the two-lists
# problem this whole file exists to avoid, reproduced inside it.
#
# NOT DERIVED FROM THE DECLARATION, deliberately: the declaration names tools
# billet does not install yet, and asserting those would fail every build for a
# gap that is documented rather than accidental. This is the installer's list.
#
# AN ARRAY, NOT A STRING. `for tool in $TOOLS` word-splits AND glob-expands, which
# is the defect this file just had: CodeQL's declared `*` iterated the working
# directory. No value here contains a glob character, and that is precisely the
# reasoning that made the other one safe until it was not.
TOOLCACHE_TOOLS=(node go Python PyPy Ruby CodeQL)

# THE SAME ALIAS MAP THE BUILD INSTALLS FROM.
#
# A mapped package must still be VERIFIED, not excused. If the build installed
# netcat-openbsd for a declared `netcat` and this gate went on looking for
# `netcat`, it would report a missing package on every correct image -- and the
# obvious fix for that noise is to drop the check, which is how a mapping quietly
# becomes an exemption.
APT_ALIASES="${APT_ALIASES:-$(cd "$(dirname "$0")/.." && pwd)/internal/runnerimages/apt-aliases.json}"

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
	# /proc FIRST. It is mounted INSIDE the image's mountpoint, so unmounting the
	# image while it is still there fails with "target is busy" and leaves both
	# behind -- holding the image file open for whatever wants to upload or delete
	# it next.
	if mountpoint -q "$MNT/proc" 2>/dev/null; then
		umount "$MNT/proc" || umount -l "$MNT/proc" || true
	fi

	if mountpoint -q "$MNT" 2>/dev/null; then
		umount "$MNT" || umount -l "$MNT" || true
	fi

	rmdir "$MNT" 2>/dev/null || true
}

trap cleanup EXIT

# READ ONLY, because nothing here should be able to change the artifact it is
# checking -- and because a mount that replays a journal has modified the image,
# which changes the digest the manifest published.
mount -o ro,loop "$IMAGE" "$MNT"

# /proc IS REQUIRED BY THE THINGS THIS GATE EXECUTES, and that was measured rather
# than anticipated: without it `go env GOVERSION` reports "'go' binary is trimmed
# and GOROOT is not set" and the JDK's launcher fails to load libjli.so. Both
# locate their own installation through /proc/self/exe, so in a chroot without
# /proc neither can find files sitting correctly beside it.
#
# The first version of this gate had no /proc and reported every go entry and
# every JDK as broken on an image where all of them worked -- a gate that fails
# correct images is worse than no gate, because the fix people reach for is
# deleting the check.
#
# READ-ONLY AND NOSUID/NODEV/NOEXEC: this is the host's /proc, exposed only so a
# binary can read its own path. Nothing here should be able to write through it.
mount -o bind,ro,nosuid,nodev,noexec /proc "$MNT/proc"

fail() {
	echo "  FAIL  $*" >&2
	FAILED=1
}

# declared_packages_missing_from compares github's declaration against what dpkg
# reports, printing either the count that matched or the names that did not.
#
# A FUNCTION SO IT CAN BE TESTED. The rest of this script needs root and a mounted
# image, which is why none of it has unit tests; this comparison is the part most
# able to be silently wrong -- a grep that matched substrings, an empty install
# list read as "everything present", a declaration that parsed to nothing -- and
# all of that is testable with two strings.
#
# EXIT 0 with the count, 1 with the missing names, 2 when the declaration is
# empty. The third is not the same as the second: a toolset that parsed to nothing
# would make this gate pass an image carrying no packages at all, which is the
# vacuous-check failure this project keeps finding.
declared_packages_missing_from() {
	local toolset="$1" installed="$2"
	# ${APT_ALIASES:-} RATHER THAN $APT_ALIASES. Under `set -u` an unset variable
	# is fatal, and this function is deliberately callable on its own -- that is
	# what makes the comparison testable at all.
	local aliases="${3:-${APT_ALIASES:-}}"
	local missing="" declared=0 pkg

	# AN ABSENT ALIAS FILE IS AN EMPTY MAP, NOT A FAILURE. jq --slurpfile refuses a
	# missing path, so the file is read into a variable first and defaults to `{}`.
	# The mapping is a narrow exception list; a deployment without one is the
	# ordinary case rather than a broken one.
	local alias_json="{}"
	if [ -r "$aliases" ]; then
		alias_json=$(cat "$aliases")
	fi

	while IFS= read -r pkg; do
		[ -n "$pkg" ] || continue

		declared=$((declared + 1))

		# grep -qxF: EXACT, WHOLE LINE, LITERAL. Without -x, "ssh" matches the
		# line "sshpass" and the gate reports a package present that is not; -F
		# because a package name is not a pattern.
		#
		# AND ITS EXIT STATUS IS THREE-VALUED, NOT TWO. grep answers 0 for found,
		# 1 for not found, and ANYTHING ABOVE 1 for "I could not look" -- so
		# `if ! grep` folds a failure to run into a negative result, and the gate
		# reports a package absent when what actually happened is that the check
		# did not happen. That is the same collapse this project fixed in its
		# credential paths: could-not-tell is not the same answer as no.
		#
		# Whether it explains a run of this gate that failed once in CI and could
		# not be reproduced in 25 local runs is NOT established -- it is written
		# this way because the distinction is real, and a false "missing package"
		# is a build failure nobody can act on.
		# A HERE-STRING, NOT A PIPELINE, AND THIS IS THE MEASURED CAUSE OF THAT CI FAILURE.
		#
		# `grep -q` exits the moment it matches. Under `set -o pipefail` that makes
		# the PIPELINE's status the writer's, and a printf whose write has not
		# completed takes SIGPIPE and exits 141. Measured:
		#
		#   early match, pipefail -> 141
		#   late match            -> 0
		#   no match              -> 1
		#
		# So a package that IS installed could report 141, which the old
		# `if ! pipeline` read as "not found" and turned into a missing package --
		# rarely, because a short list usually completes its single write before
		# grep exits. That is a CI failure naming a package the image has, which
		# will not reproduce.
		#
		# The here-string removes the writer, so there is no process to signal. It
		# is the same fix, for the same reason, as the pypy checksum lookup in
		# install-toolcache.sh -- the second time this exact shape has cost a
		# debugging round here.
		local found=0
		grep -qxF "$pkg" <<<"$installed" || found=$?

		case "$found" in
			0) ;;
			1) missing="$missing $pkg" ;;
			*)
				echo "could not check whether $pkg is installed: grep exited $found" >&2

				return 2
				;;
		esac
	done <<EOF
$(jq -r --argjson alias "$alias_json" '
	[.apt.vital_packages[], .apt.common_packages[], .apt.cmd_packages[],
		(.clang.versions[]? | select(. != null and . != "") | "clang-" + .),
			(.gcc.versions[]?), (.gfortran.versions[]?),
			(.php.versions[]? | select(. != null and . != "") | "php" + . + "-cli"),
			(.postgresql.version | select(. != null and . != "") | "postgresql-client-" + .),
			(if (.pipx | length) > 0 then "pipx" else empty end)]
	| map(select(. != null and . != ""))
	| map(
		$alias[.] as $entry
		| if $entry == null then .
		  # AN EMPTY install PRODUCES A BLANK EXPECTED NAME, which the loop then
		  # skips -- so the package vanishes from what this gate requires at the
		  # same moment it vanishes from what the build installs, and the image
		  # publishes without it. Erroring here makes the declaration empty, which
		  # the caller already treats as its own failure rather than a pass.
		  elif ($entry | type) != "object" or ($entry.install // "") == "" then
			error("alias maps to nothing usable")
		  else $entry.install
		  end)
	| reduce .[] as $p ([]; if index($p) then . else . + [$p] end)
	| .[]
' "$toolset" 2>/dev/null)
EOF

	if [ "$declared" -eq 0 ]; then
		return 2
	fi

	if [ -n "$missing" ]; then
		printf '%s\n' "${missing# }"

		return 1
	fi

	printf '%s\n' "$declared"
}

pass() { echo "  ok    $*"; }

# toolset_query reads one expectation set out of the pinned declaration, and
# treats a parser failure as a failure rather than as an empty expectation.
#
# A GATE THAT SWALLOWS jq PASSES ON NOTHING. Every one of these reads used
# `2>/dev/null || true`, so a malformed declaration, a renamed key or a jq that
# is not installed produced an empty string, the loop below it iterated zero
# times, and the gate reported success for an image it had not looked at. That
# is the whole failure mode this script exists to prevent, arrived at from the
# other side. An empty RESULT is still allowed -- a section may legitimately
# declare nothing -- but an empty result because the question could not be asked
# is not.
toolset_query() {
	local out status=0

	out=$(jq -r "$1" "$TOOLSET_FILE") || status=$?

	if [ "$status" -ne 0 ]; then
		echo "could not read $2 out of $TOOLSET_FILE (jq exited $status); every check" \
			"that reads it would otherwise pass against an empty expectation" >&2
		exit 1
	fi

	printf '%s' "$out"
}

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

# THE SHIM IS WHAT MAKES `type=gha` FROM A CONTAINER-DRIVER BUILDER WORK WITH
# NO WORKFLOW CHANGE: it points the buildx client's cache URL at the adapter,
# because the runner sets that URL per step and nothing else billet controls can.
DOCKER_SHIM="$MNT/usr/local/bin/docker"
if [ -x "$DOCKER_SHIM" ] && grep -Fq 'ACTIONS_RESULTS_URL=$BILLET_ACTIONS_CACHE_URL' "$DOCKER_SHIM" &&
	grep -Fq 'exec "$real" "$@"' "$DOCKER_SHIM"; then
	pass "the docker shim points a build's BuildKit cache client at billet's adapter"
else
	fail "no docker shim at /usr/local/bin/docker; on an interception tier a type=gha
        export from a container-driver builder fails with an x509 error unless the
        workflow names the adapter itself"
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

# CHECKED HERE BECAUSE THE FAILURE IS OTHERWISE INVISIBLE.
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

# --- parity with github's declaration ---------------------------------------

# EVERY PACKAGE GITHUB SAYS IS THERE, CHECKED AGAINST dpkg RATHER THAN A PATH.
#
# THE EXPECTED SET IS ITERATED, NOT THE INSTALLED ONE. That direction is the whole
# value: walking what happens to be installed and reporting it can never notice an
# absence, which is the shape of gate this project has already been bitten by
# twice. Here the pinned declaration is the list, and anything on it that dpkg
# does not report is named individually.
#
# dpkg-query, NOT A FILE TEST. Most of these packages install no binary of their
# own name -- libssl-dev, tzdata, locales, fonts-noto-color-emoji -- so a path
# check would report failures for packages that are correctly present, and the
# noise would make the real absences unreadable.

if [ ! -r "$TOOLSET_FILE" ]; then
	fail "cannot read the pinned toolset at $TOOLSET_FILE, so nothing can check that this
        image carries what github's declaration says it does"
elif ! chroot "$MNT" /usr/bin/dpkg-query --version >/dev/null 2>&1; then
	fail "the image has no working dpkg-query, so package parity cannot be established"
else
	# ONE dpkg-query CALL, not one per package: seventy-four chroots is slow
	# enough to matter in a gate that runs on every build.
	installed=$(chroot "$MNT" /usr/bin/dpkg-query -W -f '${Package} ${Status}\n' 2>/dev/null |
		awk '$4 == "installed" {print $1}' | sort)

	if result=$(declared_packages_missing_from "$TOOLSET_FILE" "$installed"); then
		pass "all $result packages from github's declaration are installed"
	else
		status=$?

		if [ "$status" -eq 2 ]; then
			fail "the pinned toolset declares no packages; this gate would pass an image
        carrying nothing, which is exactly the vacuous check it exists to avoid"
		else
			fail "github's declaration names these packages and this image does not have them:
       $result

        Every one is something a workflow may reasonably assume, so the failure it
        causes arrives inside somebody's job rather than here."
		fi
	fi
fi

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

# EVERY ONE OF THESE FAILS SILENTLY, WHICH IS WHY THEY ARE ASSERTED.
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

	# ALL SIX THE DECLARATION NAMES. PyPy, Ruby and CodeQL were installed by the
	# shared installer and checked by nothing, so the gate reported success about
	# half a toolcache.
	for tool in "${TOOLCACHE_TOOLS[@]}"; do
		for versiondir in "$TOOLCACHE/$tool"/*; do
			[ -d "$versiondir" ] || continue

			version=$(basename "$versiondir")

			# A FULL SEMVER OR IT IS INVISIBLE. tool-cache keeps only directories that
			# parse as an explicit version when resolving a range, so `20` is skipped
			# rather than matched by `node-version: 20`.
			#
			# A REGEX ANCHORED AT BOTH ENDS, NOT A SHELL GLOB. `[0-9]*.[0-9]*.[0-9]*`
			# is three "digit followed by anything" runs: it accepts `1.2.3junk`,
			# `1.2.3.4` and `1x.2y.3z`, which are exactly the names a botched
			# extraction produces and which tool-cache then refuses to parse. The
			# glob passes them and the gate reports a toolcache that is invisible to
			# every range request.
			if ! printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
				fail "$tool/$version is not a full semver, so a workflow asking for a
        range will never match it"

				continue
			fi

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

# check_toolcache_coverage asserts every line the declaration names has an entry.
#
# A FUNCTION SO IT CAN BE RUN WITHOUT A LOOP MOUNT. Everything else in this file
# needs root and a mounted image; this section needs only $TOOLCACHE and
# $TOOLSET_FILE, and it is where the two failures that cost the most were --
# a loop that ran zero times and reported success, and a declared `*` that
# glob-expanded against the working directory. Both are cheap to test and were
# untestable while this was inline.
check_toolcache_coverage() {
	local tool declared_globs glob prefix found versiondir candidate

	# AN EMPTY LIST IS A FAILURE, NOT AN EMPTY LOOP. `"${arr[@]}"` on an UNSET
	# array is not an error under `set -u` -- bash expands it to nothing -- so a
	# renamed or unsourced list makes this function check every tool it was given,
	# which is none, and report success. That is the vacuous check the comment
	# below is about, arriving through the refactor that was meant to remove a
	# duplicated list. Caught by a test, not by reading it.
	if [ "${#TOOLCACHE_TOOLS[@]}" -eq 0 ]; then
		fail "no toolcache tools are named, so this check would iterate nothing and
        report success. TOOLCACHE_TOOLS is what the gate walks; an empty one means
        it was renamed or never sourced."

		return
	fi

	for tool in "${TOOLCACHE_TOOLS[@]}"; do
		# THE DECLARATION IS CAPTURED AND CHECKED BEFORE IT IS ITERATED.
		#
		# `for glob in $(jq ...)` runs zero times when the tool is absent, its
		# version list is empty, or jq errors -- and a loop that runs zero times
		# records nothing, so the gate reports success having checked nothing.
		# The entries already on disk keep the earlier non-zero count happy, so
		# nothing else notices either. That is precisely the vacuous check this
		# section was written to replace, reintroduced one level up.
		declared_globs=$(jq -r --arg t "$tool" \
			'.toolcache[] | select(.name == $t) | .versions[]' "$TOOLSET_FILE" 2>/dev/null ||
			true)

		if [ -z "$(printf '%s' "$declared_globs" | tr -d '[:space:]')" ]; then
			fail "the pinned toolset declares no $tool versions. Either the declaration is
        malformed or upstream renamed the tool -- the names are case-sensitive and
        inconsistent ($tool here, but node and go are lowercase while Python and Ruby
        are not). Refusing rather than checking nothing and reporting success."

			continue
		fi

		# READ, NOT `for glob in $declared_globs`. An unquoted variable in a
		# `for` list undergoes PATHNAME EXPANSION as well as word splitting,
		# and CodeQL's declared version is literally `*` -- which expands to
		# every non-dot entry in whatever directory the gate was started from.
		# Measured from the repo root: a three-entry declaration iterated 21
		# times, and the gate failed once per filename, so no image could ever
		# be published. Every earlier declaration was digit-led and only
		# matched a file by accident.
		#
		# THE INSTALLER ALREADY LEARNED THIS. read_toolset_versions reads the
		# same declaration into an array for the same reason and says so in a
		# comment -- the rule was enforced where the versions are used and not
		# where they are checked.
		while IFS= read -r glob; do
			[ -n "$glob" ] || continue

			# THE GLOB IS A LINE, AND ANY PATCH ON IT SATISFIES THE LINE. A
			# workflow asks for `3.10`, not `3.10.21`, so the check is that the
			# line is represented rather than that a particular patch is.
			# THE THREE SHAPES THE DECLARATION USES, and only one of them is
			# already a pattern. `22.*` trims to `22.` and matches 22.20.0.
			# `*` trims to nothing and matches anything, which is what CodeQL
			# means by it. A BARE MINOR IS NEITHER: PyPy declares `3.9`, and
			# trimming a star that is not there leaves `3.9`, which matches
			# `3.90.1` -- a different line entirely, satisfying a promise about
			# 3.9 with an entry for something else. The EC2 gate appends `.`
			# for exactly this case, and a check the two backends disagree
			# about is worse than either.
			case "$glob" in
				*\*) prefix="${glob%\*}" ;;
				*) prefix="$glob." ;;
			esac

			found=""

			for versiondir in "$TOOLCACHE/$tool"/*; do
				[ -d "$versiondir/x64" ] || continue

				candidate=$(basename "$versiondir")

				# NOT `found=... && break`. Under `set -e` an && list whose left
				# side is false returns 1, and a compound command returning 1
				# outside a condition is what set -e exits on -- the idiom this
				# project's guest agent was rewritten to remove.
				case "$candidate" in
					"$prefix"*)
						found="$candidate"
						break
						;;
				esac
			done

			if [ -n "$found" ]; then
				pass "toolcache covers $tool $glob (as $found)"
			elif grep -qxF "$tool $glob" "$TOOLCACHE/.billet-unpublished" 2>/dev/null; then
				# EXCUSED ONLY BY THE BUILD'S OWN RECORD. github declares Ruby
				# 4.0.* and ruby-builder publishes nothing for it, because Ruby
				# 4.0 is not released -- a declaration ahead of reality rather
				# than a download that went wrong. The build writes that file;
				# this reads it. An -x match, so a line cannot be excused by a
				# longer one that happens to contain it.
				pass "toolcache omits $tool $glob, which the build recorded as unpublished"
			else
				fail "github's image offers $tool $glob and this toolcache has no entry on
        that line, and the build did not record it as unpublished. A workflow
        pinning it finds nothing, downloads a runtime, and succeeds -- so the only
        symptom is that the job is as slow as it would be with no toolcache at all,
        and nothing reports why."
			fi
		done <<<"$declared_globs"
	done
}

	# EVERY LINE GITHUB DECLARES HAS AN ENTRY, checked by iterating the DECLARATION.
	#
	# The loop above walks what is on disk, which can report what is there and can
	# never report what is missing -- and "missing" is the whole failure mode: a
	# workflow pinning `python-version: '3.10'` finds nothing, downloads an
	# interpreter, and succeeds. Nothing anywhere says the toolcache was useless.
	# Counting entries does not catch it either, because the other four lines make
	# the count non-zero.
	if [ ! -r "$TOOLSET_FILE" ]; then
		fail "cannot read the pinned toolset at $TOOLSET_FILE, so nothing can check that the
        toolcache covers the versions github's image offers"
	else
		check_toolcache_coverage
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

	# GO IS RUN TOO, FOR THE REASON NODE IS.
	#
	# Everything above it is structural: a directory, an x64 subdirectory, a marker
	# beside it. An empty `go/1.24.0/x64` with a `.complete` marker satisfies every
	# one of those AND the coverage check, so a tar that unpacked to the wrong
	# place, or a download that produced nothing, ships an image whose go toolcache
	# is a set of empty directories. Executing the binary is the only check that
	# distinguishes "the entry exists" from "the entry works".
	#
	# GOVERSION RATHER THAN `go version`, because the latter prints a sentence with
	# the platform in it and the former prints exactly the release name -- which is
	# what has to match the directory.
	#
	# AND THE COMPARISON ALLOWS AN INITIAL RELEASE. go reports "go1.26" from a
	# directory named "1.26.0", because the directory must be a full semver for
	# tool-cache to see it at all while go's own name for that release is not.
	for versiondir in "$TOOLCACHE/go"/*; do
		[ -d "$versiondir/x64" ] || continue

		version=$(basename "$versiondir")
		reported=$(chroot "$MNT" "/opt/hostedtoolcache/go/$version/x64/bin/go" \
			env GOVERSION 2>/dev/null || true)

		if [ "$reported" = "go$version" ] || [ "$reported" = "go${version%.0}" ]; then
			pass "go $version reports itself as $reported"
		else
			fail "go in $version reports \"$reported\". An entry that is structurally
        complete but does not run is one setup-go finds, uses, and fails the job
        with -- or an empty directory nothing here would otherwise notice."
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

	# AND THE THREE THIS GATE USED TO CHECK ONLY STRUCTURALLY.
	#
	# A directory, an x64 subdirectory and a marker beside it are satisfied by a
	# tarball that unpacked to the wrong place -- which is precisely the failure
	# each of these three is prone to: ruby-builder's archive is NOT stripped, the
	# codeql bundle nests under `codeql/`, and pypy's entry points are symlinks
	# this build creates rather than ones the tarball ships. The EC2 gate already
	# executes all three, so leaving this one structural made the two backends
	# disagree about what a usable image is.
	#
	# `--version` PROVES THE WHOLE CHAIN: for pypy it resolves two symlinks, and
	# for a shared-library runtime it proves the loader finds what it needs, which
	# no stat can.
	for tool_spec in "PyPy bin/python --version" "Ruby bin/ruby --version" \
		"CodeQL codeql/codeql version"; do
		# PARAMETER EXPANSION RATHER THAN `set --`. At top level `set --` replaces
		# the SCRIPT's positional parameters, and this script reads its image and
		# manifest from $1 and $2. They are captured into variables long before
		# here so nothing breaks today -- which is exactly what makes it the kind
		# of trap a later edit springs.
		tool="${tool_spec%% *}"
		tool_rest="${tool_spec#* }"
		binary="${tool_rest%% *}"
		flag="${tool_rest#* }"

		for versiondir in "$TOOLCACHE/$tool"/*; do
			[ -d "$versiondir/x64" ] || continue

			version=$(basename "$versiondir")

			if chroot "$MNT" "/opt/hostedtoolcache/$tool/$version/x64/$binary" \
				"$flag" >/dev/null 2>&1; then
				pass "$tool $version runs from the toolcache"
			else
				fail "$tool $version has an x64 directory and a completion marker and its
        $binary does not run. An entry that is structurally complete but broken is
        one the action finds, uses, and fails the job with."
			fi
		done
	done

	# THE TOOLCHAINS THAT ARE NOT TOOLCACHE ENTRIES, RUN.
	#
	# cmake, pwsh and the dotnet SDKs go on PATH rather than under
	# <tool>/<version>/<arch>, so the loops above cannot see them -- and a tool that
	# is installed and checked by nothing is the failure this file exists to catch.
	# Each is executed rather than stat'ed: dotnet in particular is a launcher that
	# finds its SDKs through DOTNET_ROOT, so a present binary proves nothing about
	# whether a job can build.
	# clang IS HERE BECAUSE apt CREATES NO BARE `clang`. The versioned packages are
	# co-installable, so none of them owns the unsuffixed name, and a check that
	# only counted packages passed against an image where `clang` was not a command
	# at all.
	for spec in "cmake /usr/local/bin/cmake --version" \
		"pwsh /usr/bin/pwsh --version" \
		"clang /usr/bin/clang --version" \
		"clang++ /usr/bin/clang++ --version" \
		"dotnet /usr/bin/dotnet --list-sdks"; do
		name="${spec%% *}"
		rest="${spec#* }"
		bin="${rest%% *}"
		flag="${rest#* }"

		if chroot "$MNT" "$bin" "$flag" >/dev/null 2>&1; then
			pass "$name runs from $bin"
		else
			fail "$name does not run from $bin, so a workflow using it fails on an image
        that advertises having it"
		fi
	done

	# EVERY DOTNET TOOL THE DECLARATION NAMES, RUN BY ITS OWN DECLARED PROBE. The
	# probe and the tool name need not match, and reading the declaration is the
	# whole point of having one.
	declared_dotnet_tools=$(toolset_query '.dotnet.tools[]?.test // empty' "the dotnet tools")

	while IFS= read -r probe; do
		[ -n "$probe" ] || continue

		if chroot "$MNT" sh -lc "$probe" >/dev/null 2>&1; then
			pass "the dotnet tool probe \"$probe\" runs"
		else
			fail "the declaration names the dotnet tool probe \"$probe\" and it does not run
        on this image"
		fi
	done <<<"$declared_dotnet_tools"

	# AND IT MUST BE THE DECLARED MAJOR. A `clang` linked to any installed version
	# runs, so the check above passes while a workflow pinning the toolset's default
	# gets a different compiler than the hosted image would have given it.
	clang_default=$(toolset_query '.clang.default_version // empty' "clang's default version")

	if [ -n "$clang_default" ]; then
		clang_on_path=$(chroot "$MNT" /usr/bin/clang --version 2>/dev/null | head -1)

		case "$clang_on_path" in
		*"clang version $clang_default."*)
			pass "the default clang is $clang_default"
			;;
		*)
			fail "the toolset names clang $clang_default as the default and the image reports
        \"$clang_on_path\""
			;;
		esac
	fi

	# AND EVERY DECLARED SDK IS THERE, not merely one of them. `dotnet --list-sdks`
	# succeeding proves the launcher works; a channel that failed to install leaves
	# it succeeding with fewer SDKs than the declaration names.
	declared_sdks=$(toolset_query '.dotnet.versions[]? // empty' "the dotnet channels")
	listed=$(chroot "$MNT" /usr/bin/dotnet --list-sdks 2>/dev/null || true)

	while IFS= read -r channel; do
		[ -n "$channel" ] || continue

		if grep -q "^$channel\." <<<"$listed"; then
			pass "dotnet has an SDK on the $channel channel"
		elif grep -qxF "dotnet $channel" "$TOOLCACHE/.billet-unpublished" 2>/dev/null; then
			pass "dotnet omits $channel, which the build recorded as unpublished"
		else
			fail "the declaration names dotnet $channel and the image has no SDK on it;
        a workflow pinning that channel downloads one"
		fi
	done <<<"$declared_sdks"

	# THE DEFAULT RUNTIMES ARE ON PATH, which nothing checked until something
	# needed them. A toolcache entry is found by an ACTION; a workflow step that
	# runs a bare `node` or `gem` resolves against the system, and this image's apt
	# set carries neither.
	for cmd in node npm ruby gem; do
		if chroot "$MNT" "/usr/local/bin/$cmd" --version >/dev/null 2>&1; then
			pass "$cmd resolves on PATH"
		else
			fail "$cmd is not on PATH; a step calling it without setup-node or setup-ruby
        fails on an image that contains several of them"
		fi
	done

	# AND IT IS THE DECLARED DEFAULT, not merely some version. `node.default` is
	# upstream saying which line the system one is, and an image whose bare `node`
	# is a different major breaks steps that never asked for a version.
	node_default=$(toolset_query '.node.default // empty' "node's default version")
	node_on_path=$(chroot "$MNT" /usr/local/bin/node --version 2>/dev/null || true)

	case "$node_on_path" in
		"v$node_default."*) pass "the default node on PATH is $node_on_path" ;;
		*)
			fail "the toolset names node $node_default as the default and PATH has
        \"$node_on_path\""
			;;
	esac

	# EVERY GLOBAL THE DECLARATION NAMES A COMMAND FOR. pipx and node_modules
	# entries carry the command they provide, which is the strongest check
	# available -- a package that installed and left no working command is exactly
	# what a job discovers instead of the image build. The two sections spell the
	# field differently, which is why this asks for both.
	declared_globals=$(toolset_query \
		'(.pipx[]?.cmd // empty), (.node_modules[]?.command // empty)' \
		"the global commands")

	while IFS= read -r cmd; do
		[ -n "$cmd" ] || continue

		if chroot "$MNT" sh -lc "command -v $cmd" >/dev/null 2>&1; then
			pass "the global $cmd is on PATH"
		else
			fail "the declaration names a global providing $cmd and the image has no such
        command"
		fi
	done <<<"$declared_globals"

	# CODEQL NEEDS A SECOND MARKER, INSIDE THE ENTRY.
	#
	# `x64.complete` beside the entry is what @actions/tool-cache stats; codeql's
	# own action stats `pinned-version` within it, and without that it re-downloads
	# a bundle that is already there -- the entire cost this bakes in to avoid,
	# paid on every job while every other check passes.
	for versiondir in "$TOOLCACHE/CodeQL"/*; do
		[ -d "$versiondir/x64" ] || continue

		version=$(basename "$versiondir")

		if [ -f "$versiondir/x64/pinned-version" ]; then
			pass "codeql $version carries the pinned-version marker its action looks for"
		else
			fail "codeql $version has no x64/pinned-version, so the action re-downloads the
        bundle it already has on every job"
		fi
	done
fi

# --- the JDKs ---------------------------------------------------------------

# EVERY DECLARED VERSION, EXECUTED, WITH ITS VARIABLE POINTING AT IT.
#
# Three things have to hold together and each fails silently on its own: the JDK
# is installed, `java -version` actually runs (a package that unpacked wrong is
# structurally complete and non-functional), and JAVA_HOME_<v>_X64 names that
# directory. The third is the one worth stating: setup-java reads the variable to
# find a JDK already present, so a variable pointing at a directory that does not
# exist is WORSE than no variable at all -- an unset one makes setup-java install
# a JDK and the job succeeds, while a wrong one is trusted and everything
# downstream fails naming none of this.
if [ ! -r "$TOOLSET_FILE" ]; then
	fail "cannot read the pinned toolset, so nothing can check the JDKs"
else
	java_versions=$(toolset_query '.java.versions[]' "the java versions")

	if [ -z "$(printf '%s' "$java_versions" | tr -d '[:space:]')" ]; then
		fail "the pinned toolset declares no java versions; refusing to check nothing and
        report success"
	else
		for v in $java_versions; do
			home="/usr/lib/jvm/temurin-$v-jdk-amd64"

			if [ ! -d "$MNT$home" ]; then
				fail "the toolset declares java $v and $home does not exist. Every workflow
        using setup-java with that version downloads a JDK on a machine that was
        supposed to have one."

				continue
			fi

			# -XX:-UsePerfData BECAUSE THIS MOUNT IS READ-ONLY. The JVM creates
			# /tmp/hsperfdata_<user> on startup, and this image is mounted ro on
			# purpose -- a mount that replayed a journal would modify the artifact
			# whose digest the manifest published. The same reasoning is why the
			# python check runs `--version` rather than `pip cache dir`: the test
			# must be about the image, not about the mount's writability.
			if ! chroot "$MNT" "$home/bin/java" -XX:-UsePerfData -version >/dev/null 2>&1; then
				fail "java $v is installed at $home and does not run. A package that unpacked
        wrong is structurally complete and non-functional, and nothing above this
        check can tell the difference."

				continue
			fi

			if ! grep -qxF "JAVA_HOME_${v}_X64=$home" "$MNT/etc/billet-image-env"; then
				fail "java $v is installed and JAVA_HOME_${v}_X64 does not name $home.
        setup-java reads that variable to find a JDK already present, so a wrong or
        missing one either sends it to a path that does not exist or makes it
        download a JDK this image already has."

				continue
			fi

			# AND IT HAS A TOOLCACHE ENTRY POINTING AT IT.
			#
			# Everything above proves the JDK is installed and findable through an
			# environment variable. setup-java ALSO looks in the toolcache, and an
			# entry that is missing, unlinked, or lacks its sibling marker is
			# invisible there while every check above still passes — the JDK gets
			# downloaded on a machine that has it, which is the silent failure this
			# whole section exists to prevent.
			#
			# MATCHED BY TARGET RATHER THAN BY RECOMPUTING THE VERSION. The entry
			# is named from the JDK's own release file, and deriving that here
			# would be a second copy of java_toolcache_version to drift against the
			# build. What must be true is that some entry links to THIS jdk and is
			# complete, which is checkable without knowing its name.
			entry=""
			for candidate in "$TOOLCACHE/Java_Temurin-Hotspot_jdk"/*; do
				[ -L "$candidate/x64" ] || continue

				if [ "$(readlink "$candidate/x64")" = "$home" ]; then
					entry="$candidate"

					break
				fi
			done

			if [ -z "$entry" ]; then
				fail "java $v is installed and no toolcache entry links to $home.
        setup-java would not find it there and would download a JDK this image
        already carries."

				continue
			fi

			if [ ! -f "$entry/x64.complete" ]; then
				fail "the java $v toolcache entry $(basename "$entry") has no x64.complete
        marker beside its arch directory, so tool-cache treats it as a
        half-finished download and ignores it."

				continue
			fi

			if ! printf '%s' "$(basename "$entry")" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
				fail "the java $v toolcache entry is named $(basename "$entry"), which is not an
        explicit version; tool-cache skips a directory that does not parse as one,
        so the entry is invisible to every range request."

				continue
			fi

			pass "java $v runs, JAVA_HOME_${v}_X64 names it, toolcache entry $(basename "$entry")"
		done

		java_default=$(toolset_query '.java.default' "java's default version")

		if [ -n "$java_default" ] && [ "$java_default" != "null" ] &&
			! grep -qxF "JAVA_HOME=/usr/lib/jvm/temurin-$java_default-jdk-amd64" \
				"$MNT/etc/billet-image-env"; then
			fail "JAVA_HOME does not name the declared default JDK ($java_default). A bare
        \`java\` or a build tool reading JAVA_HOME gets a different version from a
        hosted runner, which is the kind of difference that surfaces as a compiler
        error in somebody's job."
		fi
	fi
fi

# --- the environment a hosted runner exports --------------------------------

# BOTH HALVES, BECAUSE EITHER ALONE IS SATISFIED BY A BROKEN IMAGE.
#
# The file can exist while nothing reads it, and the agent can read a file that
# is not there. Only the pair means a job actually sees these variables -- and
# the job's environment is built with `env -i`, so a variable that does not reach
# that array does not exist for the job however plainly it is written on disk.
IMAGE_ENV="$MNT/etc/billet-image-env"

if [ ! -r "$IMAGE_ENV" ]; then
	fail "no /etc/billet-image-env, so a job sees none of the variables a hosted runner
        exports. Actions that select a prebuilt binary by reading ImageOS fall back to
        building from source, or fail outright."
elif [ "$(grep -Ec '^ImageOS=[A-Za-z0-9_.-]+$' "$IMAGE_ENV" || true)" != 1 ]; then
	# EXACTLY ONE, NOT AT LEAST ONE. The agent appends every assignment it reads
	# in order, and `env -i` takes the LAST value for a repeated name -- so a
	# second ImageOS line silently overrides the first, and a check that stops at
	# the first match reports the value the job will not see. Zero and two are
	# different bugs with the same fix: say how many there are.
	fail "/etc/billet-image-env must set ImageOS exactly once and sets it
        $(grep -Ec '^ImageOS=' "$IMAGE_ENV" || true) time(s). It is the variable
        third-party actions branch on to pick a prebuilt binary for the platform, and
        a repeated assignment means the job sees the last one rather than this one."
elif [ "$(grep -Ec '^ImageVersion=[A-Za-z0-9_.-]+$' "$IMAGE_ENV" || true)" != 1 ]; then
	fail "/etc/billet-image-env must set ImageVersion exactly once. It is written beside
        ImageOS and read by the same actions."
elif ! grep -Fq 'IMAGE_ENV_FILE:-/etc/billet-image-env' "$AGENT"; then
	fail "the guest agent never reads /etc/billet-image-env, so the file is written into
        the image and no job ever sees it. The job environment is built with env -i:
        a variable absent from that array does not exist for the job."
elif ! grep -Fq 'runner_env+=("$line")' "$AGENT"; then
	fail "the guest agent reads /etc/billet-image-env but does not add what it finds to
        the array the job is launched with, so the read has no effect."
else
	# BEFORE THE LAUNCH, NOT MERELY PRESENT. The array is passed to `env -i` at the
	# launch; anything appended after that point is appended to a variable nobody
	# reads again. Every check above is satisfied by a block sitting below the
	# launch, which is the shape of "the text is there and does nothing".
	#
	# BOTH LINE NUMBERS ARE REQUIRED TO EXIST BEFORE THEY ARE COMPARED. `[ "" -gt
	# 5 ]` is not false, it is an ERROR -- exit 2 with "integer expected" -- and in
	# an elif chain a non-zero condition falls through to the `else` that reports
	# success. So a missing launch marker would have made this check pass silently,
	# which is the exact vacuous shape it was written to prevent.
	env_line=$(grep -n 'IMAGE_ENV_FILE:-/etc/billet-image-env' "$AGENT" | cut -d: -f1 | head -1)
	launch_line=$(grep -n 'BILLET_AGENT_LAUNCH_BEGIN' "$AGENT" | cut -d: -f1 | head -1)

	if [ -z "$env_line" ] || [ -z "$launch_line" ]; then
		fail "cannot locate both the image-environment read and the job launch in the guest
        agent, so their order cannot be established. Without that, a read placed after
        the launch would append to an array nothing uses again."
	elif [ "$env_line" -gt "$launch_line" ]; then
		fail "the guest agent reads /etc/billet-image-env at line $env_line and launches the
        job at line $launch_line, so the variables are added to an array that has
        already been used and no job ever sees them."
	else
		pass "the image declares $(grep -E '^ImageOS=' "$IMAGE_ENV") and the agent passes it to jobs"
	fi
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
	grep -Fq 'results-receiver.actions.githubusercontent.com' "$ACTIONS_PROXY" &&
	grep -Fq 'def node_tunnel' "$ACTIONS_PROXY" &&
	grep -Fq 'def systemd_listener' "$ACTIONS_PROXY"; then
	pass "the guest carries the transparent Actions results passthrough"
else
	fail "the guest has no Actions results passthrough; only the one results origin is
        DNS-remapped to it, so without it interception silently degrades to GitHub's cache"
fi

# THE ADAPTER IS PART OF THE INTERCEPTION CONTRACT, so an image that speaks the
# current contract without it is refused here rather than discovered by a build.
# Both halves are checked: the script can serve the plaintext endpoint on the docker gateway,
# and the agent actually starts it -- a script nothing launches is dead code, and
# a unit whose script does not know the mode fails after the guest has booted.
#
# `--mode cache-adapter` AND THE BIND ADDRESS, because those two appear ONLY in
# the startup command. The unit name alone is satisfied by the shutdown line the
# agent runs after every job, so an agent whose startup block had been deleted
# would pass a check keyed on it.
if [ -x "$ACTIONS_PROXY" ] &&
	grep -Fq 'def handle_adapter' "$ACTIONS_PROXY" &&
	grep -Fq 'X-Billet-Cache-Client' "$ACTIONS_PROXY" &&
	grep -Fq 'X-Billet-Cache-Origin' "$ACTIONS_PROXY" &&
	[ -x "$AGENT" ] &&
	grep -Fq -- '--mode cache-adapter' "$AGENT" &&
	grep -Fq -- '--socket-property=ListenStream="$docker_gateway:$actions_cache_port"' "$AGENT" &&
	grep -Fq 'BILLET_ACTIONS_CACHE_URL' "$AGENT"; then
	pass "the guest can serve BuildKit's type=gha cache over plaintext on the docker gateway"
else
	fail "the guest has no plaintext cache adapter, or the agent does not start it; a
        container-driver BuildKit cannot verify the node's leaf, so type=gha would fail
        with x509 on an image this contract says supports it"
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

# DHCP MUST be keyed on the MAC, not a DUID. The image bakes one /etc/machine-id
# into every clone, so networkd's default DUID client id is identical on every
# guest and dnsmasq hands them all one address -- the collision that stalled large
# downloads. The backend gives each guest a stable per-tap MAC, so ClientIdentifier=mac
# makes each live guest's lease unique and reuses the address when a tap is reused.
if grep -qxE '[[:space:]]*ClientIdentifier=mac' "$MNT/etc/systemd/network/10-eth0.network" 2>/dev/null; then
	pass "DHCP is keyed on the per-guest MAC, so cloned guests do not share a lease"
else
	fail "10-eth0.network does not set ClientIdentifier=mac; cloned guests share the
        machine-id-derived DUID, collide on one DHCP lease, and stall large transfers"
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
