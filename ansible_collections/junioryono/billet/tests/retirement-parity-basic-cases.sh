#!/usr/bin/env bash
. "$here/retirement-role-common.sh"
for scenario in active inactive failed input; do
  r_role_pair "$scenario"
done
