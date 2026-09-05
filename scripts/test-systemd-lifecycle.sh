#!/bin/sh
# Drives `billet local up`/`status`/`down` against REAL systemd and the real
# Debian package, in a throwaway container.
#
# THE GAP THIS CLOSES. Those three commands are the only ones that drive a
# service manager, and every test of them supplies a fake one — so what systemd
# does with the units billet ships had never been exercised. Nor had the thing
# `billet local down` exists for: refusing to stop a service while a host is
# running compute the ledger cannot see. This runs both, and asserts on
# UNIT STATE rather than on an exit code, because an error value is the cheapest
# thing a command produces.
#
# Same platform/arch selection and same package-not-binary reasoning as
# scripts/test-package-lifecycle.sh and scripts/test-restore-rehearsal.sh, and
# for one more reason here: `billet local up` deliberately REFUSES to write unit
# files — a unit it wrote could shadow a later package install — so the units
# have to arrive the way they really do.
#
# It needs a config with a working GitHub App, because a control plane will not
# start without one and `local down` skips the compute stage when the server is
# not running. BILLET_SYSTEMD_LIFECYCLE_CONFIG names that config and
# BILLET_SYSTEMD_LIFECYCLE_KEY the App key it points at; without them this skips
# rather than failing, since there is nothing to substitute for a real credential.
set -eu

if [ "$(uname -s)" != Linux ]; then
    echo "systemd lifecycle: skipped, this needs a Linux host with docker" >&2
    exit 0
fi

: "${BILLET_SYSTEMD_LIFECYCLE_CONFIG:=}"
: "${BILLET_SYSTEMD_LIFECYCLE_KEY:=}"

if [ -z "${BILLET_SYSTEMD_LIFECYCLE_CONFIG}" ] || [ -z "${BILLET_SYSTEMD_LIFECYCLE_KEY}" ]; then
    echo "systemd lifecycle: skipped." >&2
    echo "  Set BILLET_SYSTEMD_LIFECYCLE_CONFIG and BILLET_SYSTEMD_LIFECYCLE_KEY to a" >&2
    echo "  billet.yaml with a working GitHub App and the key it names. A control" >&2
    echo "  plane cannot start without one, and there is nothing honest to put in" >&2
    echo "  its place: a fake would exercise a code path no deployment takes." >&2
    exit 0
fi

case "$(uname -m)" in
    x86_64) package_arch=amd64 ;;
    aarch64 | arm64) package_arch=arm64 ;;
    *)
        echo "unsupported systemd lifecycle architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

deb_count=0
deb_path=
for candidate in "$(pwd)"/dist/billet_*_linux_${package_arch}.deb; do
    if [ -f "${candidate}" ]; then
        deb_count=$((deb_count + 1))
        deb_path=${candidate}
    fi
done

if [ "${deb_count}" -ne 1 ]; then
    echo "expected exactly one linux/${package_arch} Debian package in dist, found ${deb_count}" >&2
    exit 1
fi

name=billet-systemd-lifecycle
# ITS OWN SNAPSHOT STORAGE, on a real filesystem. The container runs its own
# docker daemon — which is better isolation than sharing the host's, since no
# container this test creates is ever visible to the host's billet — and
# overlayfs on overlayfs is refused. BOTH directories are needed: docker 29
# delegates snapshots to containerd, so mounting only /var/lib/docker fails
# identically while naming a path that looks unrelated.
storage=$(mktemp -d)

cleanup() {
    docker rm -f "${name}" >/dev/null 2>&1 || true
    rm -rf "${storage}" || true
}
trap cleanup EXIT INT TERM

mkdir -p "${storage}/docker" "${storage}/containerd"

docker run -d --name "${name}" --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -v "${storage}/docker:/var/lib/docker" \
    -v "${storage}/containerd:/var/lib/containerd" \
    -e DEBIAN_FRONTEND=noninteractive \
    ubuntu:24.04 sh -c \
    'timeout -v -k 10 300 apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 -o Acquire::http::Timeout=30 update -qq && timeout -v -k 10 300 apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 -o Acquire::http::Timeout=30 install -y -qq systemd systemd-sysv dbus docker.io >/dev/null && exec /lib/systemd/systemd' \
    >/dev/null

# LONGER THAN THE BOOTSTRAP IT WAITS FOR: the entrypoint may spend up to 300s in
# each of two bounded apt calls before systemd starts, so 240 x 3s = 720s, and a
# container that has exited ends the wait as a verdict rather than being polled.
printf 'waiting for systemd in the container'
state=
i=0
while [ "${i}" -lt 240 ]; do
    state=$(docker exec "${name}" systemctl is-system-running 2>/dev/null || true)
    case "${state}" in running | degraded) break ;; esac
    if [ "$(docker inspect -f '{{.State.Running}}' "${name}" 2>/dev/null)" = "false" ]; then
        state=exited
        break
    fi
    printf '.'
    sleep 3
    i=$((i + 1))
done
echo " -> ${state:-unknown}"

case "${state}" in
    running | degraded) ;;
    *)
        echo "systemd never came up in the container" >&2
        docker logs "${name}" 2>&1 | tail -20 >&2
        exit 1
        ;;
esac

docker cp "${deb_path}" "${name}":/tmp/billet.deb
docker cp "${BILLET_SYSTEMD_LIFECYCLE_KEY}" "${name}":/tmp/app-key.pem
docker cp "${BILLET_SYSTEMD_LIFECYCLE_CONFIG}" "${name}":/tmp/billet.yaml
docker cp "$(pwd)/scripts/systemd-lifecycle.sh" "${name}":/tmp/systemd-lifecycle.sh
docker exec "${name}" chmod +x /tmp/systemd-lifecycle.sh

docker exec "${name}" /tmp/systemd-lifecycle.sh
result=$?

echo
echo "=== the container's journal for billet's units ==="
docker exec "${name}" journalctl -u billet-server -u billet-node \
    -n 30 --no-pager -o cat 2>&1 | tail -30 || true

exit "${result}"
