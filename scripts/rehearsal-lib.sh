#!/bin/bash
# Shared machinery for the real-host rehearsals (scripts/*-rehearsal.sh): a
# packaged billet under REAL systemd in a throwaway container, driven by the
# real commands, asserting on what the host holds afterwards.
#
# WHY CONTAINERS AND NOT FAKES. Every one of these rehearsals exists because a
# path was proved only against a fake: `billet host-upgrade` had never stopped a
# real service or replaced a real binary, `local recover` had never run over a
# deployment a real control plane had served, a CA rotation had never crossed a
# real renewing fleet, and a promotion had never been measured under a real
# partition. A privileged ubuntu:24.04 container with systemd as PID 1 is the
# cheapest real service manager there is, and the same shape
# scripts/test-systemd-lifecycle.sh already trusts.
#
# WHAT EVERY REHEARSAL NEEDS AND THIS FILE REFUSES TO FAKE: a control plane will
# not start without reaching GitHub, so each rehearsal needs a config with a
# working App and the key it names (BILLET_REHEARSAL_APP_CONFIG and
# BILLET_REHEARSAL_APP_KEY), and skips rather than fails without them.
#
# EVERY GATE IN HERE IS &&-CHAINED OR AN if WITH AN EXPLICIT exit, never a `;`
# chain and never a `!`-prefixed pipeline: under `set -e` both let a failing
# checksum or a failing install run on into the step that trusted it
# (billet-shell-gates).

rehearsal_fail() {
    echo >&2
    echo "rehearsal FAILED: $*" >&2
    exit 1
}

rehearsal_step() {
    echo
    echo "=== $*"
}

# rehearsal_require_docker refuses to run where the rehearsal cannot: a machine
# with no docker, or one whose docker serves an architecture billet does not
# package for. The ARCHITECTURE IS THE DAEMON'S, not `uname -m`'s: on a Mac the
# daemon is a Linux VM and that is what the package has to match.
rehearsal_require_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        echo "rehearsal: skipped, docker is not installed" >&2
        exit 0
    fi

    local arch
    arch=$(docker version --format '{{.Server.Arch}}' 2>/dev/null) ||
        rehearsal_fail "docker is installed but its daemon did not answer"

    case "${arch}" in
        amd64 | arm64) REHEARSAL_ARCH=${arch} ;;
        *) rehearsal_fail "docker serves ${arch}, which billet does not package for" ;;
    esac
}

# rehearsal_require_app skips the rehearsal when no real App is available. A
# fake would exercise a code path no deployment takes, so there is nothing
# honest to substitute (see scripts/test-systemd-lifecycle.sh).
rehearsal_require_app() {
    : "${BILLET_REHEARSAL_APP_CONFIG:=}"
    : "${BILLET_REHEARSAL_APP_KEY:=}"

    if [ -z "${BILLET_REHEARSAL_APP_CONFIG}" ] || [ -z "${BILLET_REHEARSAL_APP_KEY}" ]; then
        echo "rehearsal: skipped." >&2
        echo "  Set BILLET_REHEARSAL_APP_CONFIG to a billet.yaml whose github: block names a" >&2
        echo "  working App, and BILLET_REHEARSAL_APP_KEY to the key it points at. A control" >&2
        echo "  plane cannot start without one, and nothing honest stands in for it." >&2
        exit 0
    fi

    test -f "${BILLET_REHEARSAL_APP_CONFIG}" ||
        rehearsal_fail "BILLET_REHEARSAL_APP_CONFIG ${BILLET_REHEARSAL_APP_CONFIG} is not a file"
    test -f "${BILLET_REHEARSAL_APP_KEY}" ||
        rehearsal_fail "BILLET_REHEARSAL_APP_KEY ${BILLET_REHEARSAL_APP_KEY} is not a file"
}

# rehearsal_github_block prints the github: block of the App config for
# splicing into a rehearsal's own config, with the key path rewritten to where
# rehearsal_install_package put the key inside the host. The rehearsal owns
# everything else about its deployment; the App is the one thing it borrows.
# Both the block form and the one-line flow form `github: {...}` (which
# `billet github-app create --config` writes) are handled.
rehearsal_github_block() {
    awk '
        /^github:/ { on = 1; print; next }
        on && /^[^ #]/ { on = 0 }
        on { print }
    ' "${BILLET_REHEARSAL_APP_CONFIG}" |
        sed -E 's#private_key_path: *[^,}]+#private_key_path: /etc/billet/app-private-key.pem#'
}

# rehearsal_fetch_release downloads one release's Debian package for the
# daemon's architecture and its manifest, and VERIFIES the package against the
# release's checksums before anything trusts it. Sets REHEARSAL_DEB and
# REHEARSAL_MANIFEST_SHA256.
#
# `grep` then `sha256sum -c` joined with &&, because `checksums.txt` names every
# asset and a chain that ran on past a FAILED line is exactly how a wrong
# binary gets installed by a script that printed the failure.
rehearsal_fetch_release() {
    local tag=$1 dir=$2
    local base="https://github.com/junioryono/billet/releases/download/${tag}"
    local version=${tag#v}
    local deb="billet_${version}_linux_${REHEARSAL_ARCH}.deb"

    mkdir -p "${dir}"

    curl -fsSL --proto '=https' -o "${dir}/${deb}" "${base}/${deb}" &&
        curl -fsSL --proto '=https' -o "${dir}/checksums.txt" "${base}/checksums.txt" &&
        curl -fsSL --proto '=https' -o "${dir}/release-manifest.json" "${base}/release-manifest.json" ||
        rehearsal_fail "could not download ${tag}'s ${deb}, checksums or manifest"

    (
        cd "${dir}" &&
            grep -F " ${deb}" checksums.txt >"${deb}.sha256" &&
            test -s "${deb}.sha256" &&
            sha256sum -c "${deb}.sha256" >/dev/null
    ) || rehearsal_fail "${deb} does not match ${tag}'s checksums.txt"

    REHEARSAL_DEB="${dir}/${deb}"
    REHEARSAL_MANIFEST_SHA256=$(sha256sum "${dir}/release-manifest.json" | cut -c1-64)

    echo "fetched ${tag}: ${deb} verified; manifest ${REHEARSAL_MANIFEST_SHA256}"
}

# rehearsal_require_dist_package finds the tree's own package (`make dist`) for
# the daemon's architecture and sets REHEARSAL_DIST_DEB. Exactly one, or it
# refuses: two snapshots in dist is a stale one waiting to be installed.
rehearsal_require_dist_package() {
    local root=$1 count=0 candidate

    for candidate in "${root}"/dist/billet_*_linux_"${REHEARSAL_ARCH}".deb; do
        if [ -f "${candidate}" ]; then
            count=$((count + 1))
            REHEARSAL_DIST_DEB=${candidate}
        fi
    done

    test "${count}" -eq 1 ||
        rehearsal_fail "expected exactly one linux/${REHEARSAL_ARCH} package in dist (run make dist), found ${count}"
}

# rehearsal_issue_bundle has the controller issue a node's certificate bundle
# and copies it out to a host directory. As the service account: on a fresh
# package this mints the deployment identity and the authority into the state
# directory systemd created for that account, and a root-run mint leaves files
# the server cannot open (docs/reference/records/restore-rehearsal.md).
rehearsal_issue_bundle() {
    local controller=$1 node=$2 out=$3
    shift 3

    rehearsal_as_billet "${controller}" /usr/bin/billet ca issue "${node}" \
        --config /etc/billet/billet.yaml --out "/tmp/${node}-tls" "$@" >/dev/null ||
        rehearsal_fail "the controller could not issue ${node}'s certificate"
    docker cp "${controller}:/tmp/${node}-tls" "${out}"
    docker exec "${controller}" rm -rf "/tmp/${node}-tls"
}

# rehearsal_install_bundle installs an issued bundle where a node's config
# names it: the key readable by root alone, because the node runs as root.
rehearsal_install_bundle() {
    local node=$1 bundle=$2

    docker exec "${node}" install -d -m 0750 -o root -g billet /etc/billet/tls
    docker cp "${bundle}/node.crt" "${node}:/etc/billet/tls/node.crt"
    docker cp "${bundle}/node.key" "${node}:/etc/billet/tls/node.key"
    docker cp "${bundle}/ca.crt" "${node}:/etc/billet/tls/ca.crt"
    docker exec "${node}" chmod 0600 /etc/billet/tls/node.key
}

# rehearsal_cert_serial prints the serial of the certificate at a path in a host.
rehearsal_cert_serial() {
    docker exec "$1" openssl x509 -in "$2" -noout -serial 2>/dev/null | cut -d= -f2
}

# rehearsal_cert_issuer prints the issuer of the certificate at a path in a host.
rehearsal_cert_issuer() {
    docker exec "$1" openssl x509 -in "$2" -noout -issuer 2>/dev/null | cut -d= -f2-
}

# rehearsal_start_host starts a container whose PID 1 is systemd, on the given
# network, optionally with its own docker daemon (a node that runs the docker
# provider). Waits for systemd to reach running or degraded.
#
# ITS OWN SNAPSHOT STORAGE for the nested daemon, on a real filesystem: overlay
# on overlay is refused, and docker 29 keeps snapshots under containerd, so
# both directories are mounted (scripts/test-systemd-lifecycle.sh measured
# this).
rehearsal_start_host() {
    local name=$1 network=$2 with_docker=$3 storage=$4 alias=${5:-}
    local packages="systemd systemd-sysv dbus curl ca-certificates openssl"
    local volumes=() aliases=()

    if [ "${with_docker}" = yes ]; then
        packages="${packages} docker.io"
        mkdir -p "${storage}/${name}/docker" "${storage}/${name}/containerd"
        volumes=(-v "${storage}/${name}/docker:/var/lib/docker"
            -v "${storage}/${name}/containerd:/var/lib/containerd")
    fi

    # A NETWORK ALIAS SHARED BY SEVERAL HOSTS is how two controllers answer to
    # one name a node dials: docker's DNS returns every host carrying it.
    if [ -n "${alias}" ]; then
        aliases=(--network-alias "${alias}")
    fi

    docker run -d --name "${name}" --hostname "${name}" --network "${network}" \
        "${aliases[@]}" \
        --privileged --cgroupns=host \
        -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
        "${volumes[@]}" \
        -e DEBIAN_FRONTEND=noninteractive \
        --platform "linux/${REHEARSAL_ARCH}" \
        ubuntu:24.04 sh -c \
        "apt-get update -qq && apt-get install -y -qq ${packages} >/dev/null 2>&1 && exec /lib/systemd/systemd" \
        >/dev/null || rehearsal_fail "could not start the ${name} container"

    printf 'waiting for systemd in %s' "${name}"
    local state= i=0
    while [ "${i}" -lt 120 ]; do
        state=$(docker exec "${name}" systemctl is-system-running 2>/dev/null || true)
        case "${state}" in running | degraded) break ;; esac
        printf '.'
        sleep 3
        i=$((i + 1))
    done
    echo " -> ${state:-unknown}"

    case "${state}" in
        running | degraded) ;;
        *)
            docker logs "${name}" 2>&1 | tail -20 >&2
            rehearsal_fail "systemd never came up in ${name}"
            ;;
    esac
}

# rehearsal_install_package installs a billet .deb into a host and stages the
# App key where the packaged config template expects it, owned by the service
# account the postinstall created.
rehearsal_install_package() {
    local name=$1 deb=$2

    docker cp "${deb}" "${name}":/tmp/billet.deb
    docker exec "${name}" sh -c 'apt-get install -y -qq /tmp/billet.deb >/dev/null 2>&1' ||
        rehearsal_fail "the package would not install in ${name}"

    docker cp "${BILLET_REHEARSAL_APP_KEY}" "${name}":/tmp/app-key.pem
    docker exec "${name}" install -m 0600 -o billet -g billet /tmp/app-key.pem /etc/billet/app-private-key.pem
    docker exec "${name}" rm -f /tmp/app-key.pem /tmp/billet.deb
}

# rehearsal_install_config writes a host's /etc/billet/billet.yaml from stdin
# with the mode and owner the package would have given it.
rehearsal_install_config() {
    local name=$1

    docker exec -i "${name}" sh -c 'cat >/tmp/billet.yaml' &&
        docker exec "${name}" install -m 0640 -o root -g billet /tmp/billet.yaml /etc/billet/billet.yaml &&
        docker exec "${name}" rm -f /tmp/billet.yaml &&
        docker exec "${name}" systemctl daemon-reload ||
        rehearsal_fail "could not install the config into ${name}"
}

# rehearsal_as_billet runs a billet command in a host as the service account,
# which is what an operator command on a packaged host must do: run as root it
# leaves root-owned WAL files in a directory the service account owns
# (docs/reference/records/restore-rehearsal.md).
rehearsal_as_billet() {
    local name=$1
    shift
    docker exec "${name}" runuser -u billet -- "$@"
}

# rehearsal_version prints the version the installed binary reports in a host.
rehearsal_version() {
    docker exec "$1" /usr/bin/billet version 2>/dev/null | awk 'NR == 1 { print $2 }'
}

# rehearsal_active prints a unit's ActiveState in a host.
rehearsal_active() {
    docker exec "$1" systemctl is-active "$2" 2>/dev/null || true
}

# rehearsal_wait_for polls a command in a host until it succeeds or the
# deadline (seconds) passes. The command's success is the condition.
rehearsal_wait_for() {
    local deadline=$1 what=$2 name=$3
    shift 3
    local waited=0

    printf 'waiting up to %ss for %s' "${deadline}" "${what}"
    while [ "${waited}" -lt "${deadline}" ]; do
        if docker exec "${name}" "$@" >/dev/null 2>&1; then
            echo " -> after ${waited}s"
            return 0
        fi
        printf '.'
        sleep 5
        waited=$((waited + 5))
    done
    echo " -> NOT within ${deadline}s"
    return 1
}

# rehearsal_journal_step prints the last step a host's newest upgrade journal
# reached, or nothing when there is no journal.
rehearsal_journal_step() {
    docker exec "$1" sh -c '
        j=$(ls -1dt /var/lib/billet/upgrades/*/journal.json 2>/dev/null | head -1)
        test -n "$j" && grep -oE "\"step\": *\"[a-z_]+\"" "$j" | head -1 | sed -E "s/.*: *\"([a-z_]+)\"/\\1/"
    ' 2>/dev/null || true
}

# rehearsal_teardown_hosts removes the containers, the network and the storage
# a rehearsal created. Registered on EXIT by every driver, so a failed
# assertion still leaves nothing running.
rehearsal_teardown_hosts() {
    local network=$1 storage=$2
    shift 2
    local name

    for name in "$@"; do
        docker rm -f "${name}" >/dev/null 2>&1 || true
    done

    docker network rm "${network}" >/dev/null 2>&1 || true
    rm -rf "${storage}" || true
}
