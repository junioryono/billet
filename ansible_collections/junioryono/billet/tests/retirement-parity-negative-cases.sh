#!/usr/bin/env bash
. "$here/retirement-role-common.sh"
r_role_case active control r25-negative-control
# Each negative executes a real extra request through the recording manager.
# Closing is canned healthy; the independent trace comparison must still fail.
for operation in restart stop start; do
  r_role_case "extra-$operation" retained
  "$python" "$here/retirement-role-observe.py" negative \
    "$work/cases/r25-negative-control" "$work/cases/r25-extra-$operation-retained" "$operation"
done
