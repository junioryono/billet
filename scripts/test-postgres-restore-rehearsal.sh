#!/bin/sh
# Runs the PostgreSQL restore rehearsal against the real Debian package, in a
# container that has never seen billet.
#
# THE DATABASE IS INSTALLED INSIDE THE SAME CONTAINER rather than run beside it,
# which is a deliberate trade. A second container would be closer to a real
# deployment's topology and would cost this gate a network, a health wait and a
# cleanup path that has to run even when the rehearsal fails — and none of that
# is what the rehearsal is about. What it is about is the archive, the refusals
# and the ownership of what a root-run restore publishes; the database only has
# to be a real PostgreSQL, and the distribution's own package is one.
#
# Same shape as scripts/test-restore-rehearsal.sh, which is where the
# platform/arch selection comes from.
set -eu

case "$(uname -m)" in
    x86_64) package_arch=amd64 ;;
    aarch64 | arm64) package_arch=arm64 ;;
    *)
        echo "unsupported rehearsal architecture: $(uname -m)" >&2
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

docker run --rm --platform "linux/${package_arch}" \
    --volume "${deb_path}:/tmp/billet.deb:ro" \
    --volume "$(pwd)/scripts/postgres-restore-rehearsal.sh:/tmp/postgres-restore-rehearsal.sh:ro" \
    ubuntu:24.04 sh /tmp/postgres-restore-rehearsal.sh
