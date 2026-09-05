#!/bin/sh
set -eu

case "$(uname -m)" in
    x86_64) package_arch=amd64 ;;
    aarch64 | arm64) package_arch=arm64 ;;
    *)
        echo "unsupported package test architecture: $(uname -m)" >&2
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

rpm_count=0
rpm_path=
for candidate in "$(pwd)"/dist/billet_*_linux_${package_arch}.rpm; do
    if [ -f "${candidate}" ]; then
        rpm_count=$((rpm_count + 1))
        rpm_path=${candidate}
    fi
done

if [ "${rpm_count}" -ne 1 ]; then
    echo "expected exactly one linux/${package_arch} RPM package in dist, found ${rpm_count}" >&2
    exit 1
fi

# APT FAILS FAST OR NOT AT ALL. On 2026-09-05 Ubuntu's archive was mid-sync and a
# fresh container's apt-get update sat silent for eighteen minutes inside a
# thirty-minute job, then was killed with nothing to read; the run that did finish
# reported "File has unexpected size ... Mirror sync in progress?" after ten. A
# gate that can only fail by being killed is not a gate: every fetch is bounded,
# the transient half is retried, and the whole call is capped so the failure
# arrives with its reason while the job still has time to print it (-v names the
# signal and the command it went to, -k kills a dpkg that shrugs off TERM). And an index
# apt-get update could not fetch is only a WARNING to it, exit 0, measured against
# a black-holed proxy; Error-Mode=any makes that the failure it is, here rather
# than two commands later as "openssl has no installation candidate".
docker run --rm --platform "linux/${package_arch}" --volume "${deb_path}:/tmp/billet.deb:ro" ubuntu:24.04 sh -euxc '
    APT="timeout -v -k 10 300 apt-get -o APT::Update::Error-Mode=any -o Acquire::Retries=3 -o Acquire::http::Timeout=30"
    ${APT} update
    ${APT} install --yes /tmp/billet.deb
    test -x /usr/bin/billet
    test -f /usr/lib/modules-load.d/billet-rbd.conf
    test -f /etc/billet/billet.yaml
    test -d /var/lib/billet
    test -d /srv/jailer
    touch /var/lib/billet/package-lifecycle-state
    touch /srv/jailer/package-lifecycle-guest
    ${APT} remove --yes billet
    test ! -e /usr/bin/billet
    test ! -e /usr/lib/modules-load.d/billet-rbd.conf
    test -f /etc/billet/billet.yaml
    test -f /var/lib/billet/package-lifecycle-state
    test -f /srv/jailer/package-lifecycle-guest
'

docker run --rm --platform "linux/${package_arch}" --volume "${rpm_path}:/tmp/billet.rpm:ro" fedora:42 sh -euxc '
    dnf install --assumeyes /tmp/billet.rpm
    test -x /usr/bin/billet
    test -f /usr/lib/modules-load.d/billet-rbd.conf
    test -f /etc/billet/billet.yaml
    test -d /var/lib/billet
    test -d /srv/jailer
    touch /var/lib/billet/package-lifecycle-state
    touch /srv/jailer/package-lifecycle-guest
    dnf remove --assumeyes billet
    test ! -e /usr/bin/billet
    test ! -e /usr/lib/modules-load.d/billet-rbd.conf
    test -f /etc/billet/billet.yaml
    test -f /var/lib/billet/package-lifecycle-state
    test -f /srv/jailer/package-lifecycle-guest
'
