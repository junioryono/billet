#!/usr/bin/env bash
# scenario.sh NAME [ARG...]: run one of the Ansible collection's scenario tests,
# each converging a role in one specific state (the billet-infra-terraform-ansible
# skill lists them). CI's host-lifecycle groups run them through this, and so does
# a person, so a scenario has one way in.
#
# A scenario is either ansible_collections/junioryono/billet/tests/NAME.sh, run
# as it is with ARG, or NAME.yml, run by ansible-playbook with this checkout's
# collections directory as its collection path.
# development-check-mode runs in check mode: it is the first dry run of a Linux
# development host before its tools exist. They need ansible-core and
# ansible.posix at the pins in that directory's ansible-core-version and
# ansible-posix-version, as CI installs them.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

if [ "$#" -lt 1 ]; then
	printf 'usage: %s NAME [ARG...]\n' "${0##*/}" >&2
	exit 2
fi

name=$1
shift
tests=ansible_collections/junioryono/billet/tests

case "$name" in
'' | */* | .*)
	printf 'scenario: %q is not a scenario name\n' "$name" >&2
	exit 2
	;;
esac

if [ -f "$tests/$name.sh" ]; then
	exec "$tests/$name.sh" "$@"
fi

if [ -f "$tests/$name.yml" ]; then
	check=()
	if [ "$name" = development-check-mode ]; then
		check=(--check)
	fi
	ANSIBLE_COLLECTIONS_PATH="$gates_root/ansible_collections" exec ansible-playbook ${check[@]+"${check[@]}"} \
		"$tests/$name.yml" "$@"
fi

printf 'scenario: no %s/%s.sh or %s.yml\n' "$tests" "$name" "$name" >&2
exit 2
