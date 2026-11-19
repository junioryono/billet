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

# THE INTERPRETER IS THE FIRST ONE THE PIN ACTUALLY RUNS UNDER, tried rather than
# guessed. ansible-core supports a range of Pythons, and a machine's default
# python3 can move past it: on 2026-11-07 a Mac's Homebrew python3 became 3.15, the
# pinned ansible-core failed inside its own import ("module 'dataclasses' has no
# attribute '_is_type'"), and the laptop converge, the break-glass path, failed
# before touching a host (#792). So the default is tried first, then the named
# versions newest first, each in a fresh venv, and the first under which the pinned
# ansible-core installs and answers --version is kept. None is a refusal that
# names what was tried.
venv="$RUNNER_TEMP/billet-ansible"
chosen=""
tried=()
seen=" "
for py in python3 python3.14 python3.13 python3.12; do
  command -v "$py" >/dev/null 2>&1 || continue
  if ! version=$("$py" -c 'import sys; print("%d.%d" % sys.version_info[:2])' 2>/dev/null); then
    tried+=("$py (would not report its version)")
    continue
  fi
  # ONE ATTEMPT PER VERSION: python3 is often the same interpreter as one named.
  [[ $seen == *" $version "* ]] && continue
  seen="$seen$version "
  rm -rf "$venv"
  if "$py" -m venv "$venv" &&
    "$venv/bin/python" -m pip install --disable-pip-version-check --quiet "ansible-core==${core_version}" &&
    banner=$("$venv/bin/ansible" --version 2>&1); then
    chosen="$py ($version)"
    break
  fi
  tried+=("$py ($version)")
done
if [[ -z $chosen ]]; then
  echo "::error::ansible-core ${core_version} could not be installed and run under any Python here; tried: ${tried[*]:-no python3 at all}. Install one it supports (python3.12 to python3.14 are tried by name) and converge again; nothing has touched a host"
  exit 1
fi
echo "ansible-core ${core_version} under ${chosen}: ${banner%%$'\n'*}"
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
