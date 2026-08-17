#!/bin/bash
set -euo pipefail

tag=${RELEASE_TAG:?RELEASE_TAG must name the release tag}
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if ! [[ $tag =~ $semver ]]; then
	printf 'release tag is not semantic versioning: %s\n' "$tag" >&2
	exit 1
fi

expected_collection_version=${tag#v}
actual_collection_version="$(sed -n 's/^version: //p' ansible_collections/junioryono/billet/galaxy.yml)"
if [[ $actual_collection_version != "$expected_collection_version" ]]; then
	printf 'Ansible collection version is %s, expected %s for %s\n' "$actual_collection_version" "$expected_collection_version" "$tag" >&2
	exit 1
fi

internal_ref_count=0
refs_are_exact=true
internal_refs=
while IFS= read -r ref; do
	[[ -n $ref ]] || continue
	internal_ref_count=$((internal_ref_count + 1))
	internal_refs="${internal_refs}${internal_refs:+
}${ref}"
	[[ $ref == *"@$tag" ]] || refs_are_exact=false
done < <(grep -RhE --include='action.yml' '^[[:space:]]+uses: junioryono/billet/actions/[^[:space:]@]+@' actions || true)
if [[ $internal_ref_count -ne 3 || $refs_are_exact != true ]]; then
	printf 'release action references did not all resolve exactly to %s:\n%s\n' "$tag" "$internal_refs" >&2
	exit 1
fi
