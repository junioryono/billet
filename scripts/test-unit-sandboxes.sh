#!/bin/sh
# Starts every shipped unit whose sandbox is ProtectSystem=strict under REAL
# systemd, from the real Debian package, with its real sandbox and a stub
# ExecStart, and proves the sandbox permits what the unit's command does (#174).
#
# THE GAP THIS CLOSES. billet-images-refresh.service failed on a real host twice
# past every test in the repository, each of which asserted the TEXT of a
# directive: once because it required a path under its own PrivateTmp, so
# systemd refused to start it at all (226/NAMESPACE, #169), and once because
# ProtectSystem=strict left /run/billet/locks unwritable to it, after it had
# downloaded and imported a five-gigabyte image (#176). Both were found by a
# fleet. This starts the unit.
#
# THE PATHS COME FROM BILLET, NEVER FROM A LIST HERE. The stub is `billet local
# unit-paths <unit> --exercise`, which performs each access the unit's command
# makes, built by the functions the command itself calls, inside the unit's
# sandbox; the same command without --exercise, run outside it, says where each
# write must have landed. Only ExecStart (and Type, for the two notify units,
# and Restart) is replaced, by a drop-in; every other line is the package's.
#
# It needs no GitHub App, because nothing it starts contacts anything, and so CI
# runs it. It needs privileged docker and the package `make dist` builds, which
# is why it is outside `make check`.
#
# BILLET_UNIT_SANDBOX_UNIT_DIR names a directory of unit files installed over
# the package's before anything starts: how the gate is shown to fail, by
# handing it a unit with #169's or #176's fix reverted. BILLET_UNIT_SANDBOX_NAME
# names the container (default billet-unit-sandboxes).
set -eu

if [ "$(uname -s)" != Linux ]; then
    echo "unit sandboxes: skipped, this needs a Linux host with docker" >&2
    exit 0
fi

case "$(uname -m)" in
    x86_64) package_arch=amd64 ;;
    aarch64 | arm64) package_arch=arm64 ;;
    *)
        echo "unsupported unit sandbox architecture: $(uname -m)" >&2
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

name=${BILLET_UNIT_SANDBOX_NAME:-billet-unit-sandboxes-$$}
replacements=${BILLET_UNIT_SANDBOX_UNIT_DIR:-}

# ONLY THE CONTAINER THIS RUN CREATED IS REMOVED, by the id `docker run`
# answered: a name another run already holds fails the run below and is left
# alone.
container=
cleanup() {
    if [ -n "${container}" ]; then
        docker rm -f "${container}" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

container=$(docker run -d --name "${name}" --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -v /etc/ssl/certs/ca-certificates.crt:/usr/local/share/billet-host-ca.crt:ro \
    -e DEBIAN_FRONTEND=noninteractive \
    ubuntu:24.04 sh -c \
    'sed -i -e "s,^URIs: http://archive[.]ubuntu[.]com/ubuntu/$,URIs: https://archive.ubuntu.com/ubuntu/ https://mirrors.edge.kernel.org/ubuntu/," -e "s,^URIs: http://ports[.]ubuntu[.]com/ubuntu-ports/$,URIs: https://ports.ubuntu.com/ubuntu-ports/," /etc/apt/sources.list.d/ubuntu.sources && timeout -v -k 10 300 apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::CAInfo=/usr/local/share/billet-host-ca.crt update -qq && ls /var/lib/apt/lists/*InRelease >/dev/null && timeout -v -k 10 300 apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::CAInfo=/usr/local/share/billet-host-ca.crt install -y -qq systemd systemd-sysv dbus >/dev/null && exec /lib/systemd/systemd' \
    )

# LONGER THAN THE BOOTSTRAP IT WAITS FOR, as in test-systemd-lifecycle.sh: two
# bounded apt calls of up to 300s each come before systemd, and a container that
# has exited ends the wait as a verdict.
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
docker cp "$(pwd)/scripts/unit-sandboxes.sh" "${name}":/tmp/unit-sandboxes.sh
if [ -n "${replacements}" ]; then
    docker cp "${replacements}/." "${name}":/tmp/replacement-units/
fi

docker exec "${name}" sh /tmp/unit-sandboxes.sh
