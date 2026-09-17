#!/usr/bin/env bash
. "$here/retirement-role-common.sh"
for scenario in network migration combined stable; do
  r_role_pair "$scenario"
done
