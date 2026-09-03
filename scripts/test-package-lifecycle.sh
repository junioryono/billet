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

docker run --rm --platform "linux/${package_arch}" --volume "${deb_path}:/tmp/billet.deb:ro" ubuntu:24.04 sh -euxc '
    apt-get update
    apt-get install --yes /tmp/billet.deb
    test -x /usr/bin/billet
    test -f /usr/lib/modules-load.d/billet-rbd.conf
    test -f /etc/billet/billet.yaml
    test -d /var/lib/billet
    test -d /srv/jailer
    touch /var/lib/billet/package-lifecycle-state
    touch /srv/jailer/package-lifecycle-guest
    apt-get remove --yes billet
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
