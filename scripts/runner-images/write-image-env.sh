#!/bin/bash
# write-image-env.sh SOURCE DEST: write DEST, the environment billet's agent hands
# a job, from the NAME=VALUE lines GitHub's scripts wrote to SOURCE, an image's
# /etc/environment: ImageOS, ImageVersion, the JAVA_HOME and GOROOT lines,
# ANDROID_HOME, the tool cache and GitHub's PATH, as a hosted runner exports them.
#
# THE ONE WRITER OF IT. build-guest-image.sh runs it on the image it builds, and
# the image's completer (/opt/billet/complete) runs it again after installing what
# the image left out, so a completed image's jobs see the variables its components
# added (#584).
#
# THE AGENT PASSES EACH LINE AS IT IS, as the job's whole environment, so each line
# is made a plain NAME=VALUE by pam-environment.awk, the reader the build's own
# steps use, which sits beside this script; and $HOME, which GitHub writes into
# PATH and XDG_CONFIG_HOME for a login shell to expand, is the runner's home, which
# nothing downstream expands.
set -euo pipefail

source=${1:?write-image-env.sh SOURCE DEST}
dest=${2:?write-image-env.sh SOURCE DEST}
here=$(cd "$(dirname "$0")" && pwd)

awk -f "$here/pam-environment.awk" "$source" |
	sed -e 's#\$HOME#/home/runner#g' -e 's#\${HOME}#/home/runner#g' \
		>"$dest.tmp"
if ! grep -q '^ImageOS=' "$dest.tmp"; then
	rm -f "$dest.tmp"
	echo "GitHub's build wrote no ImageOS to $source; refusing an image whose jobs" >&2
	echo "would not see the variables a hosted runner exports" >&2
	exit 1
fi
chmod 0644 "$dest.tmp"
mv "$dest.tmp" "$dest"
