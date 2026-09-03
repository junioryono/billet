# The toolcache GitHub's runner image declares, installed the same way by both
# backends.
#
# ONE COPY, BECAUSE EVERY LINE HERE IS A TRAP THAT COST A DEBUGGING SESSION.
# `x64.complete` is a SIBLING of the arch directory and its absence makes a
# complete entry invisible; setup-python looks for `Python` while setup-node
# looks for `node`; go publishes an initial release as `go1.26` rather than
# `go1.26.0` and tool-cache skips a directory that is not an explicit semver;
# Temurin's field is `SEMANTIC_VERSION` and three of its five releases are not
# valid semver; python's tarball has no pip until ensurepip runs offline from the
# wheel it bundles. A second implementation of this for the EC2 backend would
# have copied all of it, which is the two-pins problem internal/runnerimages
# exists to prevent.
#
# SOURCED, NOT EXECUTED. These are bash functions -- arrays, namerefs and
# ${x/y/z} substitution -- so a POSIX sh caller must run a bash driver that
# sources this file and calls billet_install_toolcache in the same process.
# Function definitions do not cross a process boundary.
#
# THE CALLER SETS THESE, and every one of them is required:
#
#   BILLET_TC_ROOT       the target's root as the CALLER sees it, or "" when the
#                        target is the machine running this
#   BILLET_TC_DIR        the toolcache directory as the CALLER sees it
#   BILLET_TC_IN_TARGET  the toolcache directory as the TARGET sees it
#   BILLET_TC_WORK       a scratch directory on the caller's filesystem
#   BILLET_TC_TOOLSET    the pinned toolset-2404.json, as the caller sees it
#   BILLET_TC_ENV_FILE   the KEY=VALUE image env file, as the caller sees it
#
# shellcheck shell=bash

# billet_tc_run runs a command inside the target.
#
# THE ONE PLACE THE TWO CALLERS DIFFER. The guest build assembles a filesystem it
# is not running, so anything that must see the target's own libraries, apt or
# interpreter goes through chroot. The AMI build IS the target, so the same
# command runs directly. Everything else in this file is identical for both.
billet_tc_run() {
	if [ -n "$BILLET_TC_ROOT" ]; then
		chroot "$BILLET_TC_ROOT" "$@"
	else
		"$@"
	fi
}

# toolset_versions prints the version globs GitHub declares for one toolcache tool.
#
# GLOBS, NOT VERSIONS: upstream declares "3.13.*" and "22.*", which is a statement
# about which LINES a workflow may ask for rather than which patch it gets. Each
# is resolved against the vendor's own manifest at build time, because a toolcache
# is a cache and not a contract -- `node-version: 22` does not care which patch it
# finds, and pinning would ship stale runtimes for a reproducibility nobody wants.
#
# THE TOOL NAME IS SPELLED AS UPSTREAM SPELLS IT: "Python" and "Ruby" are
# capitalised while "node" and "go" are not, which is an inconsistency in the
# actions themselves. Passing the wrong case here silently yields no versions,
# which is why the callers refuse an empty answer.
toolset_versions() {
	jq -r --arg name "$1" '
		.toolcache[] | select(.name == $name) | .versions[]
	' "$BILLET_TC_TOOLSET"
}

# read_toolset_versions fills an array with one tool's declared version globs, or
# fails naming the tool.
#
# AN ARRAY, NOT A STRING SPLIT BY THE SHELL. `for glob in $globs` word-splits AND
# GLOB-EXPANDS: the values here are literally `22.*` and `3.13.*`, so a file named
# `22.foo` in whatever directory the build was started from would replace the
# version being resolved with a filename. Reading into an array and iterating
# "${arr[@]}" does neither.
#
# IT ALSO REFUSES AN EMPTY ANSWER RATHER THAN LOOPING ZERO TIMES. A missing tool,
# an empty version list, or a jq error all produce no output, and a loop that runs
# zero times installs nothing and reports success -- which is how a toolcache ends
# up silently absent while every gate above it passes. Upstream's tool names are
# case-sensitive and inconsistent (`Python` and `Ruby`, but `node` and `go`), so
# the case that produces "no versions" is a plausible edit rather than a remote one.
read_toolset_versions() {
	local -n out="$1"
	local tool="$2"

	out=()

	local line
	while IFS= read -r line; do
		# WHITESPACE-ONLY IS EMPTY. A manifest value of " " is non-empty to a
		# string test and yields nothing to iterate, which is the vacuous case
		# wearing a disguise.
		[ -n "$(printf '%s' "$line" | tr -d '[:space:]')" ] || continue

		out+=("$line")
	done < <(toolset_versions "$tool")

	if [ "${#out[@]}" -eq 0 ]; then
		echo "the toolset declares no $tool versions; refusing to bake a toolcache that" >&2
		echo "silently differs from what github's image offers. Note the tool names are" >&2
		echo "case-sensitive: Python and Ruby are capitalised, node and go are not." >&2
		return 1
	fi

	return 0
}

# NO TOOLCACHE_DIR HERE. Nothing in this file reads it -- the installers use
# BILLET_TC_IN_TARGET, which the caller states -- and defining it in both files
# meant the sourced copy silently won. One value, one definition; the reasoning
# lives with the definition, in build-guest-image.sh.

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
	local rootfs="$BILLET_TC_ROOT"
	local tc="$BILLET_TC_DIR"

	mkdir -p "$tc"

	# PYTHON'S EXTENSION MODULES LINK LIBRARIES THE TARBALL DOES NOT BUNDLE.
	# Without these, the interpreter starts and `import sqlite3` fails with a
	# loader error naming a .so, which reads as a broken workflow rather than a
	# missing package.
	billet_tc_run /bin/bash -euxc '
		export DEBIAN_FRONTEND=noninteractive
		apt-get -o DPkg::Lock::Timeout=600 update -qq
		apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends \
			libsqlite3-0 libreadline8t64 libgdbm6t64 libgdbm-compat4t64 \
			libbz2-1.0 liblzma5 libffi8 libuuid1 libncursesw6
		apt-get clean
		rm -rf /var/lib/apt/lists/*
	'

	install_node_toolcache "$tc"
	install_go_toolcache "$tc"
	install_python_toolcache "$rootfs" "$tc"
	install_java_toolcache "$rootfs" "$tc"
	install_pypy_toolcache "$rootfs" "$tc"
	install_ruby_toolcache "$rootfs" "$tc"
	install_codeql_toolcache "$rootfs" "$tc"

	# THE TOOLCHAINS, WHICH ARE NOT TOOLCACHE ENTRIES. cmake, pwsh and the dotnet
	# SDKs go on PATH rather than under <tool>/<version>/<arch>, because that is
	# where their actions look: setup-dotnet reads DOTNET_ROOT, and nothing resolves
	# pwsh or cmake through @actions/tool-cache at all. They live in this file
	# because it is the one installer surface both backends share.
	install_cmake
	install_powershell
	install_dotnet
	install_dotnet_tools
	install_powershell_modules
	install_android
	install_clang_default

	# AFTER THE TOOLCACHE, BECAUSE IT LINKS INTO IT. The default runtimes are
	# symlinks to entries the installers above created, so this cannot run first.
	install_default_runtimes "$tc"
	install_global_packages

	# READ AND WRITTEN BY EVERY JOB, which runs as the unprivileged runner account.
	#
	# -h SO SYMLINKS ARE NOT FOLLOWED. Java's toolcache entries are symlinks into
	# /usr/lib/jvm; without -h this would chmod the JDK trees through them, which
	# is both wrong and slow. The JDKs get their own permissions where they are
	# installed.
	# `find -P`, NOT `chmod -R -h`, AND THE REASON IS A PORTABILITY UNCERTAINTY I
	# COULD NOT RESOLVE. `-h` is documented by GNU coreutils 9.7 and accepted by
	# the uutils chmod on the reference host; whether the 9.4 that ships on
	# ubuntu-24.04 -- the runner the scheduled build uses -- accepts it, I could not
	# establish. `find -P` needs no answer: it never traverses a symlink, so the
	# JDK trees the toolcache links into are left alone on every implementation.
	#
	# THE REAL BUILD COULD NOT HAVE CAUGHT THIS. Ubuntu 26.04 ships uutils
	# coreutils, so the three end-to-end builds all ran against a chmod that
	# accepts the flag. That is the same trap CLAUDE.md records for `sh` and
	# `sleep` in the firecracker tests: green on the machine it was written on,
	# and possibly wrong on the machine it runs on.
	# `! -type l` IS LOAD-BEARING, not tidiness. Linux has no per-symlink mode, so
	# chmod on a link fails with EPERM even as root -- and with `-exec ... +` that
	# failure propagates out of find and kills the build under `set -e`. Measured:
	# the first version of this line, without the predicate, died on the Java
	# entries, which are symlinks by design.
	find -P "$tc" ! -type l -exec chmod 0777 {} +

	echo "toolcache: $(du -sh "$tc" | cut -f1)"
}

# java_toolcache_version turns a JDK's own release metadata into a version string
# @actions/tool-cache will match.
#
# MEASURED AGAINST ALL FIVE TEMURIN RELEASES, not read off upstream's script. Two
# things are true of the real files and neither is obvious:
#
#   - THE FIELD IS `SEMANTIC_VERSION`, NOT `SEMANTIC`. Every one of 8, 11, 17, 21
#     and 25 spells it that way. Upstream greps `^SEMANTIC`, which matches the
#     longer name by prefix; anchoring on `SEMANTIC=` matches NONE of them, which
#     is what the first version of this did -- it failed on the first JDK in the
#     loop and would have failed on all five.
#
#   - THREE OF THE FIVE ARE NOT SEMVER. 17.0.20.1+1, 21.0.12.1+1 and 25.0.4.1+1
#     carry a fourth component, and tool-cache keeps only directories that parse
#     as an explicit version -- so those entries would sit on disk complete and
#     invisible to every range request. The fourth component is dropped, which is
#     what upstream does and what its comment attributes to temurin-build#2248.
#
# `+` BECOMES `-` because a `+` in semver is build metadata, which is ignored in
# comparisons; a `-` makes it a prerelease that orders predictably.
java_toolcache_version() {
	local release="$1" javabin="$2"

	local version
	version=$(sed -n 's/^SEMANTIC[A-Z_]*="\(.*\)"$/\1/p' "$release" | head -1)

	# THE FALLBACK IS THE JDK ITSELF. A release file that carries no semantic
	# version at all still has a binary that reports one, and asking it is better
	# than refusing to name an entry for a JDK that is installed and working.
	if [ -z "$version" ] && [ -x "$javabin" ]; then
		version=$("$javabin" -fullversion 2>&1 | tr -d '"' | awk '{print $4}')
	fi

	[ -n "$version" ] || return 1

	version=$(printf '%s' "$version" | tr '+' '-')

	# FOUR COMPONENTS BEFORE THE PRERELEASE -> THREE. 17.0.20.1-1 becomes
	# 17.0.20-1.
	case "$version" in
		[0-9]*.[0-9]*.[0-9]*.[0-9]*-*) version=$(printf '%s' "$version" | sed -E 's/\.[0-9]+-/-/') ;;
	esac

	# AND TOO FEW COMPONENTS ARE PADDED. `8-1` and `8.0-1` are both things a JDK
	# can report, and neither parses as an explicit version.
	case "$version" in
		[0-9]*.[0-9]*.[0-9]*) ;;
		[0-9]*.[0-9]*-*) version=$(printf '%s' "$version" | sed -E 's/-/.0-/') ;;
		[0-9]*-*) version=$(printf '%s' "$version" | sed -E 's/-/.0.0-/') ;;
	esac

	printf '%s\n' "$version"
}

# install_java_toolcache installs every JDK github's image declares.
#
# FROM ADOPTIUM'S OWN APT REPOSITORY, WHICH IS WHAT UPSTREAM DOES. The alternative
# -- downloading tarballs -- means picking a build for each version and checking
# it by hand, while the repository is signed and apt verifies every package
# against a key installed here explicitly rather than through the deprecated
# `apt-key`. Temurin is GPLv2 with the Classpath Exception, so it is
# redistributable inside an image billet publishes; that is NOT true of every JDK
# and is the reason this is Temurin rather than a vendor build.
#
# THE TOOLCACHE ENTRY IS A SYMLINK, NOT A COPY. Five JDKs are well over a
# gigabyte, and the packages already put them in /usr/lib/jvm; copying each into
# the toolcache would double that for nothing. setup-java resolves the entry and
# then uses the path it finds, which a symlink satisfies.
#
# THE ENTRY'S VERSION IS THE FULL SEMVER FROM THE JDK'S OWN release FILE, not the
# feature version. `Java_Temurin-Hotspot_jdk/17/x64` is invisible to a range
# request for the reason node's directories must be full semvers; upstream reads
# SEMANTIC out of that file, and so does this.
install_java_toolcache() {
	local rootfs="$1" tc="$2"

	local versions default
	versions=$(jq -r '.java.versions[]' "$BILLET_TC_TOOLSET")
	default=$(jq -r '.java.default' "$BILLET_TC_TOOLSET")

	if [ -z "$versions" ] || [ -z "$default" ] || [ "$default" = "null" ]; then
		echo "the toolset declares no java versions or no default; refusing to bake a" >&2
		echo "toolcache that silently differs from what github's image offers" >&2
		exit 1
	fi

	# THE DEFAULT MUST BE ONE OF THE VERSIONS THAT GETS INSTALLED. Nothing else
	# checks this: JAVA_HOME is written from `default` while the JDKs come from
	# `versions`, so a declaration naming a default outside that set produces a
	# JAVA_HOME pointing at a directory the build never created -- and a JAVA_HOME
	# that names nothing is worse than an absent one, because setup-java and every
	# build tool downstream trust it instead of installing a JDK.
	# A HERE-STRING FOR THE REASON check-guest-image.sh RECORDS AT LENGTH: under
	# pipefail, `grep -q` exiting on an early match leaves the writer with SIGPIPE
	# and the pipeline with 141, which reads as "not found" for a value that is
	# there. This list is short enough that it has never fired; that is luck about
	# a buffer rather than a property of the code.
	if ! grep -qxF "$default" <<<"$versions"; then
		echo "the toolset makes java $default the default and does not list it among the" >&2
		echo "versions to install ($(printf '%s' "$versions" | tr '\n' ' ')); JAVA_HOME would" >&2
		echo "name a directory this build never creates" >&2
		exit 1
	fi

	local packages=()
	local v
	for v in $versions; do
		packages+=("temurin-$v-jdk")
	done

	# THE KEY IS DEARMORED INTO A KEYRING THIS SOURCE NAMES, rather than added to
	# the global trust store. A key in /etc/apt/trusted.gpg.d signs anything in any
	# repository; `signed-by` binds it to this one.
	# -o pipefail, BECAUSE THE FIRST COMMAND INSIDE IS A PIPELINE. Adoptium's key
	# is fetched with `curl | gpg --dearmor`, and without pipefail a pipeline's
	# status is its LAST command's -- so a curl that 404s or times out leaves gpg
	# writing a ZERO-BYTE keyring and `bash -eux` carrying on. Measured: the line
	# after `false | cat` runs and the driver exits 0, and the keyring file is
	# there and empty. apt would then fail on an unsigned repository, which is a
	# worse place to find out.
	billet_tc_run /bin/bash -euxo pipefail -s -- "${packages[@]}" <<'JAVA'
export DEBIAN_FRONTEND=noninteractive

curl -fsSL --retry 5 --retry-all-errors \
	https://packages.adoptium.net/artifactory/api/gpg/key/public |
	gpg --dearmor >/usr/share/keyrings/adoptium.gpg

. /etc/os-release
printf 'deb [signed-by=/usr/share/keyrings/adoptium.gpg] %s %s main\n' \
	"https://packages.adoptium.net/artifactory/deb/" "$VERSION_CODENAME" \
	>/etc/apt/sources.list.d/adoptium.list

apt-get -o DPkg::Lock::Timeout=600 update -qq
apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends "$@"

# ANT, MAVEN AND GRADLE COME FROM THE ARCHIVE, not from upstream's hand-rolled
# downloads. runner-images fetches Maven and Gradle as zips because it wants the
# newest; the distribution packages are what a workflow's `mvn` and `gradle`
# resolve to either way, and they are signed by the same archive key as
# everything else here.
apt-get -o DPkg::Lock::Timeout=600 install -y --no-install-recommends \
	ant ant-optional maven gradle

# READ AND EXECUTED BY THE UNPRIVILEGED RUNNER ACCOUNT. The packages install
# root-owned and mode 0755, which is already sufficient to read and execute --
# upstream's 0777 exists because its jobs may WRITE into the JDK, and a job that
# needs that can sudo.
chmod -R a+rX /usr/lib/jvm

apt-get clean
rm -rf /var/lib/apt/lists/*

# THE REPOSITORY IS REMOVED AFTER THE INSTALL, as upstream does. A published
# image that still lists a third-party apt source would have every job's
# `apt-get update` reach for it, which is a network dependency and a trust
# relationship no workflow asked for.
rm -f /etc/apt/sources.list.d/adoptium.list /usr/share/keyrings/adoptium.gpg
JAVA

	local jvm="$rootfs/usr/lib/jvm"
	local cache="$tc/Java_Temurin-Hotspot_jdk"

	for v in $versions; do
		local home="$jvm/temurin-$v-jdk-$BILLET_TC_DPKG"

		if [ ! -d "$home" ]; then
			echo "temurin-$v-jdk installed nothing at $home; the package layout changed" >&2
			exit 1
		fi

		local semver
		if ! semver=$(java_toolcache_version "$home/release" "$home/bin/java"); then
			echo "temurin-$v-jdk records no semantic version in $home/release and its java" >&2
			echo "binary reports none either, so nothing can name its toolcache entry a" >&2
			echo "version setup-java will match" >&2
			exit 1
		fi

		mkdir -p "$cache/$semver"
		# -T, BECAUSE WITHOUT IT AN EXISTING DIRECTORY SWALLOWS THE LINK. `ln -sfn`
		# onto a path that is a real directory creates the link INSIDE it -- so a
		# rerun, or a JDK that somehow unpacked to that name, would leave
		# `<version>/x64/temurin-17-jdk-amd64` and an x64 that is not the JDK.
		# tool-cache would find the entry, resolve nothing usable, and the failure
		# would arrive in a job. With -T ln refuses and the build stops here.
		ln -sfnT "/usr/lib/jvm/temurin-$v-jdk-$BILLET_TC_DPKG" \
			"$cache/$semver/$BILLET_TC_ARCH"
		touch "$cache/$semver/$BILLET_TC_ARCH.complete"

		echo "toolcache: java $v ($semver)"
	done

	# THE ENVIRONMENT EVERY HOSTED RUNNER EXPORTS, appended to the file the agent
	# reads. setup-java looks up JAVA_HOME_<version>_X64 to find a JDK already on
	# the machine, and a workflow that pins a toolchain by variable reads them
	# directly -- so an image carrying the JDKs without these has them installed
	# and unfindable, which is the toolcache failure one directory over.
	for v in $versions; do
		printf 'JAVA_HOME_%s_X64=/usr/lib/jvm/temurin-%s-jdk-%s\n' "$v" "$v" \
			"$BILLET_TC_DPKG" \
			>>"$BILLET_TC_ENV_FILE"
	done

	printf 'JAVA_HOME=/usr/lib/jvm/temurin-%s-jdk-%s\n' "$default" "$BILLET_TC_DPKG" \
		>>"$BILLET_TC_ENV_FILE"
	printf 'ANT_HOME=/usr/share/ant\n' >>"$BILLET_TC_ENV_FILE"
}

# fetch_verified downloads a file and checks it against a digest.
fetch_verified() {
	local url="$1" out="$2" want="$3" algo="${4:-sha256}"

	curl -fL -sS --http1.1 --connect-timeout 20 --max-time 900 \
		--retry 5 --retry-delay 5 --retry-all-errors -o "$out" "$url"

	# SHA-512 EXISTS HERE BECAUSE .NET PUBLISHES ONLY THAT. Its release metadata
	# carries a 128-hex-character hash per file, which sha256sum reads as a
	# malformed line rather than a mismatch -- so without this the choice was
	# verifying dotnet against nothing or not baking it.
	#
	# AN UNKNOWN ALGORITHM IS A REFUSAL, NOT A SKIP. A default that fell through to
	# "no check" would turn a typo in a caller into an unverified download, which is
	# the one outcome this function exists to make impossible.
	# A HERE-STRING, NOT A PIPE. `echo "$want  $out" | sha256sum -c -` has a writer,
	# and a writer under pipefail can report SIGPIPE as the pipeline's status --
	# the trap that already cost this file two rounds elsewhere. There is no reason
	# to keep one here.
	local ok=0

	case "$algo" in
		sha256) sha256sum -c - >/dev/null <<<"$want  $out" || ok=$? ;;
		sha512) sha512sum -c - >/dev/null <<<"$want  $out" || ok=$? ;;
		*)
			echo "fetch_verified was asked for the $algo digest of $url, which it cannot" >&2
			echo "  compute; refusing rather than leaving the download unverified" >&2
			ok=1
			;;
	esac

	# EVERY REFUSAL REMOVES THE FILE, not only the unknown-algorithm one. The two
	# branches disagreed: a download whose checksum did not match was left on disk,
	# so anything that unpacks by PATH rather than by return value got the
	# unverified bytes. `set -e` happens to stop the build before that today, which
	# makes it a latent defect rather than a live one -- and a verifier whose safety
	# depends on its caller's shell options is not one. A test written for the
	# unknown-algorithm branch is what found it.
	if [ "$ok" -ne 0 ]; then
		rm -f "$out"

		return 1
	fi
}

# python_release_tag binds manifest coordinates to the only release namespace the
# image builder trusts, before either filename is turned into a download URL.
python_release_tag() {
	local release_url="$1" filename="$2"
	local release_prefix="https://github.com/actions/python-versions/releases/tag/"

	if [[ "$release_url" != "$release_prefix"* ]]; then
		echo "python release is not from actions/python-versions: $release_url" >&2
		return 1
	fi

	local tag="${release_url#"$release_prefix"}"
	if [[ ! "$tag" =~ ^[A-Za-z0-9._-]+$ ]] ||
		[[ ! "$filename" =~ ^[A-Za-z0-9._-]+$ ]] ||
		[[ "$tag" == "." || "$tag" == ".." || "$filename" == "." || "$filename" == ".." ]]; then
		echo "python release has an unsafe tag or asset name; refusing to derive a download URL" >&2
		return 1
	fi

	printf '%s\n' "$tag"
}

# python_release_checksum reads the digest the actions/python-versions release
# published beside its archives. A digest calculated from a second download of the
# archive proves only that both downloads agreed; it gives an altered archive the
# authority to attest to itself.
python_release_checksum() {
	local release_url="$1" filename="$2"
	local tag
	tag=$(python_release_tag "$release_url" "$filename")

	local hashes_url="https://github.com/actions/python-versions/releases/download/$tag/hashes.sha256"
	local want
	if ! want=$(curl -fsSL --retry 3 --retry-all-errors "$hashes_url" |
		awk -v filename="$filename" '
			{
				hash = substr($0, 1, 64)
				separator = substr($0, 65, 1)
				target = substr($0, 66)
				sub(/^\*/, "", target)
				if (target == filename) {
					count++
					record_hash = hash
					if (separator != " " ||
						(substr($0, 66) != filename &&
						 substr($0, 66) != "*" filename)) invalid = 1
				}
			}
			END {
				if (count != 1 || invalid || length(record_hash) != 64 ||
					record_hash ~ /[^0-9A-Fa-f]/) exit 1
				print record_hash
			}'); then
		echo "no single published checksum for python asset $filename; refusing to bake it" >&2
		return 1
	fi

	printf '%s\n' "$want"
}

# fetch_python_toolcache resolves one manifest entry and verifies its archive
# against the release's independent hashes file before returning it to the build.
fetch_python_toolcache() {
	local manifest="$1" version="$2" out="$3"
	local fields
	if ! fields=$(printf '%s' "$manifest" | jq -er --arg v "$version" \
		--arg a "$BILLET_TC_ARCH" '
		[.[] | select(.version == $v)] as $releases
		| select(($releases | length) == 1)
		| $releases[0] as $release
		| [$release.files[] | select(.platform == "linux"
			and .platform_version == "24.04" and .arch == $a)] as $files
		| select(($files | length) == 1)
		| [$release.release_url, $files[0].filename, $files[0].download_url] as $asset
		| select(all($asset[]; type == "string" and length > 0))
		| $asset | @tsv'); then
		echo "python $version has no single linux 24.04 $BILLET_TC_ARCH release asset" >&2
		return 1
	fi

	local release_url filename url
	IFS=$'\t' read -r release_url filename url <<<"$fields"

	local tag
	tag=$(python_release_tag "$release_url" "$filename")
	local expected_url="https://github.com/actions/python-versions/releases/download/$tag/$filename"
	if [ "$url" != "$expected_url" ]; then
		echo "python $version manifest asset is not the file published by its release" >&2
		return 1
	fi

	local want
	want=$(python_release_checksum "$release_url" "$filename")
	fetch_verified "$url" "$out" "$want"
}

# install_node_toolcache bakes in the two newest LTS lines.
#
# THE VERSION DIRECTORY MUST BE A FULL SEMVER. @actions/tool-cache resolves a range
# by listing the version directories and keeping only those that parse as explicit
# semver, so a directory named `20` is invisible to `node-version: 20` -- it does
# not match loosely, it is skipped entirely.
install_node_toolcache() {
	local tc="$1"

	# THE LINES COME FROM GITHUB'S DECLARATION, THE PATCHES FROM NODE.
	#
	# This used to take "the two newest LTS lines", which is a rule that happened to
	# agree with upstream and had no reason to keep agreeing: the moment GitHub
	# ships a non-LTS line, or three, a workflow asking for what a hosted runner has
	# finds nothing here and silently downloads it. The declaration says which lines
	# a workflow may ask for; node's own index says which patch each resolves to.
	local index
	index=$(curl -fsSL --retry 3 --retry-all-errors https://nodejs.org/dist/index.json)

	local globs
	read_toolset_versions globs node

	local versions=""

	local glob
	for glob in "${globs[@]}"; do
		local resolved
		resolved=$(printf '%s' "$index" | jq -r --arg p "${glob%\*}" '
			[.[] | select((.version | ltrimstr("v")) | startswith($p))]
			| sort_by(.version | ltrimstr("v") | split(".") | map(tonumber))
			| last | .version // empty')

		if [ -z "$resolved" ]; then
			echo "node publishes nothing matching $glob, which github's image declares;" >&2
			echo "refusing to ship a toolcache missing a line a workflow may ask for" >&2
			exit 1
		fi

		versions="$versions $resolved"
	done

	local v
	for v in $versions; do
		local bare="${v#v}"
		local file="node-$v-linux-$BILLET_TC_ARCH.tar.gz"
		local want

		# THE CHECKSUM COMES FROM THE RELEASE ITSELF, published beside the tarball.
		want=$(curl -fsSL --retry 3 --retry-all-errors "https://nodejs.org/dist/$v/SHASUMS256.txt" |
			awk -v f="$file" '$2 == f {print $1}')

		if [ -z "$want" ]; then
			echo "no published checksum for node $v; refusing to bake an unverified runtime" >&2
			exit 1
		fi

		fetch_verified "https://nodejs.org/dist/$v/$file" "$BILLET_TC_WORK/node.tgz" "$want"

		local dir="$tc/node/$bare/$BILLET_TC_ARCH"
		mkdir -p "$dir"

		# STRIPPED, because setup-node adds `<dir>/bin` to PATH and the tarball has
		# everything under a `node-vX-linux-x64/` component.
		tar -xzf "$BILLET_TC_WORK/node.tgz" -C "$dir" --strip-components=1
		rm -f "$BILLET_TC_WORK/node.tgz"

		# THE MARKER IS A SIBLING OF THE ARCH DIRECTORY, not inside it, and its
		# absence makes the entry invisible however complete it is: tool-cache stats
		# `<version>/<arch>.complete` and treats a missing one as a half-extracted
		# download.
		touch "$tc/node/$bare/$BILLET_TC_ARCH.complete"

		echo "toolcache: node $bare"
	done
}

# install_go_toolcache bakes in every go line github's image declares.
#
# EVERY LINE, NOT JUST THE NEWEST. This installed only the current release, which
# meant a workflow pinning `go-version: 1.24` -- the version GitHub's own image
# makes DEFAULT -- downloaded a toolchain on a machine advertising a go toolcache.
# `--mode=json` alone lists only supported stable releases, which is why the older
# lines need `include=all`.
install_go_toolcache() {
	local tc="$1"

	local meta
	meta=$(curl -fsSL --retry 3 --retry-all-errors 'https://go.dev/dl/?mode=json&include=all')

	local globs
	read_toolset_versions globs go

	local glob
	for glob in "${globs[@]}"; do
		# THE NEWEST PATCH ON THAT LINE. go.dev lists newest first, but sorting
		# numerically rather than trusting the order means a feed that changed its
		# ordering cannot silently downgrade a runtime.
		# THE LINE ITSELF MATCHES, NOT ONLY ITS PATCHES. go names an initial release
		# "go1.26" rather than "go1.26.0", so the prefix "go1.26." misses exactly
		# the release that exists on the day a line is new -- and the `.0`
		# normalization below could never fire, because nothing it could normalize
		# was ever selected. `$line` is the bare form, `$p` the patch prefix.
		local version want
		version=$(printf '%s' "$meta" |
			jq -r --arg p "go${glob%\*}" --arg line "go${glob%.\*}" '
			[.[] | select(.version == $line or (.version | startswith($p)))]
			| sort_by(.version | ltrimstr("go") | split(".") | map(tonumber))
			| last | .version // empty')

		if [ -z "$version" ]; then
			echo "go publishes nothing matching $glob, which github's image declares;" >&2
			echo "refusing to ship a toolcache missing a line a workflow may ask for" >&2
			exit 1
		fi

		want=$(printf '%s' "$meta" | jq -r --arg v "$version" --arg a "$BILLET_TC_DPKG" '
			.[] | select(.version == $v) | .files[]
			| select(.filename == ($v + ".linux-" + $a + ".tar.gz")) | .sha256')

		if [ -z "$want" ] || [ "$want" = "null" ]; then
			echo "go published no checksum for $version; refusing to bake an unverified runtime" >&2
			exit 1
		fi

		fetch_verified "https://go.dev/dl/$version.linux-$BILLET_TC_DPKG.tar.gz" \
			"$BILLET_TC_WORK/go.tgz" "$want"

		# THE DIRECTORY IS A BARE SEMVER WITH NO `go` PREFIX. setup-go coerces its
		# version through semver before looking, so `go1.26.6` on disk is never found.
		#
		# AND IT MUST BE A FULL SEMVER. go publishes an initial release as "go1.26"
		# rather than "go1.26.0", and @actions/tool-cache skips a directory that does
		# not parse as an explicit version -- so that entry would be invisible to
		# every range request while looking present on disk.
		local bare="${version#go}"
		case "$bare" in
			*.*.*) ;;
			*) bare="$bare.0" ;;
		esac

		local dir="$tc/go/$bare/$BILLET_TC_ARCH"

		mkdir -p "$dir"
		tar -xzf "$BILLET_TC_WORK/go.tgz" -C "$dir" --strip-components=1
		rm -f "$BILLET_TC_WORK/go.tgz"
		touch "$tc/go/$bare/$BILLET_TC_ARCH.complete"

		echo "toolcache: go $bare"
	done
}

# install_python_toolcache bakes in every minor github's image declares.
#
# THE TOOL NAME IS CAPITALISED. setup-python looks for `Python`, while setup-node
# and setup-go look for `node` and `go` -- an inconsistency in the actions
# themselves, and one that fails silently on a case-sensitive filesystem: the
# directory exists, nothing finds it, and the job downloads a runtime anyway.
install_python_toolcache() {
	local rootfs="$1" tc="$2"

	local manifest
	manifest=$(curl -fsSL --retry 3 --retry-all-errors \
		https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json)

	local globs
	read_toolset_versions globs Python

	# ONE MINOR PER DECLARED GLOB, resolved to its newest stable patch.
	#
	# This took "the two newest minors", which is a rule that only coincidentally
	# tracked upstream: GitHub declares five, so a workflow pinning 3.10 -- still
	# supported, still on a hosted runner -- found nothing and downloaded an
	# interpreter while the image advertised a python toolcache.
	#
	# STABLE ONLY, AND NOT THE FREE-THREADED BUILD. The free-threaded interpreter is
	# published as a separate arch (`x64-freethreaded`) beside the ordinary one, and
	# baking it in would give workflows an interpreter with different semantics and
	# nothing in the logs to explain why.
	local versions=""

	local glob
	for glob in "${globs[@]}"; do
		local resolved
		resolved=$(printf '%s' "$manifest" | jq -r --arg p "${glob%\*}" \
			--arg a "$BILLET_TC_ARCH" '
			[.[] | select(.stable == true)
			     | select(.version | startswith($p))
			     | select(any(.files[]; .platform == "linux"
			                        and .platform_version == "24.04"
			                        and .arch == $a))]
			| sort_by(.version | split(".") | map(tonumber))
			| last | .version // empty')

		if [ -z "$resolved" ]; then
			echo "actions/python-versions publishes no stable 24.04 $BILLET_TC_ARCH build" >&2
			echo "  matching $glob," >&2
			echo "which github's image declares; refusing to ship a toolcache missing a" >&2
			echo "version a workflow may ask for" >&2
			exit 1
		fi

		versions="$versions $resolved"
	done

	local v
	for v in $versions; do
		fetch_python_toolcache "$manifest" "$v" "$BILLET_TC_WORK/python.tgz"

		local dir="$tc/Python/$v/$BILLET_TC_ARCH"
		mkdir -p "$dir"

		# NOT STRIPPED. This tarball's root already holds bin/, lib/ and setup.sh.
		tar -xzf "$BILLET_TC_WORK/python.tgz" -C "$dir"
		rm -f "$BILLET_TC_WORK/python.tgz"

		# WHAT setup.sh WOULD HAVE DONE, minus only the part that needs a network.
		#
		# The tarball ships NO `python` or `pip` entry point -- only `python3.13` and
		# friends -- and no working pip until `ensurepip` runs, because setup.sh does
		# both. Skipping ALL of setup.sh (as this once did) left an interpreter with no
		# `pip` executable AND no pip MODULE, so setup-python's `cache: pip` (which
		# execs `pip`) and every `python -m pip` failed the instant a workflow used the
		# baked version -- the fast path this cache exists for. The missing work is the
		# entry-point symlinks below, then ensurepip: OFFLINE, from the wheel the
		# checksum-verified tarball bundles. Only setup.sh's subsequent PyPI upgrade of
		# pip needs a network and stays skipped; a workflow needing newer pip upgrades it.
		local minor="${v%.*}"

		ln -sf "./bin/python$minor" "$dir/python"
		ln -sf "python$minor" "$dir/bin/python"
		ln -sf "python$minor" "$dir/bin/python${minor/./}"

		# ensurepip supplies the pip MODULE; a force-reinstall of the bundled wheel then
		# deterministically (re)creates the `pip` CONSOLE SCRIPT -- ensurepip alone can
		# decide pip is already satisfied and leave bin/pip absent. Run in the chroot so
		# the generated shebangs carry this non-relocatable distribution's absolute
		# /opt/hostedtoolcache interpreter path, valid in the running guest.
		local guest_dir="$BILLET_TC_IN_TARGET/Python/$v/$BILLET_TC_ARCH"
		local bundled=("$dir/lib/python$minor/ensurepip/_bundled"/pip-*.whl)
		if [ "${#bundled[@]}" -ne 1 ] || [ ! -f "${bundled[0]}" ]; then
			echo "python $v has no single bundled pip wheel" >&2
			exit 1
		fi
		billet_tc_run "$guest_dir/bin/python$minor" -m ensurepip --default-pip >/dev/null
		billet_tc_run "$guest_dir/bin/python$minor" -m pip install \
			--no-index --force-reinstall --disable-pip-version-check --no-warn-script-location \
			"${bundled[0]#"$rootfs"}" >/dev/null
		if [ ! -x "$dir/bin/pip" ]; then
			echo "python $v baked without a pip entry point" >&2
			exit 1
		fi

		rm -f "$dir/setup.sh" "$dir/build_output.txt" "$dir/tools_structure.txt"

		touch "$tc/Python/$v/$BILLET_TC_ARCH.complete"

		echo "toolcache: python $v"
	done
}

# BILLET_TC_UNPUBLISHED is the file that keeps the gate honest about a line
# nobody can supply.
#
# THE RULE IS NARROWED, NOT BROKEN. A declared line billet FAILED to fetch is
# still a refusal -- that is what stops an image quietly missing a runtime a
# workflow asks for. What is different is a line the vendor has published NOTHING
# for: github declares Ruby 4.0.* and ruby-builder has no such asset among its 919,
# because Ruby 4.0 is not released. That is a declaration ahead of reality rather
# than a download that went wrong, and refusing it means no image can be built at
# all until upstream catches up.
#
# SO IT IS RECORDED, IN THE IMAGE. Skipping quietly is the failure this project
# keeps removing, so the skip is written where the gate can read it: every declared
# line must be either present in the toolcache or named in this file. The gate
# stays offline -- it reads a filesystem, not an API -- and cannot pass by
# accident, and the day Ruby 4.0 ships the line stops being recorded and starts
# being installed with no edit anywhere.
BILLET_TC_UNPUBLISHED=.billet-unpublished

# THE ARCHITECTURE, SPELLED SIX DIFFERENT WAYS BY SEVEN VENDORS.
#
# BILLET_TC_ARCH is what @actions/tool-cache looks for -- the `<version>/<arch>`
# directory and the `<arch>.complete` marker beside it -- and it is `x64` or
# `arm64` because those are the names node's `os.arch()` produces. Everything
# else here is a translation of it, and NOT ONE of them is derivable from
# another:
#
#   node      linux-x64        / linux-arm64      (the tool-cache spelling)
#   Python    -x64             / -arm64           (the tool-cache spelling)
#   go        linux-amd64      / linux-arm64      (dpkg's amd64, not x64)
#   temurin   -jdk-amd64       / -jdk-arm64       (dpkg's, in a filesystem path)
#   pypy      x64              / aarch64          (the kernel's name, for one arch)
#   ruby      <bare>           / -arm64           (x64 carries NO suffix at all)
#   cmake     x86_64           / aarch64          `uname -m`, a sixth spelling
#   codeql    linux64          / nothing at all
#
# MEASURED AGAINST EACH VENDOR'S OWN INDEX, not inferred from the x64 spelling.
# Two of those rows are traps: pypy calls arm64 `aarch64` where every neighbour
# says arm64, and ruby-builder's x64 asset has no arch suffix while its arm64 one
# does -- so a single substitution would break both, in opposite directions.
billet_tc_set_arch() {
	case "${BILLET_TC_ARCH:-}" in
		x64)
			BILLET_TC_DPKG=amd64
			BILLET_TC_PYPY_ARCH=x64
			BILLET_TC_RUBY_SUFFIX=""
			BILLET_TC_UNAME=x86_64
			;;
		arm64)
			BILLET_TC_DPKG=arm64
			BILLET_TC_PYPY_ARCH=aarch64
			BILLET_TC_RUBY_SUFFIX="-arm64"
			BILLET_TC_UNAME=aarch64
			;;
		*)
			echo "BILLET_TC_ARCH is \"${BILLET_TC_ARCH:-}\"; it must be x64 or arm64, which" >&2
			echo "  are the names @actions/tool-cache resolves against" >&2
			return 1
			;;
	esac

	return 0
}

# billet_tc_unpublished records a declared line the vendor publishes nothing for.
billet_tc_unpublished() {
	local tool="$1" glob="$2" where="$3"

	printf '%s %s\n' "$tool" "$glob" >>"$BILLET_TC_DIR/$BILLET_TC_UNPUBLISHED"

	echo "toolcache: $tool $glob is declared and $where publishes nothing for it;" >&2
	echo "  recorded as unpublished rather than failing the build. A workflow asking" >&2
	echo "  for that line will download a runtime, which is the same thing it would" >&2
	echo "  do on github's own image today." >&2
}

# install_pypy_toolcache bakes in a PyPy for every python line github declares.
#
# THE DIRECTORY IS THE PYTHON VERSION, NOT THE PYPY VERSION, and that is the trap
# here. pypy 7.3.23 implements python 3.11.15, and setup-python resolves
# `PyPy/3.11.15/x64` -- so an entry named 7.3.23 is complete on disk and invisible
# to every lookup. Upstream gets the number by RUNNING the interpreter, which is
# the only authority for what a given pypy actually implements, and so does this.
#
# THE DECLARATION NAMES PYTHON MINORS ("3.9", "3.10", "3.11") rather than globs,
# and they are matched against `.python_version` with a prefix, exactly as upstream
# does: the newest stable pypy whose python version starts with that line.
install_pypy_toolcache() {
	local rootfs="$1" tc="$2"

	local versions
	read_toolset_versions versions PyPy

	# CHECKSUMS COME FROM A SEPARATE PAGE, because versions.json carries none.
	# Measured: its file entries have arch, download_url, filename and platform and
	# nothing else. pypy.org/checksums.html publishes `<sha256>  <filename>` lines
	# in the format sha256sum reads, so billet verifies what upstream does not.
	local checksums
	checksums=$(curl -fsSL --retry 3 --retry-all-errors https://www.pypy.org/checksums.html)

	local index
	index=$(curl -fsSL --retry 3 --retry-all-errors https://downloads.python.org/pypy/versions.json)

	local line
	for line in "${versions[@]}"; do
		local url
		# `first // empty` RATHER THAN `first`, because jq's `first` on an empty
		# array is null and `.files[]` on null is a fatal "Cannot iterate over
		# null" -- so a line pypy has never released aborted the whole build
		# instead of being recorded, which is the case the record exists for.
		# `empty` ends the pipeline with no output, which is what the caller reads.
		url=$(printf '%s' "$index" | jq -r --arg v "$line" --arg a "$BILLET_TC_PYPY_ARCH" '
			[.[] | select(.stable == true and (.python_version | startswith($v + ".")))]
			| sort_by([(.python_version | split(".") | map(tonumber? // 0)),
			           (.pypy_version | split(".") | map(tonumber? // 0))])
			| last // empty | .files[]
			| select(.arch == $a and .platform == "linux") | .download_url // empty')

		if [ -z "$url" ]; then
			# UNPUBLISHED IS ABOUT THE LINE, NOT ABOUT THE FILE SELECTOR. The query
			# above asks two things at once -- is there a stable release for this
			# python line, and does it carry a linux x64 file -- and a "no" to the
			# SECOND is billet's selector having gone stale against a rename, not
			# the vendor having published nothing. Recording that as unpublished
			# excuses a real gap: both gates then pass an image with no PyPy on a
			# line that has one. So the release is looked up on its own, and only
			# its absence may write the record.
			local release
			release=$(printf '%s' "$index" | jq -r --arg v "$line" '
				[.[] | select(.stable == true and (.python_version | startswith($v + ".")))]
				| last | .python_version // empty')

			if [ -n "$release" ]; then
				echo "pypy publishes a stable python $release for the $line line, but none of" >&2
				echo "  its files is linux/$BILLET_TC_PYPY_ARCH." >&2
				echo "  billet's selector no longer matches what versions.json names," >&2
				echo "  so this is a published line billet cannot find rather than an" >&2
				echo "  unpublished one. Refusing rather than recording it." >&2
				exit 1
			fi

			billet_tc_unpublished PyPy "$line" pypy

			continue
		fi

		local file want
		file="${url##*/}"
		# A HERE-STRING, NOT A PIPE, AND THIS WAS MEASURED ON A REAL BUILDER.
		# `awk '{ print; exit }' ` closes its input as soon as it matches, and the
		# writer upstream gets SIGPIPE -- which `pipefail` turns into a failed
		# pipeline and `set -e` turns into a dead build. The checksums page is
		# ~106KB, well past the 64KB pipe buffer, so whether printf has finished
		# writing when awk exits depends on WHERE in the page the match sits: a
		# container run installed all three lines and the AMI builder aborted on
		# the first, from the same code.
		#
		# The here-string removes the pipeline, so there is no upstream to signal
		# and the early `exit` costs nothing. Reproducing this needed the ERR trap
		# added beside it; the build had printed nothing at all.
		want=$(awk -v f="$file" '$2 == f { print $1; exit }' <<<"$checksums")

		# PUBLISHED WITHOUT A CHECKSUM IS A THIRD THING, and pypy does it on arm64.
		#
		# Measured against the real page: it carries 115 aarch64 lines, and none of
		# them is the release billet resolves for 3.9 (v7.3.16) or 3.10 (v7.3.19),
		# while 3.11 (v7.3.23) is there. So the binary exists and the vendor never
		# published a digest for it -- which is neither "the vendor published
		# nothing" nor "billet's selector is stale".
		#
		# NOT BAKED, BECAUSE THE RULE IS THE RULE: this image runs other people's
		# code and nothing enters it unverified. Recorded rather than fatal, because
		# the alternative is that no arm64 image can be built at all for a gap that
		# is the vendor's and is documented rather than accidental -- and the record
		# is what keeps it from being silent, since both gates require a declared
		# line to be installed or named there exactly.
		#
		# AN EMPTY PAGE IS STILL FATAL, and that is the discriminator. If the page
		# yields no checksums whatsoever then its format has changed or the fetch
		# was truncated, and treating that as "the vendor published no digests"
		# would record every line and ship an image with no PyPy at all, past a gate
		# that would accept it. The same shape as ruby's: only the narrowest
		# question may write the record.
		if [ -z "$want" ]; then
			local any_checksum
			any_checksum=$(awk 'NF == 2 && $1 ~ /^[0-9a-f]{64}$/ { print; exit }' \
				<<<"$checksums")

			if [ -z "$any_checksum" ]; then
				echo "pypy's checksums page yielded no digests at all, so its format has" >&2
				echo "  changed or the fetch was truncated. Refusing rather than recording" >&2
				echo "  every line as unverifiable and shipping an image with no PyPy." >&2
				exit 1
			fi

			billet_tc_unpublished PyPy "$line" \
				"pypy (it ships $file without a checksum)"

			continue
		fi

		fetch_verified "$url" "$BILLET_TC_WORK/pypy.tgz" "$want"

		# STAGED INSIDE THE TARGET, NOT IN THE SCRATCH DIRECTORY. The entry has to
		# be named after a number only the extracted interpreter can report, so the
		# interpreter must RUN -- and billet_tc_run chroots into the target, while
		# BILLET_TC_WORK is on the BUILD HOST. On the guest build those are
		# different filesystems ($WORK versus $WORK/rootfs), so a host-side stage
		# is a path the chroot cannot see and every image build aborts here. On the
		# AMI they are the same machine and the two coincide, which is why a test
		# suite that only ever sets BILLET_TC_ROOT="" cannot see the difference.
		#
		# A DOT-PREFIXED NAME so nothing mistakes it for an entry: both gates
		# iterate `<tool>/*`, which does not match a leading dot, so a build that
		# dies between the extraction and the rename leaves something no lookup and
		# no check will ever see.
		local stage="$tc/PyPy/.staging"
		local guest_stage="$BILLET_TC_IN_TARGET/PyPy/.staging"

		rm -rf "$stage"
		mkdir -p "$stage"
		tar -xzf "$BILLET_TC_WORK/pypy.tgz" -C "$stage" --strip-components=1
		rm -f "$BILLET_TC_WORK/pypy.tgz"

		# `pypy3` FOR EVERY 3.x LINE. Upstream derives the binary name from the
		# major version; billet declares only 3.x, so the name is fixed and the
		# assumption is stated rather than computed from a version it already knows.

		# WHAT THE INTERPRETER FAILED WITH, AND WHAT WAS ACTUALLY THERE.
		#
		# This is the one place in the file that must EXECUTE what it downloaded,
		# so it is the one place where "the archive arrived" and "the runtime
		# works" come apart -- and a loader error names a missing .so without
		# saying whether the file is absent, the disk is full, or the path is
		# wrong. Each of those wants a different fix and they are indistinguishable
		# from the message alone. A guest build fails on a machine that is gone
		# minutes later, so the diagnosis has to be in the output or it does not
		# exist.
		local python_version=""

		# THE CALLER MOUNTS /proc, AND THAT IS WHAT MAKES THIS WORK. `pypy3` is a
		# 14KB launcher whose 59MB library sits beside it, found through a DT_RPATH
		# of `$ORIGIN/`, and glibc resolves $ORIGIN for a main executable by reading
		# /proc/self/exe. A guest build whose chroot had no /proc failed here with
		# "cannot open shared object file" for a file present at full size in that
		# exact directory -- and pypy said so itself on every run, warning that it
		# could not read /proc/cpuinfo.
		#
		# An explicit LD_LIBRARY_PATH stood here while the cause was unknown. It is
		# gone because the cause is fixed at its source: the codeql bundle's own JVM
		# fails identically and had no such workaround, so keeping one for pypy
		# alone covered half the class and left this comment describing a solved
		# mystery.
		# STDOUT ONLY. NOT `2>&1`, WHICH IS WHAT NAMED AN ENTRY AFTER A WARNING.
		# pypy prints "cannot find your CPU L2 cache size in /proc/cpuinfo" to
		# stderr on every run in a chroot with no /proc, and merging that into the
		# capture made $python_version the warning followed by the number -- so the
		# entry became a two-line directory name with the warning inside it, and
		# every later path built from it pointed at nothing. The loader error that
		# exposed this was itself the same mistake one step on.
		#
		# The interpreter's stderr goes to the build log on its own, which is where
		# it was legible in the first place; a value that has to be a version must
		# not be a channel that carries prose.
		if ! python_version=$(billet_tc_run "$guest_stage/bin/pypy3" -c \
			'import sys; print("{}.{}.{}".format(*sys.version_info[0:3]))'); then
			echo "the pypy for python $line did not run from $guest_stage/bin/pypy3" >&2
			echo "what the extraction left in bin/:" >&2
			ls -l "$stage/bin" >&2 || true
			echo "free space on the target:" >&2
			df -h "$tc" >&2 || true
			exit 1
		fi

		# DIGITS OR IT IS NOT A VERSION. This value becomes a DIRECTORY NAME, and
		# the empty-check that stood here accepted the warning-plus-number a merged
		# stderr produced -- naming an entry after a sentence and pointing every
		# later path at nothing.
		#
		# A GLOB, WHICH IS LOOSER THAN THE GATES AND DELIBERATELY SO: `1x.2y.3z`
		# would pass here. Both gates re-check with a regex anchored at both ends,
		# because that is where an unparseable entry has to be refused; this one is
		# upstream of them and its job is to catch prose before it becomes a path,
		# in a script whose every byte competes for a 16KiB delivery budget.
		case "$python_version" in
			[0-9]*.[0-9]*.[0-9]*) ;;
			*)
				echo "the pypy for python $line reported \"$python_version\" as its" >&2
				echo "  python version; an entry named anything but a semver is skipped" >&2
				echo "  by every lookup" >&2
				exit 1
				;;
		esac

		local pypy_version
		pypy_version=$(billet_tc_run "$guest_stage/bin/pypy3" -c \
			'import sys; print("{}.{}.{}".format(*sys.pypy_version_info[0:3]))')

		printf '%s\n' "$pypy_version" > "$stage/PYPY_VERSION"

		local dir="$tc/PyPy/$python_version/$BILLET_TC_ARCH"
		rm -rf "$dir"
		mkdir -p "$(dirname "$dir")"
		mv "$stage" "$dir"

		# THE ENTRY POINTS setup-python EXPECTS. The tarball ships `pypy3` and
		# nothing named `python`, so a workflow's `python` misses an interpreter
		# that is present -- the same shape as the CPython entry points one
		# function over.
		ln -sf pypy3 "$dir/bin/python3"
		ln -sf python3 "$dir/bin/python"

		local guest_dir="$BILLET_TC_IN_TARGET/PyPy/$python_version/$BILLET_TC_ARCH"
		billet_tc_run "$guest_dir/bin/python" -m ensurepip --default-pip >/dev/null

		if [ ! -x "$dir/bin/pip" ]; then
			echo "pypy $python_version baked without a pip entry point" >&2
			exit 1
		fi

		touch "$tc/PyPy/$python_version/$BILLET_TC_ARCH.complete"

		echo "toolcache: pypy $python_version (pypy $pypy_version)"
	done
}

# install_ruby_toolcache bakes in a Ruby for every line github declares.
#
# THE ASSET FOR x64 CARRIES NO ARCH SUFFIX, and copying upstream's pattern would
# resolve nothing. install-ruby.sh builds `ruby-<v>-ubuntu-<pv>-${arch}.tar.gz`
# with arch=x64; measured against ruby/ruby-builder's `toolcache` release, ZERO of
# its 919 assets contain the string x64 -- the unsuffixed name IS the x64 build and
# arm64 is the one that gets a suffix. So the pattern is anchored to end at
# `.tar.gz` and an arm64 asset cannot satisfy it.
#
# THE DIGEST IS GITHUB'S, NOT THE VENDOR'S, and that is weaker than every other
# download here. ruby-builder publishes no SHASUMS file; what exists is the release
# API's `digest` field, which is the HOST attesting to the bytes it stores rather
# than the builder signing what it produced. It detects a corrupted transfer and a
# swapped asset; it does not establish provenance the way pypy's checksums page or
# codeql's checksum.txt do. Stated rather than blurred, because the alternative was
# to skip Ruby entirely and a workflow asking for it would then download one.
install_ruby_toolcache() {
	local rootfs="$1" tc="$2"

	local globs
	read_toolset_versions globs Ruby

	local platform
	platform=$(jq -r '.toolcache[] | select(.name == "Ruby") | .platform_version' \
		"$BILLET_TC_TOOLSET")

	if [ -z "$platform" ] || [ "$platform" = null ]; then
		echo "the toolset declares no platform_version for Ruby, so nothing can say which" >&2
		echo "ubuntu build to fetch" >&2
		exit 1
	fi

	local assets
	assets=$(curl -fsSL --retry 3 --retry-all-errors \
		https://api.github.com/repos/ruby/ruby-builder/releases/tags/toolcache)

	local glob
	for glob in "${globs[@]}"; do
		local prefix="ruby-${glob%\*}"

		# NEWEST BY VERSION, NOT BY LIST ORDER. The API returns assets in upload
		# order, and a re-uploaded older patch would otherwise win.
		local name
		name=$(printf '%s' "$assets" | jq -r --arg p "$prefix" \
			--arg s "-ubuntu-$platform$BILLET_TC_RUBY_SUFFIX.tar.gz" '
			[.assets[].name | select(startswith($p) and endswith($s))]
			| sort_by(ltrimstr("ruby-") | rtrimstr($s) | split(".") | map(tonumber? // 0))
			| last // empty')

		# NOTHING FOR THIS LINE IS NOT A FAILED DOWNLOAD. See
		# billet_tc_unpublished: a line the vendor has never released is recorded
		# and skipped, while anything that goes wrong AFTER an asset is found is
		# still a refusal.
		if [ -z "$name" ]; then
			# THREE CASES, AND ONLY ONE OF THEM IS FATAL. "My query found nothing"
			# is not one fact, and collapsing them either fails a build nobody can
			# fix or excuses a runtime that is genuinely missing.
			#
			#   nothing at all for the line   -> the vendor has released nothing.
			#                                    Record it; this is Ruby 4.0 today.
			#   something for this ubuntu,
			#   but not the x64 name          -> released for another architecture
			#                                    only. There is nothing to bake for
			#                                    an x64 image, so record it too --
			#                                    and Ruby 4.0 arriving arm64-first is
			#                                    exactly how that will look.
			#   something for the line, none
			#   of it for this ubuntu         -> billet's `-ubuntu-<platform>` pin
			#                                    has gone stale against a bump or a
			#                                    rename. THAT is fatal, because the
			#                                    line is published and billet simply
			#                                    cannot find it.
			local for_platform any
			for_platform=$(printf '%s' "$assets" | jq -r --arg p "$prefix" \
				--arg pv "-ubuntu-$platform" '
				[.assets[].name | select(startswith($p) and contains($pv))] | first // empty')
			any=$(printf '%s' "$assets" | jq -r --arg p "$prefix" '
				[.assets[].name | select(startswith($p))] | first // empty')

			if [ -z "$for_platform" ] && [ -n "$any" ]; then
				echo "ruby-builder publishes $any for the $glob line, but nothing carrying" >&2
				echo "  -ubuntu-$platform -- billet's asset pattern no longer matches what the" >&2
				echo "  vendor names, so this line is published and billet cannot find it." >&2
				echo "  Refusing rather than recording it as unpublished." >&2
				exit 1
			fi

			if [ -n "$for_platform" ]; then
				billet_tc_unpublished Ruby "$glob" "ruby-builder for $BILLET_TC_ARCH"
			else
				billet_tc_unpublished Ruby "$glob" ruby-builder
			fi

			continue
		fi

		local want url
		want=$(printf '%s' "$assets" | jq -r --arg n "$name" '
			.assets[] | select(.name == $n) | .digest // empty | ltrimstr("sha256:")')
		url=$(printf '%s' "$assets" | jq -r --arg n "$name" '
			.assets[] | select(.name == $n) | .browser_download_url')

		if [ -z "$want" ]; then
			echo "the release API reports no digest for $name; refusing to bake an" >&2
			echo "unverified runtime" >&2
			exit 1
		fi

		fetch_verified "$url" "$BILLET_TC_WORK/ruby.tgz" "$want"

		local version="${name#ruby-}"
		version="${version%-ubuntu-$platform$BILLET_TC_RUBY_SUFFIX.tar.gz}"

		# INTO THE VERSION DIRECTORY, BECAUSE THE ARCHIVE CARRIES `x64/` ITSELF.
		# Measured with `tar -tzf` on ruby-3.2.9-ubuntu-24.04.tar.gz: its root is a
		# single `x64/` holding bin, include, lib and share. Extracting that into a
		# directory already named x64 produces `<version>/x64/x64/bin/ruby` -- an
		# entry that is structurally complete, passes a marker check, and has no
		# runnable binary where any lookup will go.
		#
		# The comment this replaces claimed the archive had bin/ at its root. It was
		# written from the shape of the problem rather than from the tarball, and
		# the test fixture was built from the same belief -- so the fixture packed
		# bin/ruby at the root, the assertion looked for it there, and code and test
		# agreed with each other and not with the vendor.
		local dir="$tc/Ruby/$version"
		mkdir -p "$dir"

		tar -xzf "$BILLET_TC_WORK/ruby.tgz" -C "$dir"
		rm -f "$BILLET_TC_WORK/ruby.tgz"

		# PROVED, NOT ASSUMED, AND HERE RATHER THAN IN THE GATE. The gate does catch
		# an entry that will not run -- it caught this one -- but it reports it two
		# hundred lines later, against a path, with no idea which installer put it
		# there. A runtime that does not execute where it was just installed is the
		# installer's failure and belongs in the installer's message.
		if ! billet_tc_run \
			"$BILLET_TC_IN_TARGET/Ruby/$version/$BILLET_TC_ARCH/bin/ruby" \
			--version >/dev/null; then
			echo "ruby $version installed and does not run from" >&2
			echo "  $BILLET_TC_IN_TARGET/Ruby/$version/$BILLET_TC_ARCH/bin/ruby -- the" >&2
			echo "  archive layout is" >&2
			echo "  not what this installer expects, or the runtime is missing a shared" >&2
			echo "  library the image does not carry" >&2
			exit 1
		fi

		touch "$tc/Ruby/$version/$BILLET_TC_ARCH.complete"

		echo "toolcache: ruby $version"
	done
}

# install_codeql_toolcache bakes in the CodeQL bundle.
#
# THE DECLARED VERSION IS A BARE `*`, meaning "whatever the action currently
# pins" rather than a line to resolve. So the version comes from codeql-action
# itself: the newest major tag's src/defaults.json names a cliVersion, and that is
# both the bundle tag and the toolcache directory. Resolving it any other way --
# newest bundle release, say -- would bake a bundle the action does not expect.
install_codeql_toolcache() {
	local rootfs="$1" tc="$2"

	# THERE IS NO arm64 BUNDLE, AND THAT IS A VENDOR FACT RATHER THAN A GAP HERE.
	# Measured against the release for the pinned cliVersion: codeql-action ships
	# `codeql-bundle-linux64` and nothing else for linux -- no aarch64, no arm64,
	# in either the .tar.gz or the .tar.zst. So on arm64 this is exactly the case
	# the unpublished record exists for, the same shape as Ruby 4.0 on x64: a line
	# github declares that its vendor has not published, recorded rather than
	# failing a build nobody can fix.
	#
	# THE RECORD IS WHAT KEEPS THIS HONEST. Both gates accept a declared line only
	# when it is installed or named there exactly, so an arm64 image that silently
	# skipped codeql would fail its own gate rather than ship a toolcache missing
	# what it advertises.
	if [ "$BILLET_TC_ARCH" != x64 ]; then
		billet_tc_unpublished CodeQL '*' "codeql-action for $BILLET_TC_ARCH"

		return 0
	fi

	# `sed -n 1p` RATHER THAN `head -1`, for the reason the pypy checksum lookup
	# records: head exits after one line and sort gets SIGPIPE writing the rest,
	# which pipefail reports as a failed pipeline. Thirty tags fit the pipe buffer
	# so it has never fired here -- which is exactly how the pypy one looked until
	# a page grew past 64KB. sed reads its input to the end.
	# `per_page=100` BECAUSE THE DEFAULT IS 30. codeql-action publishes a release
	# per CLI bump plus a moving tag per major, so thirty entries is one page of a
	# fast-moving repository -- and if the newest major's releases ever fill it,
	# the major that wins here is decided by pagination rather than by version.
	# That failure is silent: it bakes a real bundle, just the wrong one.
	local major
	major=$(curl -fsSL --retry 3 --retry-all-errors \
		'https://api.github.com/repos/github/codeql-action/releases?per_page=100' |
		jq -r '.[] | select(.prerelease == false) | .tag_name | select(test("^v[0-9]"))' |
		sed -E 's/^v([0-9]+).*/\1/' | sort -nr | sed -n '1p')

	if [ -z "$major" ]; then
		echo "github/codeql-action publishes no vN tag, so nothing can say which bundle" >&2
		echo "the action currently expects" >&2
		exit 1
	fi

	local version
	version=$(curl -fsSL --retry 3 --retry-all-errors \
		"https://raw.githubusercontent.com/github/codeql-action/v$major/src/defaults.json" |
		jq -r '.cliVersion // empty')

	if [ -z "$version" ]; then
		echo "codeql-action v$major names no cliVersion, so nothing can say which bundle" >&2
		echo "to bake" >&2
		exit 1
	fi

	local base="https://github.com/github/codeql-action/releases/download/codeql-bundle-v$version"

	# A VENDOR-PUBLISHED CHECKSUM, unlike Ruby's. The bundle ships a sibling
	# .checksum.txt, so this verifies against what github published rather than
	# against what the host stores.
	local want
	# THE SAME SHAPE AS PYPY'S, and safe only by accident: a one-line checksum
	# file fits the pipe buffer, so curl finishes writing before awk exits. That is
	# a property of the vendor's file rather than of this code, so it is written
	# the way that does not depend on it.
	local checksum_line
	checksum_line=$(curl -fsSL --retry 3 --retry-all-errors \
		"$base/codeql-bundle-linux64.tar.gz.checksum.txt")
	want=$(awk '{ print $1; exit }' <<<"$checksum_line")

	if [ -z "$want" ]; then
		echo "the codeql bundle $version publishes no checksum; refusing to bake an" >&2
		echo "unverified analysis toolchain" >&2
		exit 1
	fi

	fetch_verified "$base/codeql-bundle-linux64.tar.gz" "$BILLET_TC_WORK/codeql.tgz" "$want"

	local dir="$tc/CodeQL/$version/$BILLET_TC_ARCH"
	mkdir -p "$dir"
	tar -xzf "$BILLET_TC_WORK/codeql.tgz" -C "$dir"
	rm -f "$BILLET_TC_WORK/codeql.tgz"

	# THE ACTION LOOKS FOR THIS MARKER INSIDE THE ENTRY, separately from the
	# completion marker beside it. Its absence makes the action re-download a
	# bundle that is already there.
	# THE SAME PROOF AS RUBY'S. Measured, this bundle's root IS `codeql/`, so
	# extracting into the x64 directory is right here and wrong one installer over
	# -- which is exactly why neither is left to a reading of the archive.
	# STDERR IS NOT SUPPRESSED. `2>&1 >/dev/null` here threw away the only
	# explanation of why the launcher would not start, which cost a build round to
	# learn nothing -- the same shape as the pypy capture that merged stderr into a
	# version, pointed the other way. The rule both times: a runtime's own words go
	# to the build log, and nowhere else.
	if ! billet_tc_run \
		"$BILLET_TC_IN_TARGET/CodeQL/$version/$BILLET_TC_ARCH/codeql/codeql" \
		version >/dev/null; then
		echo "the codeql bundle $version installed and does not run from" >&2
		echo "  $BILLET_TC_IN_TARGET/CodeQL/$version/$BILLET_TC_ARCH/codeql/codeql" >&2
		exit 1
	fi

	touch "$dir/pinned-version"
	touch "$tc/CodeQL/$version/$BILLET_TC_ARCH.complete"

	echo "toolcache: codeql $version"
}

# billet_tc_text reads a vendor's text file as text, whatever it shipped.
#
# POWERSHELL PUBLISHES hashes.sha256 AS UTF-16LE WITH A BOM AND CRLF. Measured with
# `file`: "Unicode text, UTF-16, little-endian, with CRLF line terminators". Bash
# strips the nulls out of a command substitution and warns while doing it, and what
# survives still carries the BOM and a trailing CR on every line -- so a comparison
# against a filename matched nothing and the build refused a checksum the vendor
# had published.
#
# THE LOCAL PROBE SAID IT WAS FINE, and that is the part worth remembering: the
# development shell's grep is ugrep, which reads UTF-16 transparently, so `curl |
# grep` printed the lines exactly as expected. The builder's GNU grep and bash do
# not. Four separate times in this work a probe run on the development machine has
# disagreed with the builder; the file is what to check, not the pipeline that
# happened to be at hand.
billet_tc_text() {
	# COUNTED, NOT MATCHED, BECAUSE BASH CANNOT HOLD A NUL BYTE. `$'\x00'` is the
	# EMPTY string in bash -- a variable cannot contain a null -- so a grep for it
	# matches every line of every file, and the first version of this check sent
	# plain ASCII sums files through iconv, which answered "incomplete character or
	# shift sequence" and refused a file that was already text. Comparing the byte
	# count with and without nulls asks the question without needing to express one.
	# LC_ALL=C SO tr IS BYTE-ORIENTED. In a UTF-8 locale it validates its input as
	# characters and answers "Illegal byte sequence" on a stray BOM or a partial
	# sequence -- which is stderr noise on a value that came out right, and exactly
	# the kind of thing that gets read as a failure later.
	if [ "$(LC_ALL=C tr -d '\000' <"$1" | wc -c)" -ne "$(wc -c <"$1")" ]; then
		iconv -f UTF-16 -t UTF-8 <"$1" | LC_ALL=C tr -d '\r\357\273\277'
	else
		LC_ALL=C tr -d '\r' <"$1"
	fi
}

# billet_tc_sum picks one file's digest out of a sums file.
#
# TWO FORMATS, ONE READER. Kitware writes `<sum>  <file>` and PowerShell writes
# `<sum> *<file>` -- binary mode, whose leading star makes a raw $2 comparison
# match nothing and read as "the vendor published no checksum" for a file it did.
billet_tc_sum() {
	awk -v f="$2" '{ sub(/^\*/, "", $2) } $2 == f { print $1; exit }' <<<"$1"
}

# billet_tc_install unpacks one verified vendor tarball and proves it runs.
#
# THE SHAPE ALL THREE TOOLCHAINS SHARE: resolve a digest from what the vendor
# published, download through the one verified path, extract into the target, and
# execute the result. Written out three times it was also ~1200 bytes of a 16KiB
# user-data budget, which is the other reason it is one function.
#
# PROVING IT RUNS IS NOT OPTIONAL. An archive that unpacks to the wrong depth
# leaves a directory that satisfies every structural check and no PATH lookup --
# the ruby entry that shipped one level too deep is the same mistake, and it took a
# real build to find because nothing executed what it installed.
billet_tc_install() {
	local url="$1" want="$2" dest="$3" strip="$4" prove="$5" algo="${6:-sha256}"

	fetch_verified "$url" "$BILLET_TC_WORK/t.tgz" "$want" "$algo"

	billet_tc_run mkdir -p "$dest"

	if [ "$strip" -gt 0 ]; then
		tar -xzf "$BILLET_TC_WORK/t.tgz" -C "$BILLET_TC_ROOT$dest" \
			--strip-components="$strip"
	else
		tar -xzf "$BILLET_TC_WORK/t.tgz" -C "$BILLET_TC_ROOT$dest"
	fi

	rm -f "$BILLET_TC_WORK/t.tgz"

	[ -z "$prove" ] && return 0

	if ! billet_tc_run $prove >/dev/null; then
		echo "installed into $dest and \"$prove\" does not run" >&2
		exit 1
	fi

	return 0
}

# install_cmake bakes in the cmake the declaration pins.
#
# PINNED, AND THE DECLARATION SAYS WHY: "Pinned to avoid CMake 4.0 compatibility
# issues". apt's is 3.28.3 on noble against the declared 3.31.6, so this is one of
# the few places the distribution's package is not the answer.
#
# `uname -m` SPELLING, A SIXTH ONE. Kitware names its tarballs linux-x86_64 and
# linux-aarch64 where node says x64, go says amd64 and pypy says aarch64 for one
# architecture and x64 for the other.
install_cmake() {
	local version
	version=$(jq -r '.cmake.version // empty' "$BILLET_TC_TOOLSET")
	[ -n "$version" ] || { echo "the toolset declares no cmake version" >&2; exit 1; }

	local base="https://github.com/Kitware/CMake/releases/download/v$version"
	local file="cmake-$version-linux-$BILLET_TC_UNAME.tar.gz"
	local want
	want=$(billet_tc_sum "$(curl -fsSL --retry 3 --retry-all-errors \
		"$base/cmake-$version-SHA-256.txt")" "$file")

	if [ -z "$want" ]; then
		echo "cmake $version publishes no checksum for $file" >&2
		exit 1
	fi

	# INTO /usr/local, ALREADY ON EVERY PATH. The tarball's root is
	# cmake-<version>-linux-<arch>/, so one component is stripped.
	billet_tc_install "$base/$file" "$want" /usr/local 1 "/usr/local/bin/cmake --version"

	echo "toolchain: cmake $version"
}

# install_powershell bakes in the pwsh line the declaration names.
#
# A LINE, NOT A RELEASE. `pwsh: {version: "7.6"}` is 7.6.x, resolved to the newest
# non-prerelease tag on it -- the rule node and go already follow, because pinning a
# patch ships a stale shell for a reproducibility nobody asked for.
install_powershell() {
	local line
	line=$(jq -r '.pwsh.version // empty' "$BILLET_TC_TOOLSET")
	[ -n "$line" ] || { echo "the toolset declares no powershell version" >&2; exit 1; }

	local tag
	tag=$(curl -fsSL --retry 3 --retry-all-errors \
		'https://api.github.com/repos/PowerShell/PowerShell/releases?per_page=100' |
		jq -r --arg l "v$line." '[.[] | select(.prerelease == false)
			| .tag_name | select(startswith($l))] | first // empty')

	if [ -z "$tag" ]; then
		echo "PowerShell publishes no non-prerelease $line release" >&2
		exit 1
	fi

	local version="${tag#v}"
	local base="https://github.com/PowerShell/PowerShell/releases/download/$tag"
	local file="powershell-$version-linux-$BILLET_TC_ARCH.tar.gz"
	# FETCHED TO A FILE, so the bytes can be examined before a shell touches them.
	# A command substitution has already eaten the nulls by the time anything can
	# ask what encoding they were.
	curl -fsSL --retry 3 --retry-all-errors "$base/hashes.sha256" \
		-o "$BILLET_TC_WORK/pwsh-hashes"

	local want
	want=$(billet_tc_sum "$(billet_tc_text "$BILLET_TC_WORK/pwsh-hashes")" "$file")
	rm -f "$BILLET_TC_WORK/pwsh-hashes"

	if [ -z "$want" ]; then
		echo "powershell $version publishes no checksum for $file" >&2
		exit 1
	fi

	local dir=/opt/microsoft/powershell/7
	billet_tc_install "$base/$file" "$want" "$dir" 0 ""
	billet_tc_run chmod 0755 "$dir/pwsh"
	billet_tc_run ln -sfnT "$dir/pwsh" /usr/bin/pwsh

	if ! billet_tc_run /usr/bin/pwsh --version >/dev/null; then
		echo "powershell $version installed and does not run" >&2
		exit 1
	fi

	echo "toolchain: powershell $version"
}

# install_dotnet bakes in an SDK for every channel the declaration names.
#
# SHA-512, WHICH IS WHY fetch_verified TAKES AN ALGORITHM. .NET's release metadata
# carries a 128-hex-character hash per file and publishes no sha256 anywhere;
# sha256sum reads that as a malformed line rather than a mismatch, so the choice
# was a second algorithm or an unverified download.
#
# SIDE BY SIDE IN ONE ROOT. The SDKs share /usr/share/dotnet: `dotnet` dispatches to
# whichever runtime a project asks for, and separate prefixes would give three
# `dotnet` commands that each see one SDK.
install_dotnet() {
	local channels
	channels=$(jq -r '.dotnet.versions[]? // empty' "$BILLET_TC_TOOLSET")

	if [ -z "$(printf '%s' "$channels" | tr -d '[:space:]')" ]; then
		echo "the toolset declares no dotnet channels" >&2
		exit 1
	fi

	local root=/usr/share/dotnet
	local channel

	while IFS= read -r channel; do
		[ -n "$channel" ] || continue

		local meta version want url
		meta=$(curl -fsSL --retry 3 --retry-all-errors \
			"https://builds.dotnet.microsoft.com/dotnet/release-metadata/$channel/releases.json")
		version=$(jq -r '."latest-sdk" // empty' <<<"$meta")

		if [ -z "$version" ]; then
			echo "dotnet channel $channel names no latest-sdk" >&2
			exit 1
		fi

		want=$(jq -r --arg v "$version" --arg f "dotnet-sdk-linux-$BILLET_TC_ARCH.tar.gz" '
			[.releases[] | select(.sdk.version == $v) | .sdk.files[]
			 | select(.name == $f)] | first | .hash // empty' <<<"$meta")
		url=$(jq -r --arg v "$version" --arg f "dotnet-sdk-linux-$BILLET_TC_ARCH.tar.gz" '
			[.releases[] | select(.sdk.version == $v) | .sdk.files[]
			 | select(.name == $f)] | first | .url // empty' <<<"$meta")

		# THE RECORD HAS THREE STATES AND THIS IS THE ONE THAT MUST NOT COLLAPSE.
		#
		# "My selector found nothing" is not "the vendor published nothing". If
		# Microsoft renames the asset or changes its RID shape, recording the channel
		# as unpublished ships an image with no .NET on it past a gate that accepts
		# the record -- the third state in the toolcache rule, which is fatal
		# precisely because the line IS published and billet simply cannot find it.
		# So an absence is believed only after the release's whole file list has been
		# asked whether anything linux-and-this-architecture exists at all.
		#
		# MEASURED: channel 9.0's latest SDK lists 16 files, six of them linux
		# tarballs, and every one carries a hash. Note `dotnet-sdk-linux-musl-x64`
		# also matches this architecture, so a channel that dropped glibc and kept
		# musl is fatal here rather than recorded -- which is the direction to fail
		# in, because it makes a person look at a channel billet cannot install.
		if [ -z "$want" ] || [ -z "$url" ]; then
			local names status
			names=$(jq -r --arg v "$version" \
				'[.releases[] | select(.sdk.version == $v) | .sdk.files[].name] | .[]' \
				<<<"$meta")

			if [ -z "$names" ]; then
				echo "dotnet $version lists no files at all; billet selected a version this metadata does not describe" >&2
				exit 1
			fi

			# A HERE-STRING, NOT A PIPELINE. `printf | grep -q` returns the WRITER's
			# SIGPIPE under pipefail, so a name that IS present reads as absent
			# depending on whether printf finished. grep's status is three-valued and
			# "could not look" is not "not found", so all three are handled.
			status=0
			grep -qE "^dotnet-sdk-linux-.*${BILLET_TC_ARCH}" <<<"$names" || status=$?

			case "$status" in
			0)
				echo "dotnet $version publishes a linux $BILLET_TC_ARCH SDK and billet's selector matched none of it; the asset name or RID shape has changed" >&2
				exit 1
				;;
			1) ;;
			*)
				echo "could not search dotnet $version's file list (grep exited $status)" >&2
				exit 1
				;;
			esac

			billet_tc_unpublished dotnet "$channel" "dotnet for $BILLET_TC_ARCH"

			continue
		fi

		billet_tc_install "$url" "$want" "$root" 0 "" sha512

		echo "toolchain: dotnet $version (channel $channel)"
	done <<<"$channels"

	billet_tc_run ln -sfnT "$root/dotnet" /usr/bin/dotnet

	if ! billet_tc_run /usr/bin/dotnet --list-sdks >/dev/null; then
		echo "dotnet installed and does not run" >&2
		exit 1
	fi

	# WHAT setup-dotnet READS. DOTNET_ROOT is how it finds an SDK it did not
	# install; without it the action downloads one, which is the cost this avoids.
	printf 'DOTNET_ROOT=%s\nDOTNET_CLI_TELEMETRY_OPTOUT=1\nDOTNET_NOLOGO=1\n' \
		"$root" >> "$BILLET_TC_ENV_FILE"
}

# install_android installs the SDK, the platforms, the build-tools and the NDKs
# the declaration names.
#
# THE LICENCE IS ACCEPTED BY THE OPERATOR, NOT BY BILLET. sdkmanager installs
# nothing until Google's terms are accepted, and accepting them is an act with
# legal content -- so this does nothing unless BILLET_TC_ANDROID_ACCEPT_LICENSES is
# `yes`. The EC2 build sets it: that image is built in the operator's own account,
# from their own instruction, and never leaves it. The guest build does NOT, because
# the image it produces is published as a release asset -- running the SDK is use,
# and shipping it to third parties is redistribution, which Google's terms treat
# differently. That distinction is the one policy decision in this file.
#
# NO PUBLISHED CHECKSUM. Google serves the command-line tools zip over HTTPS and
# publishes no digest beside it; everything sdkmanager fetches afterwards it
# verifies against its own signed repository manifest. So the trust boundary is TLS
# to dl.google.com for exactly one file, and sdkmanager's own verification after.
#
# EVERY DECLARATION-DERIVED STRING IS AN ARGUMENT, NEVER SHELL SOURCE. The first
# version built `sh -c "... --install $want"`, and every sdkmanager identifier
# contains a semicolon, which a shell reads as a command separator: a real build ran
# `sdkmanager --install platforms` and then tried to execute `android-34`. The same
# mistake has three other shapes and all four are closed here -- an unquoted scalar
# undergoes PATHNAME EXPANSION as well as word splitting, a path interpolated into
# an `sh -c` program can be ended by a quote in it, and a declared value spliced into
# a `grep` pattern is a regex. The rule is that nothing from the declaration is ever
# concatenated into a program.
install_android() {
	if [ "${BILLET_TC_ANDROID_ACCEPT_LICENSES:-no}" != yes ]; then
		return 0
	fi

	local zip platform_min tools_min ndk_default
	zip=$(jq -r '.android["cmdline-tools"] // empty' "$BILLET_TC_TOOLSET")
	platform_min=$(jq -r '.android.platform_min_version // empty' "$BILLET_TC_TOOLSET")
	tools_min=$(jq -r '.android.build_tools_min_version // empty' "$BILLET_TC_TOOLSET")
	ndk_default=$(jq -r '.android.ndk.default // empty' "$BILLET_TC_TOOLSET")

	if [ -z "$zip" ] || [ -z "$platform_min" ] || [ -z "$tools_min" ]; then
		echo "the declaration's android section is missing cmdline-tools, platform_min_version or build_tools_min_version" >&2
		exit 1
	fi

	# THE FLOORS ARE VALIDATED BEFORE THEY ARE COMPARED AGAINST. A non-numeric
	# platform floor makes every `[ "$api" -ge "$floor" ]` fail -- inside an `if`,
	# where errexit does not act -- so nothing qualifies, and the section would go
	# on to install NDKs and publish ANDROID_HOME over an SDK with no platforms.
	case "$platform_min" in
	'' | *[!0-9]*)
		echo "the declaration's android platform_min_version is '$platform_min', which is not a number" >&2
		exit 1
		;;
	esac

	case "$tools_min" in
	'' | *[!0-9.]*)
		echo "the declaration's android build_tools_min_version is '$tools_min', which is not a version" >&2
		exit 1
		;;
	esac

	# sdkmanager IS A JAVA PROGRAM AND THIS SCRIPT HAS NO JAVA_HOME IN SCOPE. The
	# java installer writes one into the IMAGE's environment file, which jobs read
	# and this script does not, so the value is derived here exactly as that line
	# derives it. Reading it from the environment instead would work on a
	# developer's machine and be empty on the builder.
	local java_default java_home
	java_default=$(jq -r '.java.default // empty' "$BILLET_TC_TOOLSET")

	if [ -z "$java_default" ]; then
		echo "the declaration names no default java, and sdkmanager needs one" >&2
		exit 1
	fi

	java_home="/usr/lib/jvm/temurin-$java_default-jdk-$BILLET_TC_DPKG"

	if ! billet_tc_run test -x "$java_home/bin/java"; then
		echo "sdkmanager needs a jdk and $java_home/bin/java is not there" >&2
		exit 1
	fi

	local root=/usr/local/lib/android/sdk
	billet_tc_run mkdir -p "$root/cmdline-tools"

	curl -fsSL --retry 3 --retry-all-errors \
		"https://dl.google.com/android/repository/$zip" \
		-o "$BILLET_TC_WORK/android-tools.zip"

	# THE ZIP'S ROOT IS `cmdline-tools/`, AND sdkmanager REQUIRES
	# <sdk>/cmdline-tools/<name>/bin. Unpacking straight into the destination lands
	# one level short, and sdkmanager then fails with a message about its own
	# location rather than about the layout. `unzip` is in the declaration's apt set.
	billet_tc_run rm -rf "$root/cmdline-tools/latest" "$root/cmdline-tools/.staging"
	billet_tc_run mkdir -p "$root/cmdline-tools/.staging"
	billet_tc_run unzip -q "$BILLET_TC_WORK/android-tools.zip" \
		-d "$root/cmdline-tools/.staging"

	if ! billet_tc_run test -d "$root/cmdline-tools/.staging/cmdline-tools"; then
		echo "the android tools zip does not contain a cmdline-tools directory" >&2
		exit 1
	fi

	billet_tc_run mv "$root/cmdline-tools/.staging/cmdline-tools" \
		"$root/cmdline-tools/latest"
	billet_tc_run rm -rf "$root/cmdline-tools/.staging"
	rm -f "$BILLET_TC_WORK/android-tools.zip"

	local mgr="$root/cmdline-tools/latest/bin/sdkmanager"

	if ! billet_tc_run test -x "$mgr"; then
		echo "the android command-line tools unpacked and $mgr is not there" >&2
		exit 1
	fi

	# ACCEPTED BEFORE ANYTHING IS ASKED FOR, because sdkmanager otherwise prompts per
	# package and a prompt with no terminal hangs the build rather than failing it.
	#
	# THE PROGRAM IS STATIC AND THE PATHS ARE ARGUMENTS. Interpolating $java_home and
	# $mgr into the program text is the same representation that produced the
	# semicolon bug: a quote in either ends the program early. `sh -c <prog> sh a b`
	# sets $0 to `sh` and passes the rest positionally.
	billet_tc_run sh -c \
		'yes | env "JAVA_HOME=$1" "$2" --licenses >/dev/null 2>&1 || true' \
		sh "$java_home" "$mgr"

	# THE LIST COMES FROM GOOGLE, NOT FROM ARITHMETIC. `platform_min_version: 34`
	# means every platform from 34 upward THAT EXISTS; generating 35, 36, 37 installs
	# some and fails on the first that does not.
	local available
	available=$(billet_tc_run env "JAVA_HOME=$java_home" "$mgr" --list 2>/dev/null |
		awk '{print $1}' | grep -E '^(platforms|build-tools|ndk|cmake|extras);' | sort -u)

	if [ -z "$available" ]; then
		echo "sdkmanager listed no packages; billet cannot tell an empty catalogue from a failed query" >&2
		exit 1
	fi

	# AN ARRAY, NOT A STRING. A space-delimited scalar expanded unquoted undergoes
	# pathname expansion as well as word splitting, so a `*` anywhere in a package
	# name -- and CodeQL's declaration is a bare `*` one section over, so it is not a
	# hypothetical shape -- would be replaced by whatever the working directory
	# happens to contain.
	local -a want=()
	local line api ver platforms=0 buildtools=0

	# PLATFORMS AT OR ABOVE THE FLOOR, NUMERICALLY. A lexical comparison puts
	# android-9 above android-34. Suffixed names (android-VanillaIceCream,
	# android-35-ext14) are discarded by the digit test rather than compared.
	while IFS= read -r line; do
		[ -n "$line" ] || continue

		api=${line#platforms;android-}

		case "$api" in
		'' | *[!0-9]*) continue ;;
		esac

		if [ "$api" -ge "$platform_min" ]; then
			want+=("$line")
			platforms=$((platforms + 1))
		fi
	done <<<"$(printf '%s\n' "$available" | grep '^platforms;android-' || true)"

	# BUILD-TOOLS THE SAME WAY, decided by `sort -V`: these are dotted triples and
	# 9.0.0 sorts above 34.0.0 lexically.
	while IFS= read -r line; do
		[ -n "$line" ] || continue

		ver=${line#build-tools;}

		if [ "$(printf '%s\n%s\n' "$ver" "$tools_min" | sort -V | head -1)" = "$tools_min" ]; then
			want+=("$line")
			buildtools=$((buildtools + 1))
		fi
	done <<<"$(printf '%s\n' "$available" | grep '^build-tools;' || true)"

	# NEITHER CLASS MAY BE EMPTY, and this is the check whose absence would be
	# invisible. A floor above everything published, or a catalogue that changed
	# shape, selects nothing from a category -- and the NDKs and extras would still
	# install, ANDROID_HOME would still be published, and the image would advertise
	# an SDK a job cannot build against.
	if [ "$platforms" -eq 0 ]; then
		echo "no android platform at or above $platform_min exists in sdkmanager's catalogue" >&2
		exit 1
	fi

	if [ "$buildtools" -eq 0 ]; then
		echo "no android build-tools at or above $tools_min exists in sdkmanager's catalogue" >&2
		exit 1
	fi

	# THE NDK MAJORS, MATERIALISED BEFORE THE LOOP. Read through a process
	# substitution, a jq failure does not reach the loop and reads as an empty
	# declaration -- the difference between "upstream declares no NDK" and "billet
	# could not ask" is exactly the distinction the unpublished record exists for.
	local ndk_majors
	ndk_majors=$(jq -r '.android.ndk.versions[]? // empty' "$BILLET_TC_TOOLSET") || {
		echo "could not read the declared android ndk versions" >&2
		exit 1
	}

	local major want_ndk seen_default=0
	while IFS= read -r major; do
		[ -n "$major" ] || continue

		# A MAJOR IS A NUMBER, AND IT REACHES A grep PATTERN. Unvalidated, a `.` or a
		# `+` in it matches a different major entirely.
		case "$major" in
		'' | *[!0-9]*)
			echo "the declaration names android ndk major '$major', which is not a number" >&2
			exit 1
			;;
		esac

		# `|| true` BECAUSE grep EXITS 1 ON NO MATCH AND errexit WOULD TAKE THE
		# SCRIPT OUT BEFORE THE EXPLANATORY CHECK BELOW COULD RUN -- which is this
		# file's oldest trap, and it was still here.
		want_ndk=$(printf '%s\n' "$available" | grep "^ndk;$major\." | sort -V | tail -1 || true)

		if [ -z "$want_ndk" ]; then
			echo "the declaration names ndk $major and sdkmanager publishes no ndk;$major.*" >&2
			exit 1
		fi

		want+=("$want_ndk")

		if [ "$major" = "$ndk_default" ]; then
			seen_default=1
			printf '%s\n' "ANDROID_NDK=$root/ndk/${want_ndk#ndk;}" >>"$BILLET_TC_ENV_FILE"
			printf '%s\n' "ANDROID_NDK_HOME=$root/ndk/${want_ndk#ndk;}" >>"$BILLET_TC_ENV_FILE"
			printf '%s\n' "ANDROID_NDK_ROOT=$root/ndk/${want_ndk#ndk;}" >>"$BILLET_TC_ENV_FILE"
		fi
	done <<<"$ndk_majors"

	# THE DEFAULT MUST BE ONE OF THE DECLARED MAJORS. Otherwise the NDKs install and
	# ANDROID_NDK is never written, so a job reading it finds nothing on an image
	# that carries three of them.
	if [ -n "$ndk_default" ] && [ "$seen_default" -ne 1 ]; then
		echo "the declaration's default android ndk is '$ndk_default', which is not among its declared versions" >&2
		exit 1
	fi

	# THE EXTRAS AND THE CMAKEs, EACH CONFIRMED TO EXIST. extra_list carries the tail
	# of the package name and additional_tools carries the whole of it, which is two
	# shapes in one section. A name the catalogue does not have is fatal here rather
	# than an sdkmanager error twenty minutes into a download.
	local extras pkg
	extras=$(jq -r '(.android.extra_list[]? // empty | "extras;" + .),
		(.android.additional_tools[]? // empty)' "$BILLET_TC_TOOLSET") || {
		echo "could not read the declared android extras and tools" >&2
		exit 1
	}

	while IFS= read -r pkg; do
		[ -n "$pkg" ] || continue

		# A HERE-STRING, NOT A PIPELINE, for the reason the dotnet selector above
		# gives: `printf | grep -q` returns the WRITER's SIGPIPE under pipefail,
		# because grep leaves the instant it matches. So a package that IS in the
		# catalogue reads as absent depending on whether printf finished — which is
		# a guest image build failing over a package the SDK has, intermittently,
		# in a way that does not reproduce. Observed in CI on a green tree.
		#
		# grep's status is three-valued and "could not look" is not "not found", so
		# all three are handled rather than folded into the refusal.
		status=0
		grep -qxF "$pkg" <<<"$available" || status=$?

		case "$status" in
		0) ;;
		1)
			echo "the declaration names the android package $pkg and sdkmanager's catalogue has no such entry" >&2
			exit 1
			;;
		*)
			echo "could not search sdkmanager's catalogue for the android package $pkg (grep exited $status)" >&2
			exit 1
			;;
		esac

		want+=("$pkg")
	done <<<"$extras"

	# ONE INVOCATION: sdkmanager resolves and downloads a shared dependency set once,
	# where a loop re-runs its repository sync per package.
	if ! billet_tc_run env "JAVA_HOME=$java_home" "$mgr" --install "${want[@]}" >/dev/null; then
		echo "installing the android packages failed" >&2
		exit 1
	fi

	printf '%s\n' "ANDROID_HOME=$root" >>"$BILLET_TC_ENV_FILE"
	printf '%s\n' "ANDROID_SDK_ROOT=$root" >>"$BILLET_TC_ENV_FILE"

	echo "toolchain: android sdk with $platforms platform(s) >= $platform_min and $buildtools build-tools >= $tools_min"
}

# install_powershell_modules installs the PowerShell and Azure modules the
# declaration names, through PSGallery.
#
# TWO DECLARATION KEYS, ONE MECHANISM. `powershellModules` and `azureModules` are
# separate lists upstream because they are maintained by different teams; both are
# `Install-Module` from the same gallery, and splitting the installer to match the
# declaration's shape would be two copies of one thing.
#
# THIS IS THE ONE INSTALLER WITH NO PUBLISHED DIGEST TO CHECK, and that is worth
# stating rather than glossing. Every other vendor in this file publishes a
# checksum billet verifies before unpacking; PSGallery publishes none, so what
# stands in for it is the module's Authenticode catalogue signature, which
# PowerShellGet validates on install. That is why -SkipPublisherCheck is NOT
# passed here and must not be added: it is the only integrity check on this path.
# -RequiredVersion pins WHICH version, which is a different guarantee from pinning
# the bytes, and the declaration only pins two of the four.
#
# SLOW, AND THAT IS INHERENT. Microsoft.Graph and Az are meta-modules that pull
# tens of submodules each; this is minutes, not seconds, and it is most of what the
# section costs.
install_powershell_modules() {
	# A DEFAULT RATHER THAN A CONSTANT, so a slow link can raise it without a
	# rebuild. Az is the reason it is minutes and not seconds: it is a meta-module
	# whose dependency set is roughly eighty packages, and the three that install in
	# under a minute say nothing about it.
	# PER-ATTEMPT, NOT FOR THE WHOLE MODULE. Measured in a clean container on this
	# same pwsh: Az 15.6.1 and its 102 modules install in 173 seconds. Ten minutes
	# is a wide margin over that and short enough that three attempts still fit
	# inside a sane build, where one thirty-minute deadline would not.
	local BILLET_TC_PSMOD_TIMEOUT="${BILLET_TC_PSMOD_TIMEOUT:-600}"
	local BILLET_TC_PSMOD_TRIES="${BILLET_TC_PSMOD_TRIES:-3}"

	# BOTH ARE VALIDATED, AND NOT FOR TIDINESS. These reach `timeout` as its
	# duration and `[ -ge ]` as an operand, and each has a value that turns the
	# guard into its opposite:
	#
	#   TIMEOUT=0        GNU timeout documents zero as "the timeout is disabled",
	#                    so the bound this whole change exists for is gone.
	#   TIMEOUT=--help   parsed as an OPTION: timeout prints usage, exits 0, runs
	#                    no PowerShell at all -- and every module is then reported
	#                    installed while nothing was installed. That is the worst
	#                    outcome available here, a green build over an empty image.
	#   TRIES=abc        `[ abc -ge 3 ]` exits 2, which as an `if` CONDITION set -e
	#                    does not act on, so the loop never terminates.
	#
	# A `--` before the duration would stop the option case alone; refusing anything
	# that is not a positive decimal integer stops all three, and says so.
	case "$BILLET_TC_PSMOD_TIMEOUT" in
	'' | *[!0-9]*)
		echo "BILLET_TC_PSMOD_TIMEOUT is '$BILLET_TC_PSMOD_TIMEOUT'; it must be a positive number of seconds" >&2
		exit 1
		;;
	esac

	case "$BILLET_TC_PSMOD_TRIES" in
	'' | *[!0-9]*)
		echo "BILLET_TC_PSMOD_TRIES is '$BILLET_TC_PSMOD_TRIES'; it must be a positive integer" >&2
		exit 1
		;;
	esac

	if [ "$BILLET_TC_PSMOD_TIMEOUT" -lt 1 ] || [ "$BILLET_TC_PSMOD_TRIES" -lt 1 ]; then
		echo "BILLET_TC_PSMOD_TIMEOUT and BILLET_TC_PSMOD_TRIES must both be at least 1" >&2
		exit 1
	fi

	local decl
	decl=$(jq -r '[(.powershellModules[]? // empty), (.azureModules[]? // empty)]
		| .[] | .name + " " + ((.versions // []) | first // "")' "$BILLET_TC_TOOLSET")

	[ -n "$(printf '%s' "$decl" | tr -d '[:space:]')" ] || return 0

	# TRUSTED ONCE, RATHER THAN -Force PER CALL. An untrusted repository prompts,
	# and a prompt on a machine with no terminal is a build that hangs until the
	# node's command timeout rather than one that fails.
	# GUARDED, BECAUSE THE SCRIPT RUNS UNDER errexit. Unguarded, a stall here aborts
	# the build through the ERR trap with a line number and no explanation of which
	# of this file's hundred commands it was.
	if ! billet_tc_run timeout --kill-after=30s -- "$BILLET_TC_PSMOD_TIMEOUT" \
		/usr/bin/pwsh -NoProfile -NonInteractive -Command \
		"Set-PSRepository -Name PSGallery -InstallationPolicy Trusted" >/dev/null; then
		echo "could not mark PSGallery trusted within ${BILLET_TC_PSMOD_TIMEOUT}s; every module install would prompt" >&2
		exit 1
	fi

	local failed=0 name version
	while read -r name version; do
		[ -n "$name" ] || continue

		local pin=""
		if [ -n "$version" ]; then
			pin=" -RequiredVersion '$version'"
		fi

		# EVERY INSTALL IS BOUNDED, AND THE CAUSE IS STILL UNKNOWN.
		#
		# A real build sat at 0.26% CPU for FIFTY MINUTES inside `Install-Module
		# -Name Az` -- the machine doing nothing at all -- while the three modules
		# before it took 60s, 6s and 9s. Unbounded, that consumes the whole
		# `billet ami build` timeout on a paid builder and then reports a timeout
		# against the BUILD rather than naming the module that wedged it. The comment
		# above already said a prompt with no terminal hangs the build instead of
		# failing it; the install it guards was left unbounded anyway.
		#
		# WHAT THE HANG WAS IS NOT ESTABLISHED, and this does not pretend otherwise.
		# Measured in a clean container on the same pwsh 7.6.5: this exact call
		# installs Az 15.6.1 and its 102 modules in 173 SECONDS, exit 0. So the
		# cmdlet is not the problem and swapping it for Install-PSResource -- which
		# was written and reverted -- would have been a fix aimed at a cause the
		# measurement had already ruled out, with different version-pinning semantics
		# thrown in unmeasured.
		#
		# What IS certain is that an unbounded install can own a paid builder for an
		# hour, so the bound stands on its own: the failure is bounded whatever the
		# cause, and the message names which module and how long it was given.
		# `timeout` reports 124 when its TERM ends the command and the shell reports
		# 128+9 when --kill-after's KILL does, so both are read as "did not finish".
		# Neither is proof: a module can exit 124 or be killed for its own reasons,
		# and nothing here can tell those apart. The wording says what was observed
		# rather than asserting a timeout it cannot demonstrate.
		# THE BOUND IS PER ATTEMPT, AND A STALL IS RETRIED RATHER THAN FATAL.
		#
		# The best available explanation for the fifty-minute hang is a stalled
		# socket: PowerShellGet's downloads carry no read timeout, and a builder's
		# egress goes through NAT that drops idle flows, so a connection that stops
		# delivering blocks the cmdlet forever. That is the same failure every curl
		# in this file already passes --retry-all-errors for; the difference is that
		# curl notices and this cannot, so the bound has to do the noticing.
		#
		# It is a HYPOTHESIS, not a measurement -- the instance was gone before it
		# could be confirmed. What makes it safe to act on anyway is that retrying a
		# bounded install is right whether or not that is the cause: a wedged attempt
		# is killed and repeated, a genuinely broken module fails three times and is
		# reported, and neither can own the build.
		#
		# --kill-after IS WHAT MAKES THE BOUND HARD. `timeout` alone sends TERM and
		# then WAITS, so a process that ignores or cannot service TERM runs forever
		# and the bound is decorative -- which is precisely the failure being fixed,
		# reintroduced by the fix. The grace is what guarantees the attempt ends.
		local status=0 attempt=1

		while :; do
			status=0

			# shellcheck disable=SC2086 # $pin is a deliberate fragment, quoted within
			billet_tc_run timeout --kill-after=30s -- "$BILLET_TC_PSMOD_TIMEOUT" \
				/usr/bin/pwsh -NoProfile -NonInteractive -Command \
				"\$ErrorActionPreference='Stop'; Install-Module -Name '$name'$pin \
				 -Scope AllUsers -Force -AcceptLicense" >/dev/null || status=$?

			[ "$status" -ne 0 ] || break

			if [ "$attempt" -ge "$BILLET_TC_PSMOD_TRIES" ]; then
				break
			fi

			# NAMED ON EVERY ATTEMPT, because a silent retry turns a twenty-minute
			# build into an hour-long one with nothing in the log to say why.
			# 124 AND 137 ARE BOTH "IT DID NOT FINISH", and only measuring showed
			# the second. `timeout` reports 124 when its TERM ends the command; a
			# command that IGNORES TERM is ended by --kill-after's KILL instead, and
			# the shell reports that as 128+9. The hang this exists for is exactly
			# the second kind, so reporting it as a generic failure would describe
			# the interesting case in the least useful words available.
			case "$status" in
			124 | 137)
				echo "the powershell module $name did not finish within ${BILLET_TC_PSMOD_TIMEOUT}s (attempt $attempt); retrying" >&2
				;;
			*)
				echo "installing the powershell module $name failed with exit $status (attempt $attempt); retrying" >&2
				;;
			esac

			attempt=$((attempt + 1))
		done

		case "$status" in
		124 | 137)
			echo "the powershell module $name did not finish within ${BILLET_TC_PSMOD_TIMEOUT}s on any of $BILLET_TC_PSMOD_TRIES attempts; the last exited $status, which is what stopping it looks like" >&2
			failed=1

			continue
			;;
		esac

		if [ "$status" -ne 0 ]; then
			echo "installing the powershell module $name failed (exit $status) after $attempt attempt(s)" >&2
			failed=1

			continue
		fi

		# PROVED BY IMPORT, NOT BY PRESENCE. A module directory that exists and does
		# not load is what a half-finished install leaves, and Get-Module
		# -ListAvailable is satisfied by the directory alone.
		if ! billet_tc_run timeout --kill-after=30s -- "$BILLET_TC_PSMOD_TIMEOUT" \
			/usr/bin/pwsh -NoProfile -NonInteractive -Command \
			"\$ErrorActionPreference='Stop'; \
			 if (-not (Get-Module -ListAvailable -Name '$name')) { exit 1 }" >/dev/null; then
			echo "the powershell module $name installed and cannot be found" >&2
			failed=1

			continue
		fi

		echo "toolchain: powershell module $name${version:+ $version}"
	done <<<"$decl"

	[ "$failed" -eq 0 ] || exit 1
}

# install_dotnet_tools installs the .NET global tools the declaration names.
#
# PARSED AND NEVER USED IS THE SAME BUG TWICE. `.dotnet.tools` had a Go type and no
# consumer, exactly as `.clang.default_version` did -- and the shape is worth naming
# because it survives every test: the declaration is read, the field is populated,
# nothing installs anything, and no gate asks. Upstream declares one tool, nbgv,
# which versioning steps invoke by bare name.
#
# --tool-path RATHER THAN --global, because --global writes into $HOME/.dotnet/tools
# -- root's during the build -- and the runner account would find nothing, which is
# the same mistake pipx and npm each made here in their own spelling.
install_dotnet_tools() {
	local tools
	tools=$(jq -r '.dotnet.tools[]?.name // empty' "$BILLET_TC_TOOLSET")

	[ -n "$(printf '%s' "$tools" | tr -d '[:space:]')" ] || return 0

	local failed=0 tool
	while IFS= read -r tool; do
		[ -n "$tool" ] || continue

		# DOTNET_CLI_HOME IS SET for the reason above: the CLI writes state into the
		# invoking user's home and has no business leaving root's behind in an image
		# every job runs as somebody else.
		billet_tc_run env DOTNET_CLI_HOME=/tmp DOTNET_NOLOGO=1 \
			DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 \
			/usr/bin/dotnet tool install --tool-path /usr/local/bin "$tool" >/dev/null
	done <<<"$tools"

	# AND EACH IS PROVED TO RUN, from the command the declaration itself names --
	# `test` rather than the tool's name, because the two need not match and the
	# whole point of reading a declaration is not to guess at it.
	local probe
	while IFS= read -r probe; do
		[ -n "$probe" ] || continue

		# shellcheck disable=SC2086 # the declared probe is a command line
		if ! billet_tc_run env PATH=/usr/local/bin:/usr/bin:/bin $probe >/dev/null 2>&1; then
			echo "the declaration names the dotnet tool probe \"$probe\" and it does not run" >&2
			failed=1
		fi
	done < <(jq -r '.dotnet.tools[]?.test // empty' "$BILLET_TC_TOOLSET")

	[ "$failed" -eq 0 ] || exit 1

	echo "toolchain: the declared dotnet tools are installed"
}

# install_clang_default makes the unsuffixed clang the version the declaration
# names as default.
#
# APT INSTALLS clang-16, clang-17 AND clang-18 AND CREATES NO `clang`. Ubuntu's
# versioned packages are deliberately co-installable, so none of them owns the bare
# name; `default_version` in the declaration exists precisely because upstream has
# to pick one, and billet parsed that field and did nothing with it. A workflow
# invoking bare `clang` -- which is most of them -- found no command at all, while
# every package-parity check passed, because the packages really were installed.
#
# clang++ TOO, and by the same rule. The C++ driver is a separate binary with the
# same problem, and a build that finds `clang` and not `clang++` fails halfway
# through in a way that reads as a broken toolchain rather than a missing link.
install_clang_default() {
	local want
	want=$(jq -r '.clang.default_version // empty' "$BILLET_TC_TOOLSET")

	[ -n "$want" ] || return 0

	local tool
	for tool in clang clang++; do
		if ! billet_tc_run test -x "/usr/bin/$tool-$want"; then
			echo "the declaration names clang $want as the default and the image has no /usr/bin/$tool-$want" >&2
			exit 1
		fi

		billet_tc_run ln -sfnT "/usr/bin/$tool-$want" "/usr/bin/$tool"
	done

	# PROVED, NOT ASSUMED. A dangling symlink is created happily and fails only
	# when a job execs it, which is the whole failure this exists to remove.
	if ! billet_tc_run /usr/bin/clang --version >/dev/null; then
		echo "clang $want was linked as the default and does not run" >&2
		exit 1
	fi

	echo "toolchain: clang $want is the default on PATH"
}

# install_default_runtimes puts a node and a ruby on PATH.
#
# THE TOOLCACHE IS NOT ON PATH, AND NOTHING SAID SO UNTIL SOMETHING NEEDED IT.
# `@actions/tool-cache` entries are found by an ACTION that adds them to PATH for
# one job; a plain `node` or `gem` in a workflow step resolves against the system,
# and this image's apt set carries neither -- only `python-is-python3`. So `npm -g`
# and `gem install` had nothing to run through, and any step calling node without
# setup-node would fail on an image that contains five of them.
#
# node's DEFAULT IS DECLARED and ruby's is not. `node: {default: "22"}` is
# upstream saying which line is the system one, and it had no reader here until
# now. Ruby carries no such field, so billet takes the NEWEST declared line and
# says so rather than inventing a default field upstream does not have -- if
# upstream adds one, this should read it instead.
install_default_runtimes() {
	local tc="$1"

	local node_default
	node_default=$(jq -r '.node.default // empty' "$BILLET_TC_TOOLSET")

	if [ -z "$node_default" ]; then
		echo "the toolset names no default node line, so nothing can say which of the" >&2
		echo "  installed versions a bare \`node\` should be" >&2
		exit 1
	fi

	# THE NEWEST PATCH ON THE DECLARED LINE, resolved from what was installed rather
	# than from the declaration -- the entry names a patch the toolcache chose.
	local dir
	dir=$(find "$tc/node" -maxdepth 1 -name "$node_default.*" -type d 2>/dev/null |
		sort -V | tail -1)

	if [ -z "$dir" ] || [ ! -x "$dir/$BILLET_TC_ARCH/bin/node" ]; then
		echo "the toolset names node $node_default as the default and no toolcache entry" >&2
		echo "  on that line has a runnable node" >&2
		exit 1
	fi

	local guest="$BILLET_TC_IN_TARGET/node/$(basename "$dir")/$BILLET_TC_ARCH/bin"

	for cmd in node npm npx; do
		billet_tc_run ln -sfnT "$guest/$cmd" "/usr/local/bin/$cmd"
	done

	# RUBY IS THE NEWEST INSTALLED LINE. gem installs into the ruby that runs it, so
	# this decides where every global gem lands; picking the oldest would put them
	# somewhere a workflow asking for the newest cannot see.
	local ruby_dir
	ruby_dir=$(find "$tc/Ruby" -maxdepth 1 -type d -name '[0-9]*' 2>/dev/null |
		sort -V | tail -1)

	if [ -n "$ruby_dir" ] && [ -x "$ruby_dir/$BILLET_TC_ARCH/bin/ruby" ]; then
		local ruby_guest="$BILLET_TC_IN_TARGET/Ruby/$(basename "$ruby_dir")/$BILLET_TC_ARCH/bin"

		for cmd in ruby gem bundle bundler; do
			[ -x "$ruby_dir/$BILLET_TC_ARCH/bin/$cmd" ] || continue

			billet_tc_run ln -sfnT "$ruby_guest/$cmd" "/usr/local/bin/$cmd"
		done
	fi

	# PROVED, BECAUSE A SYMLINK INTO A TOOLCACHE IS EASY TO GET WRONG and impossible
	# to notice: the link resolves, the binary is there, and only the interpreter's
	# own view of its prefix says whether `npm -g` will write somewhere useful.
	if ! billet_tc_run /usr/local/bin/node --version >/dev/null; then
		echo "node $node_default is on PATH and does not run" >&2
		exit 1
	fi

	echo "runtime: node $(basename "$dir") is the default on PATH"
}

# install_global_packages installs the sets each package manager declares.
#
# FOUR MANAGERS, FOUR SHAPES, AND EACH NAMES ITS OWN VERIFICATION. pipx entries
# carry the command they provide, node_modules entries carry theirs, and rubygems
# and the powershell modules carry only a name. Where the declaration names a
# command billet runs it; where it does not, billet asks the manager whether the
# package is installed -- which is weaker, and is why the declaration naming one is
# the better case.
#
# NOT PINNED UNLESS THE DECLARATION PINS. `Pester` carries versions and `az` carries
# one; the rest are whatever the manager resolves, the same rule the toolcache
# follows for a line rather than a patch.
#
# THESE RUN THROUGH THE DEFAULT RUNTIMES, which is why install_default_runtimes
# comes first: `npm -g` writes into the node it runs under and `gem install` into
# the ruby, so a global installed through the wrong one lands where nothing looks.
install_global_packages() {
	local failed=0

	# pipx: THE APT PACKAGE, NOT pip install pipx. Ubuntu 24.04 marks its python
	# externally-managed, so `pip install --user` into the system interpreter is
	# refused outright; pipx is packaged for exactly this and is in universe.
	local pipx_pkgs
	pipx_pkgs=$(jq -r '.pipx[]?.package // empty' "$BILLET_TC_TOOLSET")

	if [ -n "$(printf '%s' "$pipx_pkgs" | tr -d '[:space:]')" ]; then
		local pkg
		while IFS= read -r pkg; do
			[ -n "$pkg" ] || continue

			# PIPX_HOME AND PIPX_BIN_DIR ARE SET, because pipx defaults to the
			# INVOKING USER's home -- root's during the build -- and the runner
			# account would find nothing.
			billet_tc_run env PIPX_HOME=/opt/pipx PIPX_BIN_DIR=/usr/local/bin \
				pipx install "$pkg" >/dev/null
		done <<<"$pipx_pkgs"

		local cmd
		while IFS= read -r cmd; do
			[ -n "$cmd" ] || continue

			if ! billet_tc_run "/usr/local/bin/$cmd" --version >/dev/null 2>&1; then
				echo "pipx installed a package providing $cmd and it does not run" >&2
				failed=1
			fi
		done < <(jq -r '.pipx[]?.cmd // empty' "$BILLET_TC_TOOLSET")
	fi

	# node_modules: GLOBAL, THROUGH THE DEFAULT NODE.
	local node_pkgs
	node_pkgs=$(jq -r '.node_modules[]?.name // empty' "$BILLET_TC_TOOLSET" | tr '\n' ' ')

	if [ -n "$(printf '%s' "$node_pkgs" | tr -d '[:space:]')" ]; then
		# --prefix /usr/local, BECAUSE A GLOBAL'S BINARY GOES WHERE npm'S PREFIX
		# POINTS. Without it npm writes into the prefix of the node running it --
		# a toolcache entry under <version>/<arch>/bin, which nothing puts on PATH --
		# so every package installs successfully and every command it provides is
		# missing. The gate caught exactly that: `grunt` reported absent from an
		# image whose npm had just reported installing it. MEASURED against a real
		# npm rather than read: `npm install -g --prefix $T grunt` writes $T/bin/grunt
		# and $T/lib/node_modules/grunt, so /usr/local is the prefix that puts the
		# executable at the /usr/local/bin the runner's PATH already carries.
		#
		# ONE INVOCATION FOR ALL OF THEM. npm resolves a shared dependency tree
		# once, and a loop would reinstall it per package and take minutes longer.
		# shellcheck disable=SC2086 # deliberate word splitting: a package list
		billet_tc_run /usr/local/bin/npm install -g --prefix /usr/local \
			--no-fund --no-audit $node_pkgs >/dev/null

		# AND EACH COMMAND IS PROVED TO EXIST, the same way pipx's are. npm reports
		# success for an install whose binaries land somewhere nothing looks, so
		# without this the only thing that notices is the image gate -- half an hour
		# later, on a machine that has to be built to ask. `command` and not `cmd`:
		# the two global sections spell the field differently in the declaration.
		local mod
		while IFS= read -r mod; do
			[ -n "$mod" ] || continue

			if ! billet_tc_run test -x "/usr/local/bin/$mod"; then
				echo "npm installed a module providing $mod and /usr/local/bin has no such command" >&2
				failed=1
			fi
		done < <(jq -r '.node_modules[]?.command // empty' "$BILLET_TC_TOOLSET")
	fi

	# rubygems: NO VERSION UNLESS DECLARED, and none of them declares one.
	local gems
	gems=$(jq -r '.rubygems[]?.name // empty' "$BILLET_TC_TOOLSET" | tr '\n' ' ')

	if [ -n "$(printf '%s' "$gems" | tr -d '[:space:]')" ] &&
		billet_tc_run test -x /usr/local/bin/gem; then
		# --bindir FOR THE SAME REASON AS npm's --prefix. A gem's executable is
		# written into the ruby's own bin directory, which is a toolcache entry and
		# not on PATH; the gem itself still installs into that ruby's GEM_HOME,
		# which is where it belongs.
		# shellcheck disable=SC2086 # deliberate word splitting: a gem list
		billet_tc_run /usr/local/bin/gem install --no-document \
			--bindir /usr/local/bin $gems >/dev/null
	fi

	[ "$failed" -eq 0 ] || exit 1

	echo "globals: pipx, npm and gem sets installed"
}

# billet_install_toolcache is the one entry point a caller invokes.
#
# It sets its own `set -e`: several failures below rely on the shell aborting,
# and a caller that forgot would turn a refused download into a silently missing
# runtime.
billet_install_toolcache() {
	# `local -` SO THE OPTIONS DO NOT LEAK. A bare `set -e` inside a sourced
	# function changes the CALLER's shell for everything after the call, and a
	# caller that deliberately runs without it would silently start aborting.
	# Measured: with `local -` the options are restored on return.
	local -
	# `-E` SO THE TRAP BELOW FIRES INSIDE FUNCTIONS. Without errtrace an ERR trap
	# is not inherited by shell functions, which is where every one of these
	# installers runs -- so the trap would be set and never fire, the most
	# expensive kind of decoration.
	set -Eeuo pipefail

	# WHAT IT WAS DOING WHEN IT DIED, because nothing else will ever say.
	#
	# This runs on a builder nobody can log into, and its only output is a serial
	# console capped at 64KB. A real build aborted here having printed NOTHING
	# between one installer's last line and cloud-init reporting the module
	# failed: `set -e` exits on a command that wrote no diagnostic of its own, and
	# every candidate for it looked equally likely from the outside. One line
	# turns that into a location.
	trap 'echo "toolcache: FAILED at line $LINENO running: $BASH_COMMAND" >&2' ERR

	local name
	for name in BILLET_TC_DIR BILLET_TC_IN_TARGET BILLET_TC_WORK \
		BILLET_TC_TOOLSET BILLET_TC_ENV_FILE BILLET_TC_ARCH; do
		if [ -z "${!name:-}" ]; then
			echo "$name is not set; billet_install_toolcache cannot run without it" >&2
			return 1
		fi
	done

	# BILLET_TC_ROOT IS ALLOWED TO BE EMPTY and is checked separately, because
	# empty is what "the target is this machine" looks like. Folding it into the
	# loop above would refuse the AMI build for being correct.
	if [ -z "${BILLET_TC_ROOT+set}" ]; then
		echo "BILLET_TC_ROOT is not set; use \"\" when the target is this machine" >&2
		return 1
	fi

	# TRUNCATED, NOT APPENDED TO. The file excuses a declared line from the gate,
	# so a stale entry from a previous build would excuse a line that IS published
	# now and that this build failed to install -- turning the record into a way to
	# pass the gate rather than a way to report a gap.
	# BEFORE THE TRUNCATION, because install_toolcache is what creates the
	# toolcache directory and the record is written into it one line earlier. On a
	# first build there is nothing there yet and the redirection fails, which
	# `set -e` turns into an entry point that refuses every fresh image.
	# REFUSED HERE RATHER THAN DEFAULTED. An unset architecture defaulting to x64
	# would build an x64 toolcache on an arm64 image -- every entry complete, every
	# binary the wrong format, and nothing failing until a job execs one.
	billet_tc_set_arch || return 1

	mkdir -p "$BILLET_TC_DIR"

	: >"$BILLET_TC_DIR/$BILLET_TC_UNPUBLISHED"

	install_toolcache
}
