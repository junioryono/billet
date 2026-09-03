#!/bin/sh
# billet-docker-shim
# billet's docker CLI shim, installed as /opt/billet/bin/docker (first on every
# job step's PATH through GITHUB_PATH, on the host and inside a job container)
# and as /usr/local/bin/docker on the host.
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
# THE REAL CLIENT IS THE FIRST `docker` ON PATH THAT IS NOT A SHIM, told apart
# by the marker on line 2 of this file rather than by path: the shim is
# installed twice on the host and once in a container, and comparing paths let
# one copy exec the other forever. Builtins only (two `read`s), so this works on
# any PATH a job could set; a PATH with no client behind the shim is a loud 127.
set -eu

is_shim() {
	{
		IFS= read -r _first
		IFS= read -r marker
	} <"$1" 2>/dev/null && [ "$marker" = "# billet-docker-shim" ]
}

real=""
oldifs=$IFS
IFS=:
for dir in $PATH; do
	IFS=$oldifs
	[ -n "$dir" ] || continue
	[ -x "$dir/docker" ] || continue
	if is_shim "$dir/docker"; then
		continue
	fi
	real=$dir/docker
	break
done
IFS=$oldifs
if [ -z "$real" ]; then
	echo "billet docker shim: no docker client on PATH behind $0" >&2
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
