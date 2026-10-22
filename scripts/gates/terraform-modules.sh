#!/usr/bin/env bash
# terraform-modules.sh: print every top-level Terraform module under
# terraform/modules, one path a line, sorted, or refuse a list a gate could pass
# vacuously over. terraform.sh walks it, and so does
# scripts/terraform_lock_platforms_test.go.
#
# EVERY TOP-LEVEL MODULE, DISCOVERED rather than listed. The gates used to name
# one module, on the reasoning that validate and test load its CHILDREN as called
# modules: true, and it left every SIBLING invisible. Two new modules were added,
# went green through the whole terraform suite, and had never been validated,
# linted or scanned by any of it. A list would have had the same failure one
# module later, so this is a discovery.
#
# EVERY TOP-LEVEL DIRECTORY MUST BE ACCOUNTED FOR, not merely "the list is
# non-empty". A module whose configuration is .tf.json, which terraform accepts
# and a search for *.tf does not, would otherwise be skipped in silence while
# every gate reported success, which is this same failure one turn further on.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

found=$(find terraform/modules -name '*.tf' -not -path '*/.terraform/*' |
	sed -n 's|^\(terraform/modules/[^/]*\)/.*|\1|p' | LC_ALL=C sort -u)
if [ -z "$found" ]; then
	printf 'no terraform modules discovered under terraform/modules; every terraform gate would pass without examining anything\n' >&2
	exit 1
fi

# READ BEFORE IT IS WALKED, so a listing that fails stops here rather than
# reading as no directory missing.
nl=$'\n'
dirs=$(find terraform/modules -mindepth 1 -maxdepth 1 -type d | LC_ALL=C sort)
missing=
while IFS= read -r dir; do
	case "$nl$found$nl" in
	*"$nl$dir$nl"*) ;;
	*) missing="$missing $dir" ;;
	esac
done <<<"$dirs"
if [ -n "$missing" ]; then
	printf 'terraform module directories with no discovered .tf:%s\n' "$missing" >&2
	printf 'every gate would pass without examining them\n' >&2
	exit 1
fi

printf '%s\n' "$found"
