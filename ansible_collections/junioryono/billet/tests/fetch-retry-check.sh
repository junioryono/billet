#!/bin/sh
# Every get_url and uri task in both roles is bounded and retried.
#
# The rule and its reason are in fetch_retry_check.py. This wrapper only finds
# an interpreter that can read YAML: the one Ansible runs with carries PyYAML
# by construction, and a bare python3 on a developer machine may not.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collection=${here%/tests}

python=""
if command -v ansible >/dev/null 2>&1; then
    python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
fi
if [ -z "$python" ] || [ ! -x "$python" ]; then
    python=$(command -v python3) || { echo "fetch-retry-check: no python3" >&2; exit 1; }
fi

# THE GATE IS EXECUTED AGAINST FIXTURES IT MUST REFUSE before it is trusted
# with the real roles: a walk that stopped seeing a task shape would otherwise
# pass in silence, which is the one failure this gate exists to prevent.
"$python" "$here/fetch_retry_check.py" --self-test
exec "$python" "$here/fetch_retry_check.py" "$collection"
