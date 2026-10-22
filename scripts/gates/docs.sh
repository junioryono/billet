#!/usr/bin/env bash
# docs.sh: build the Sphinx site with warnings as errors, as Read the Docs and CI
# do, into a throwaway directory, so a broken cross-reference or a page missing
# from the toctree fails here rather than publishing.
#
# SPHINXBUILD names the sphinx-build to use. Unset, the one on PATH is used, and
# without one a virtual environment is made at docs/_venv from
# docs/requirements.txt, once, the way a fresh checkout gets a working build.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

sphinx=${SPHINXBUILD-}
if [ -z "$sphinx" ]; then
	if command -v sphinx-build >/dev/null 2>&1; then
		sphinx=sphinx-build
	else
		if ! [ -x docs/_venv/bin/sphinx-build ]; then
			python3 -m venv docs/_venv
			docs/_venv/bin/pip install -r docs/requirements.txt
		fi
		sphinx=docs/_venv/bin/sphinx-build
	fi
fi

build=$(mktemp -d "${TMPDIR:-/tmp}/billet-docs.XXXXXX")
trap 'rm -rf "$build"' EXIT

"$sphinx" -M html docs "$build" -W --keep-going
