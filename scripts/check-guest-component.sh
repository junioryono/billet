#!/usr/bin/env bash
#
# check-guest-component.sh present|absent NAME ROOT
# check-guest-component.sh names
# check-guest-component.sh installer NAME
#
# THE COMPONENTS BILLET MAY NOT SHIP (#584), and what each one leaves in an image:
# its paths and the variables it adds to the job's environment. One list, read two
# ways:
#
#   absent   the published image must carry none of it. check-guest-image.sh asks
#            this of every component before an image is published, because the
#            licenses forbid billet redistributing them.
#   present  an image completed with NAME carries all of it, working. The build's
#            trial completion asks this of a copy completed with each component,
#            after GitHub's own tests for it passed.
#   names    prints the components, one a line.
#   installer prints GitHub's template step that installs NAME, which the
#            completer runs; a template that has no such step does not offer NAME.
#
# ROOT is a directory holding the image's tree. `present` executes inside it with
# chroot, so it needs root; `absent` only looks.
#
# Exit status: 0 the check holds, 1 it does not, 2 it could not be asked.
set -euo pipefail

usage() {
	echo "usage: check-guest-component.sh present|absent NAME ROOT, names, or installer NAME" >&2
	exit 2
}

# THE NAMES ARE config.ImageComponents', which a test holds this list to.
names() { printf '%s\n' android-sdk codeql firefox google-chrome microsoft-edge; }

# installer prints the template step that installs NAME, the one the completer
# runs, which a test holds it to.
installer() {
	case "$1" in
	android-sdk) echo install-android-sdk.sh ;;
	codeql) echo install-codeql-bundle.sh ;;
	firefox) echo install-firefox.sh ;;
	google-chrome) echo install-google-chrome.sh ;;
	microsoft-edge) echo install-microsoft-edge.sh ;;
	*) return 2 ;;
	esac
}

# paths prints what NAME installs, one a line: an executable is marked x, a
# directory d.
paths() {
	case "$1" in
	android-sdk)
		echo "d /usr/local/lib/android/sdk"
		echo "x /usr/local/lib/android/sdk/cmdline-tools/latest/bin/sdkmanager"
		;;
	codeql) echo "d /opt/hostedtoolcache/CodeQL" ;;
	firefox)
		echo "x /usr/bin/firefox"
		echo "d /usr/local/share/gecko_driver"
		echo "x /usr/bin/geckodriver"
		;;
	google-chrome)
		echo "x /usr/bin/google-chrome"
		echo "d /opt/google/chrome"
		echo "d /usr/local/share/chromedriver-linux64"
		echo "x /usr/bin/chromedriver"
		echo "d /usr/local/share/chromium"
		echo "x /usr/bin/chromium"
		;;
	microsoft-edge)
		echo "x /usr/bin/microsoft-edge"
		echo "d /usr/local/share/edge_driver"
		echo "x /usr/bin/msedgedriver"
		;;
	*) return 1 ;;
	esac
}

# variables prints the NAME=VALUE lines NAME's installer writes to
# /etc/environment, which /etc/billet-image-env carries to the job.
variables() {
	case "$1" in
	android-sdk)
		echo "ANDROID_HOME=/usr/local/lib/android/sdk"
		echo "ANDROID_SDK_ROOT=/usr/local/lib/android/sdk"
		;;
	codeql) ;;
	firefox) echo "GECKOWEBDRIVER=/usr/local/share/gecko_driver" ;;
	google-chrome)
		echo "CHROME_BIN=/usr/bin/google-chrome"
		echo "CHROMEWEBDRIVER=/usr/local/share/chromedriver-linux64"
		;;
	microsoft-edge) echo "EDGEWEBDRIVER=/usr/local/share/edge_driver" ;;
	*) return 1 ;;
	esac
}

# versioned prints the NAME=PREFIX lines NAME's installer writes whose value goes on
# with a version the build resolves: the Android NDK's, which GitHub's installer
# sets because many builds read them. present asks that each value starts with its
# prefix, compared as text, and is a directory in the image; absent that the name
# is not there at all.
versioned() {
	case "$1" in
	android-sdk)
		echo "ANDROID_NDK=/usr/local/lib/android/sdk/ndk/"
		echo "ANDROID_NDK_HOME=/usr/local/lib/android/sdk/ndk/"
		echo "ANDROID_NDK_ROOT=/usr/local/lib/android/sdk/ndk/"
		echo "ANDROID_NDK_LATEST_HOME=/usr/local/lib/android/sdk/ndk/"
		;;
	codeql | firefox | google-chrome | microsoft-edge) ;;
	*) return 1 ;;
	esac
}

pass() { echo "  ok    $*"; }

FAILED=0
fail() {
	echo "  FAIL  $*" >&2
	FAILED=1
}

# absent fails on every path NAME installs that ROOT has, and every variable of it
# the job's environment names. A link counts, whatever it points at.
absent() {
	local name=$1 root=$2 kind path variable env="$2/etc/billet-image-env" status
	while read -r kind path; do
		if [ -e "$root$path" ] || [ -L "$root$path" ]; then
			fail "the image carries $path, part of $name, which billet may not redistribute (#584)"
		fi
	done < <(paths "$name")
	while IFS= read -r variable; do
		[ -n "$variable" ] || continue
		status=0
		grep -q "^${variable%%=*}=" "$env" || status=$?
		case "$status" in
		0) fail "the job's environment names ${variable%%=*}, which only $name sets" ;;
		1) ;;
		*) fail "could not read $env to see whether it names ${variable%%=*}" ;;
		esac
	done < <(
		variables "$name"
		versioned "$name"
	)
	[ "$FAILED" -ne 0 ] || pass "$name is not in the image"
}

# present fails on every path NAME installs that ROOT lacks or cannot run, and
# every variable of it the job's environment does not name exactly.
present() {
	local name=$1 root=$2 kind path variable value env="$2/etc/billet-image-env"
	while read -r kind path; do
		case "$kind" in
		x) chroot "$root" test -x "$path" || fail "$name: $path is not an executable in the completed image" ;;
		d) chroot "$root" test -d "$path" || fail "$name: $path is not a directory in the completed image" ;;
		esac
	done < <(paths "$name")
	while IFS= read -r variable; do
		[ -n "$variable" ] || continue
		grep -qxF -- "$variable" "$env" || fail "$name: the job's environment does not say $variable"
	done < <(variables "$name")
	while IFS= read -r variable; do
		[ -n "$variable" ] || continue
		value=$(sed -n "s|^${variable%%=*}=||p" "$env") || value=
		case "$value" in
		"${variable#*=}"?*)
			chroot "$root" test -d "$value" ||
				fail "$name: ${variable%%=*} names $value, which is not a directory in the completed image"
			;;
		*) fail "$name: the job's environment does not set ${variable%%=*} to a path under ${variable#*=}" ;;
		esac
	done < <(versioned "$name")

	case "$name" in
	android-sdk) present_android_sdk "$root" ;;
	codeql) present_codeql "$root" ;;
	esac
	[ "$FAILED" -ne 0 ] || pass "$name is completed"
}

# present_android_sdk: a platform to build against, and an SDK Gradle can write the
# NDK a project asks for into, as GitHub's is.
present_android_sdk() {
	local sdk="$1/usr/local/lib/android/sdk"
	compgen -G "$sdk/platforms/android-*" >/dev/null ||
		fail "android-sdk: no platform under /usr/local/lib/android/sdk/platforms"
	[ -n "$(find "$sdk" -maxdepth 0 -perm -o+w)" ] ||
		fail "android-sdk: /usr/local/lib/android/sdk is not writable by everyone, so Gradle cannot install the NDK a project asks for"
}

# present_codeql: one toolcache entry @actions/tool-cache can see, the bundle under
# it running, and the second marker CodeQL's own action looks for, without which it
# downloads the bundle again on every job.
present_codeql() {
	local tc="$1/opt/hostedtoolcache/CodeQL" versiondir version found=0
	for versiondir in "$tc"/*; do
		[ -d "$versiondir" ] || continue
		version=${versiondir##*/}
		found=$((found + 1))
		[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
			fail "codeql: $version is not a full semver, so a workflow asking for a range never matches it"
		[ -f "$versiondir/x64.complete" ] ||
			fail "codeql: $version has no x64.complete beside its arch directory, so tool-cache ignores it"
		[ -f "$versiondir/x64/pinned-version" ] ||
			fail "codeql: $version has no x64/pinned-version, so the action downloads the bundle again on every job"
		chroot "$1" "/opt/hostedtoolcache/CodeQL/$version/x64/codeql/codeql" version >/dev/null 2>&1 ||
			fail "codeql: the bundle under $version does not run"
	done
	[ "$found" -eq 1 ] || fail "codeql: the toolcache holds $found CodeQL entries, want one"
}

case "${1:-}" in
names)
	[ "$#" -eq 1 ] || usage
	names
	exit 0
	;;
installer)
	[ "$#" -eq 2 ] || usage
	installer "$2" || {
		echo "check-guest-component.sh: $2 is not a component billet completes" >&2
		exit 2
	}
	exit 0
	;;
present | absent)
	[ "$#" -eq 3 ] || usage
	mode=$1 name=$2 root=$3
	;;
*) usage ;;
esac

if ! grep -qxF -- "$name" <<<"$(names)"; then
	echo "check-guest-component.sh: $name is not a component billet completes" >&2
	exit 2
fi
if [ ! -d "$root" ]; then
	echo "check-guest-component.sh: $root is not a directory" >&2
	exit 2
fi
if [ "$mode" = present ] && [ "$(id -u)" -ne 0 ]; then
	echo "check-guest-component.sh: present runs the component inside $root; run it as root" >&2
	exit 2
fi

"$mode" "$name" "$root"
exit "$FAILED"
