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
            rehearsal_sha256_check "${deb}.sha256"
    ) || rehearsal_fail "${deb} does not match ${tag}'s checksums.txt"

    REHEARSAL_DEB="${dir}/${deb}"
    REHEARSAL_MANIFEST_SHA256=$(rehearsal_sha256_of "${dir}/release-manifest.json")

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

# rehearsal_sha256_check verifies the "<digest>  <file>" lines in a sum file,
# and rehearsal_sha256_of prints a file's digest, with whichever of coreutils'
# sha256sum or perl's shasum the host has: a Mac running Docker Desktop has the
# second and not the first, and a helper that quietly fell through would make
# "verified" mean "no tool was found".
rehearsal_sha256_check() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c "$1" >/dev/null
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 -c "$1" >/dev/null
    else
        rehearsal_fail "neither sha256sum nor shasum is installed, so nothing can verify a download"
    fi
}

rehearsal_sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -c1-64
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -c1-64
    else
        rehearsal_fail "neither sha256sum nor shasum is installed, so nothing can digest a manifest"
    fi
}

# rehearsal_version_ge succeeds when release tag $1 is at or above tag $2, by
# strict vX.Y.Z arithmetic. Not `sort -V`: BSD sort has no -V, and a comparison
# that fails inside an `if` would silently pick the wrong branch on a Mac.
rehearsal_version_ge() {
    local IFS=.
    local -a a b
    read -r -a a <<<"${1#v}"
    read -r -a b <<<"${2#v}"

    local i
    for i in 0 1 2; do
        if [ "${a[i]:-0}" -gt "${b[i]:-0}" ]; then return 0; fi
        if [ "${a[i]:-0}" -lt "${b[i]:-0}" ]; then return 1; fi
    done

    return 0
}

# rehearsal_clock prints a host's clock in the form journalctl --since takes,
# for marking a boundary before which a log line does not count.
rehearsal_clock() {
    docker exec "$1" date -u '+%Y-%m-%d %H:%M:%S'
}

# rehearsal_wait_registered waits until the controller's journal records a
# REGISTRATION of the node after the given clock mark.
#
# NOT `billet status`, deliberately: status prints every node the ledger knows,
# reachable or not, so a row that survived a restore, a restart or a shared
# ledger satisfies a grep for the name without any host having connected. The
# server's "node registered" line is written by the registration handler when
# a host actually arrives, and a mark taken before the boundary makes it causal.
rehearsal_wait_registered() {
    local deadline=$1 controller=$2 node=$3 since=$4

    rehearsal_wait_for "${deadline}" "${node} to register with ${controller}" "${controller}" \
        sh -c "journalctl -u billet-server --since '${since}' --no-pager -o cat | grep -q 'node registered node=${node}'"
}

# rehearsal_teardown_scale_sets removes every scale set a rehearsal's control
# plane created, and returns the command's own status so a cleanup can refuse
# to call a run green when the scale set is still there. Extra arguments are
# NAME=VALUE pairs the command needs in its environment (a PostgreSQL DSN);
# they are rendered as docker's -e on the outside and env's assignments on the
# inside, because the two vocabularies differ and a flag that leaks across is
# a teardown that never ran.
rehearsal_teardown_scale_sets() {
    local controller=$1
    shift
    local -a outside=() inside=()
    local pair

    for pair in "$@"; do
        outside+=(-e "${pair}")
        inside+=("${pair}")
    done

    docker exec "${outside[@]}" "${controller}" runuser -u billet -- env "${inside[@]}" \
        /usr/bin/billet teardown --all --yes --config /etc/billet/billet.yaml 2>&1 | tail -3
    return "${PIPESTATUS[0]}"
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

# rehearsal_install_package installs a billet .deb into a host.
rehearsal_install_package() {
    local name=$1 deb=$2

    docker cp "${deb}" "${name}":/tmp/billet.deb
    docker exec "${name}" sh -c 'apt-get install -y -qq /tmp/billet.deb >/dev/null 2>&1' ||
        rehearsal_fail "the package would not install in ${name}"
    docker exec "${name}" rm -f /tmp/billet.deb
}

# rehearsal_install_app_key stages the App key where the packaged config
# template expects it, owned by the service account the postinstall created.
# CONTROLLERS ONLY: a node has no use for the App, and a rehearsal that put the
# real credential on every compute host would be rehearsing a leak.
rehearsal_install_app_key() {
    local name=$1

    docker cp "${BILLET_REHEARSAL_APP_KEY}" "${name}":/tmp/app-key.pem
    docker exec "${name}" install -m 0600 -o billet -g billet /tmp/app-key.pem /etc/billet/app-private-key.pem
    docker exec "${name}" rm -f /tmp/app-key.pem
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
