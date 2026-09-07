#!/usr/bin/env bash
# ansible-core at the version the collection is tested against, and the
# collection's one dependency, into directories this action owns.
#
# A VENV, because Ubuntu 24.04's system Python is externally managed (PEP 668)
# and refuses a plain pip install. The venv's bin goes on GITHUB_PATH so every
# later step's ansible-playbook is this one.
#
# ONE PIN, READ FROM THE COLLECTION. The version file lives beside the
# collection's tests and CI's host-lifecycle job installs from the same file,
# so the ansible-core the role is tested on is the ansible-core the action
# runs; actions/actions_test.go fails when the two could disagree.
#
# ansible.posix IS NOT IN THE CHECKOUT. galaxy.yml declares it and the roles
# use it (authorized_key, sysctl), so pointing ANSIBLE_COLLECTIONS_PATH at the
# billet checkout alone resolves the fleet playbook and then fails inside a
# role. It is installed from Galaxy at the version CI pins, with retries,
# because Galaxy answers a 504 now and then and one of those must not fail a
# converge behind an approval; a real outage fails here, before anything
# touches a host.
set -euo pipefail

checkout=$(cd "$GITHUB_ACTION_PATH/../.." && pwd)
tests="$checkout/ansible_collections/junioryono/billet/tests"
core_version=$(tr -d '[:space:]' <"$tests/ansible-core-version")
posix_version=$(tr -d '[:space:]' <"$tests/ansible-posix-version")
[[ -n $core_version && -n $posix_version ]] || {
  echo "::error::the collection's version pins are empty ($tests/ansible-core-version, ansible-posix-version)"
  exit 1
}

venv="$RUNNER_TEMP/billet-ansible"
python3 -m venv "$venv"
"$venv/bin/python" -m pip install --disable-pip-version-check --quiet "ansible-core==${core_version}"
"$venv/bin/ansible" --version | head -n1
echo "$venv/bin" >>"$GITHUB_PATH"

collections="$RUNNER_TEMP/billet-collections"
mkdir -p "$collections"
attempt=1
until "$venv/bin/ansible-galaxy" collection install --collections-path "$collections" "ansible.posix:${posix_version}"; do
  if [[ $attempt -ge 5 ]]; then
    echo "::error::installing ansible.posix ${posix_version} from Galaxy failed ${attempt} times; nothing has touched a host"
    exit 1
  fi
  attempt=$((attempt + 1))
  echo "ansible.posix install failed; retrying (${attempt}/5) in 15s"
  sleep 15
done
