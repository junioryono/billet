#!/usr/bin/env bash
set -euo pipefail

operation="${1:-}"
lane="${2:-}"

if [[ "$operation" != prepare && "$operation" != write && "$operation" != verify ]] ||
	[[ ! "$lane" =~ ^[a-z0-9-]+$ ]]; then
	echo "usage: cache-conformance-fixtures.sh <prepare|write|verify> <lane>" >&2
	exit 2
fi

fixture_root=conformance/embedded
marker="billet-cache-conformance-$lane"

case "$operation" in
prepare)
	mkdir -p "$fixture_root/node" "$fixture_root/python" "$fixture_root/java" \
		"$fixture_root/dotnet"
	printf '{"name":"%s","version":"1.0.0","lockfileVersion":3,"packages":{}}\n' \
		"$marker" >"$fixture_root/node/package-lock.json"
	printf '# %s\nurllib3==2.5.0\n' "$marker" >"$fixture_root/python/requirements.txt"
	printf '<!-- %s --><project><modelVersion>4.0.0</modelVersion><groupId>sh.billet</groupId><artifactId>conformance</artifactId><version>1</version></project>\n' \
		"$marker" >"$fixture_root/java/pom.xml"
	printf '{"version":1,"dependencies":{},"billet":"%s"}\n' "$marker" \
		>"$fixture_root/dotnet/packages.lock.json"
	printf '%s\n' "$marker" >"$fixture_root/go.sum"
	;;
write | verify)
	python_cmd=python
	if ! command -v "$python_cmd" >/dev/null 2>&1; then
		python_cmd=python3
	fi
	npm_cache=$(npm config get cache)
	pip_cache=$($python_cmd -m pip cache dir)
	go_cache=$(go env GOMODCACHE)
	nuget_cache="${NUGET_PACKAGES:-$HOME/.nuget/packages}"
	paths=(
		"$npm_cache/_cacache/$marker"
		"$HOME/.m2/repository/$marker"
		"$pip_cache/$marker"
		"$go_cache/$marker"
		"$nuget_cache/$marker"
	)
	if [[ "$operation" == write ]]; then
		for path in "${paths[@]}"; do
			mkdir -p "$(dirname "$path")"
			printf '%s\n' "$marker" >"$path"
		done
	else
		for path in "${paths[@]}"; do
			printf '%s\n' "$marker" | cmp - "$path"
		done
	fi
	;;
esac
