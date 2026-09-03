#!/bin/sh
# Runs the restore rehearsal against the real Debian package, in a container
# that has never seen billet.
#
# THE PACKAGE RATHER THAN THE BINARY, because what this leg exists to exercise is
# everything the binary alone does not bring: the `billet` service account, the
# directories and their modes, and the config template an operator actually
# edits. Same shape as scripts/test-package-lifecycle.sh, which is where the
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
    --volume "$(pwd)/scripts/restore-rehearsal.sh:/tmp/restore-rehearsal.sh:ro" \
    ubuntu:24.04 sh /tmp/restore-rehearsal.sh
