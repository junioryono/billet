#!/bin/bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: verify-repository-key.sh KEY EXPECTED_PRIMARY_FINGERPRINT" >&2
	exit 2
fi
if [[ -z $1 || ! $2 =~ ^[A-F0-9]{40}$ ]]; then
	echo "usage: verify-repository-key.sh KEY EXPECTED_PRIMARY_FINGERPRINT" >&2
	exit 2
fi

key=$1
expected=$2
observed="$(gpg --batch --show-keys --with-colons "$key" | awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; want = 0 }')"
if [[ $observed != "$expected" ]]; then
	echo "repository key bundle must contain exactly primary fingerprint $expected; found: ${observed:-none}" >&2
	exit 1
fi
