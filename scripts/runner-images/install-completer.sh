#!/usr/bin/env bash
# install-completer.sh ROOT TOOLSET
#
# Puts the completer at ROOT/opt/billet/complete: what installs a component the
# published guest leaves out (#584), chrooted into an image or run on the machine
# itself (ROOT /, the AMI builder's case), with GitHub's own installers, helpers and
# tests for the components, unedited, from the pinned template, except that the
# tests are read from beside the helpers: a build's own /imagegeneration/tests had
# them skipped. TOOLSET is the declaration of the image's architecture, which
# GitHub's installers read as installers/toolset.json.
#
# One installer of it for the guest build and the AMI build, so the two complete
# a component the same way.
set -euo pipefail

root=${1:?install-completer.sh needs the image root}
toolset=${2:?install-completer.sh needs the toolset declaration}
here=$(cd "$(dirname "$0")" && pwd)
upstream="$here/upstream/images/ubuntu/scripts"
completer="${root%/}/opt/billet/complete"

[ -r "$toolset" ] || {
	echo "install-completer: cannot read the toolset declaration $toolset" >&2
	exit 1
}

rm -rf "$completer"
install -d -m 0755 "$completer" "$completer/installers" "$completer/scripts" \
	"$completer/scripts/build" "$completer/scripts/tests"
install -m 0755 "$here/completer/run" "$completer/run"
install -m 0755 "$here/write-image-env.sh" "$completer/write-image-env.sh"
install -m 0644 "$here/pam-environment.awk" "$completer/pam-environment.awk"
install -m 0644 "$toolset" "$completer/installers/toolset.json"
for script in install-android-sdk.sh install-codeql-bundle.sh install-firefox.sh \
	install-google-chrome.sh install-microsoft-edge.sh; do
	install -m 0755 "$upstream/build/$script" "$completer/scripts/build/$script"
done
cp -R "$upstream/helpers" "$completer/scripts/helpers"
# EXECUTABLE AS configure-environment.sh MAKES IT before linking invoke_tests
# to it; the template carries it as an ordinary file.
chmod 0755 "$completer/scripts/helpers/invoke-tests.sh"
for file in Android.Tests.ps1 Browsers.Tests.ps1 Toolset.Tests.ps1; do
	install -m 0644 "$upstream/tests/$file" "$completer/scripts/tests/$file"
done

# THE ONE EDIT, AND ONLY IF IT IS STILL THE ONE LINE: GitHub's Invoke-PesterTests
# reads every test file from /imagegeneration/tests, which the build edited.
if ! old='    $testPath = "/imagegeneration/tests/${TestFile}.Tests.ps1"' \
	new='    $testPath = Join-Path $PSScriptRoot "${TestFile}.Tests.ps1"' \
	awk '
		$0 == ENVIRON["old"] { print ENVIRON["new"]; replaced++; next }
		{ print }
		END { exit replaced == 1 ? 0 : 1 }' \
	"$upstream/tests/Helpers.psm1" >"$completer/scripts/tests/Helpers.psm1"; then
	echo "the template's Helpers.psm1 no longer reads its tests from one /imagegeneration" >&2
	echo "  path, so the completer cannot point it at its own" >&2
	exit 1
fi
chmod 0644 "$completer/scripts/tests/Helpers.psm1"
