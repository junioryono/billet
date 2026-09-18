#!/usr/bin/env bash
# Shared definitions; only the four selected case files execute namespaces.
r_role_case() { # scenario retained|control [case-name]
  local scenario=$1 variant=$2 name=r25-$1-$2
  if [ "$scenario" = resume ]; then name=r42-resume-$variant; fi
  name=${3:-$name}
  r_plant "$name"
  printf '%s %s\n' "$scenario" "$variant" >"$work/cases/$name/role-case"
  printf '%s\n' "$here/retirement-role-driver.sh" >"$work/cases/$name/role-driver"
  cp "$here/retirement-role.yml" "$work/play-retirement-role.yml"
  e "$name" PYTHONDONTWRITEBYTECODE=1
  e "$name" "BILLET_GATE_ROLE=$work/cases/$name"
  e "$name" "BILLET_GATE_ROLE_HELPER=$here/retirement-role-host.py"
  e "$name" "BILLET_GATE_SERVICES=$work/cases/$name/services"
  # Extra ordinary destinations are isolated only in these whole-role cases.
  p "$name" 'for target in /usr/local /usr/sbin /srv; do
  tag=${target//\//-}
  mkdir -p "$MNT/$tag-upper" "$MNT/$tag-work"
  mount -t overlay overlay -o "lowerdir=$target,upperdir=$MNT/$tag-upper,workdir=$MNT/$tag-work" "$target"
done
rm -rf /etc/systemd/system/billet-* /etc/systemd/system/var-lib-billet-server.mount
mkdir -p /usr/local/bin /etc/systemd/network /etc/sysctl.d /etc/modules-load.d
# The version is part of the planted names: all-host validation requires a
# vMAJOR.MINOR.PATCH release, so a made-up word here fails before any task runs.
for binary in firecracker jailer; do
  printf "#!/bin/sh\nexit 0\n" >"/usr/local/bin/$binary-v1.16.1"
  chmod 0755 "/usr/local/bin/$binary-v1.16.1"
  ln -sf "$binary-v1.16.1" "/usr/local/bin/$binary"
done
printf "#!/bin/sh\nexit 0\n" >/usr/sbin/nft
chmod 0755 /usr/sbin/nft'
  case "$scenario" in
    extra-*)
      mkdir -p "$work/cases/$name/collection/ansible_collections/junioryono"
      cp -R "$here/.." "$work/cases/$name/collection/ansible_collections/junioryono/billet"
      "$python" "$here/retirement-role-host.py" mutant \
        "$work/cases/$name/collection/ansible_collections/junioryono/billet/roles/host/tasks/services.yml" "${scenario#extra-}"
      e "$name" "ANSIBLE_COLLECTIONS_PATH=$work/cases/$name/collection:$collections_path" ;;
  esac
  ns_case "$name" escalated play-retirement-role
  # The shared failure printer tails only 60 lines. Preserve the complete
  # observer diagnostic, including the seed and measured request traces.
  if [ "$status" -ne 0 ] && [ -f "$work/cases/$name/role-failure.txt" ]; then
    cat "$work/cases/$name/role-failure.txt" >&2
    fail "$name: whole-role observer failed; see unit expectations and request trace above"
  fi
  expect_allowed "$name"
}
r_role_pair() {
  r_role_case "$1" control
  r_role_case "$1" retained
  local prefix=r25-$1
  if [ "$1" = resume ]; then prefix=r42-resume; fi
  "$python" "$here/retirement-role-observe.py" compare "$work/cases/$prefix-control" "$work/cases/$prefix-retained"
}
