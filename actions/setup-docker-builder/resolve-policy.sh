#!/usr/bin/env bash
set -euo pipefail

mount_limit=${BILLET_BUILDKIT_CACHE_MOUNT_LIMIT_BYTES:-21474836480}
if [[ ! "$mount_limit" =~ ^[1-9][0-9]{0,15}$ ]] ||
  [[ ${#mount_limit} -eq 16 && "$mount_limit" > 9007199254740991 ]]; then
  echo "the tier supplied an invalid BuildKit cache-mount ceiling" >&2
  exit 1
fi

printf 'mount_limit_bytes=%s\n' "$mount_limit" >> "$GITHUB_OUTPUT"
