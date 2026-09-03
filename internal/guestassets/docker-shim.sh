#!/bin/sh
# billet's docker CLI shim, installed as /usr/local/bin/docker so it sits ahead of
# the real client on a job's PATH.
#
# WHY A SHIM. BuildKit's GitHub Actions cache client (`--cache-to type=gha`) is
# pointed wherever the buildx CLI's ACTIONS_RESULTS_URL says, and the runner sets
# that variable for every step from the job message: nothing billet controls can
# change it there, and GITHUB_ENV loses to it. A builder created with buildx's
# docker-container driver runs a stock image that does not trust the node's
# certificate, so left alone it meets the remapped results origin and dies with
# `x509: certificate signed by unknown authority`. The guest already runs a
# plaintext adapter for exactly that client, reachable from a container on the
# docker gateway. This shim points a build at it, so a workflow that changed only
# `runs-on` gets billet's cache from a container builder without naming anything.
#
# ONLY BUILD INVOCATIONS ARE REWRITTEN, and only for the client process. The
# results origin also carries artifacts and live logs, which the adapter
# deliberately does not serve, and `docker run -e ACTIONS_RESULTS_URL` forwards
# whatever this process holds into a container. Everything that is not a build
# is exec'd untouched.
#
# THE REAL CLIENT IS THE NEXT `docker` ON PATH AFTER THIS FILE, resolved through
# symlinks, rather than a hard-coded /usr/bin/docker: that is what lets a test
# stand a fake in front of it, and it is why a PATH without one is a loud 127
# rather than a shim exec'ing itself forever.
set -eu

# Builtins only, so this works on any PATH a job could set.
case $0 in
*/*) self_dir=${0%/*} ;;
*) self_dir=. ;;
esac
self=$(cd "$self_dir" && pwd -P)/${0##*/}
real=""
oldifs=$IFS
IFS=:
for dir in $PATH; do
	IFS=$oldifs
	[ -n "$dir" ] || continue
	[ -x "$dir/docker" ] || continue
	resolved=$(cd "$dir" 2>/dev/null && pwd -P)/docker
	[ "$resolved" != "$self" ] || continue
	real=$dir/docker
	break
done
IFS=$oldifs
if [ -z "$real" ]; then
	echo "billet docker shim: no docker client on PATH behind $self" >&2
	exit 127
fi

if [ -n "${BILLET_ACTIONS_CACHE_URL:-}" ]; then
	rewrite=""
	case "${1:-}" in
	build | buildx | bake) rewrite=1 ;;
	compose)
		for arg in "$@"; do
			case "$arg" in
			build | --build) rewrite=1 ;;
			esac
		done
		;;
	esac
	if [ -n "$rewrite" ]; then
		# version=2 is what makes buildx read url_v2 at all; the adapter speaks
		# only the v2 service, so the v1 URL is left alone rather than pointed at
		# a listener that would refuse it.
		ACTIONS_RESULTS_URL=$BILLET_ACTIONS_CACHE_URL
		ACTIONS_CACHE_SERVICE_V2=true
		export ACTIONS_RESULTS_URL ACTIONS_CACHE_SERVICE_V2
	fi
fi

exec "$real" "$@"
