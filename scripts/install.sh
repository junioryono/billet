#!/bin/sh
#
# Install billet.
#
#   curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
#
# Resolves the signed `stable` channel to one release, downloads it for this
# platform, VERIFIES ITS CHECKSUM, and installs the binary to /usr/local/bin. Set
# BILLET_OS and BILLET_ARCH when the target differs from the machine running this
# script. It does not create users, write config, or start anything — use the .deb
# or .rpm for the systemd units.
#
#   BILLET_CHANNEL=candidate  follow a different signed channel
#   BILLET_VERSION=v0.4.0     install exactly that release, consulting no channel
#
# POSIX sh, not bash: this runs on whatever a fresh host happens to have.
set -eu
LC_ALL=C
export LC_ALL

REPO="junioryono/billet"
BIN="billet"
INSTALL_DIR="${BILLET_INSTALL_DIR:-/usr/local/bin}"
VERSION="${BILLET_VERSION:-latest}"

# CHANNEL is the signed pointer to follow when no exact version is asked for.
# An exact BILLET_VERSION never consults it: somebody who typed a version is
# asking for that version.
CHANNEL="${BILLET_CHANNEL:-stable}"

# HTTPS ONLY, INCLUDING ACROSS REDIRECTS. -L follows wherever it is sent, and a
# release asset redirect that downgraded to http would put the bytes this script
# is about to run on the wire in the clear. The checksum is fetched the same way,
# so an attacker who could rewrite one could rewrite both.
CURL="curl -fsSL --proto =https --proto-redir =https"

die() {
    printf 'install: %s\n' "$*" >&2
    exit 1
}

need() {
    command -v "$1" >/dev/null 2>&1 || die "this needs $1, which is not installed"
}

# THE PLATFORM IS TRANSLATED, NOT GUESSED. `uname` says Linux/Darwin and
# x86_64/aarch64; the archives are named with Go's linux/darwin and amd64/arm64.
# Getting this mapping wrong produces a 404 that reads like the release is
# missing rather than like the script is wrong, so an unsupported platform is
# refused BY NAME instead.
normalize_platform() {
    os="$1"
    arch="$2"
    mode="${3:-fatal}"

    case "${os}" in
        Linux | linux) os=linux ;;
        Darwin | darwin) os=darwin ;;
        *)
            [ "${mode}" = "quiet" ] && return 1
            die "${os} is not a platform billet builds for (linux and darwin only)"
            ;;
    esac

    case "${arch}" in
        x86_64 | amd64) arch=amd64 ;;
        aarch64 | arm64) arch=arm64 ;;
        *)
            [ "${mode}" = "quiet" ] && return 1
            die "${arch} is not an architecture billet builds for (amd64 and arm64 only)"
            ;;
    esac

    # macOS on Intel is not built. Saying so beats a 404.
    if [ "${os}" = "darwin" ] && [ "${arch}" = "amd64" ]; then
        [ "${mode}" = "quiet" ] && return 1
        die "billet does not build for macOS on Intel; only Apple Silicon"
    fi

    echo "${os}_${arch}"
}

detect_target_platform() {
    os_set="${BILLET_OS+x}"
    arch_set="${BILLET_ARCH+x}"
    [ "${os_set}" = "x" ] && [ "${arch_set}" = "x" ] ||
        die "BILLET_OS and BILLET_ARCH must be set together"
    [ -n "${BILLET_OS}" ] && [ -n "${BILLET_ARCH}" ] ||
        die "BILLET_OS and BILLET_ARCH must not be empty"
    normalize_platform "${BILLET_OS}" "${BILLET_ARCH}"
}

# sha256 has three spellings across the platforms billet supports.
# resolve_channel reads the signed channel statement and prints the tag it names.
#
# PRINTS NOTHING ON ANY FAILURE, and the caller falls back. Every refusal here is
# "billet cannot tell which release is current", and the alternative to falling
# back is a first install that does not happen — on a machine that has no billet
# yet, and so no way to be told why.
#
# THE TAG IS EXTRACTED WITHOUT A JSON PARSER, because this is POSIX sh on whatever
# a fresh host happens to have and jq is not there. The pattern is anchored on the
# quoted key so a tag appearing in some other field cannot be picked up.
resolve_channel() {
    channel="$1"
    url="https://raw.githubusercontent.com/${REPO}/release-channel/${channel}.json"

    body="$(${CURL} "${url}" 2>/dev/null)" || return 0

    tag="$(printf '%s' "${body}" |
        tr ',' '\n' |
        sed -n 's/.*"tag"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
        head -n1)"

    # REFUSED UNLESS IT IS A RELEASE TAG. The document is fetched over the network
    # and interpolated into a URL below; anything that is not vX.Y.Z is either a
    # channel this script cannot read or bytes somebody else served.
    case "${tag}" in
    v[0-9]*.[0-9]*.[0-9]*) printf '%s' "${tag}" ;;
    *) return 0 ;;
    esac
}

checksum_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    else
        die "this needs sha256sum or shasum to verify the download"
    fi
}

main() {
    need curl
    need tar

    host_os="$(uname -s)" || die "could not detect the host operating system with uname"
    host_arch="$(uname -m)" || die "could not detect the host architecture with uname"
    if host_platform="$(normalize_platform "${host_os}" "${host_arch}" quiet)"; then
        host_supported=true
    else
        host_supported=false
        host_platform=
    fi

    if [ "${BILLET_OS+x}" = "x" ] || [ "${BILLET_ARCH+x}" = "x" ]; then
        platform="$(detect_target_platform)"
        native=false
        if [ "${host_supported}" = true ] && [ "${platform}" = "${host_platform}" ]; then
            native=true
        fi
    else
        [ "${host_supported}" = true ] || normalize_platform "${host_os}" "${host_arch}"
        platform="${host_platform}"
        native=true
    fi

    # THE SIGNED CHANNEL IS WHAT "latest" MEANS NOW, and the distinction matters.
    # GitHub's `releases/latest` is whatever the newest non-prerelease happens to
    # be at the moment somebody asks — including one cut on an older series. The
    # channel is a SIGNED statement naming exactly one release, with an expiry so
    # an old copy cannot be replayed, and an assertion that the release was proved
    # immutable before the pointer moved.
    #
    # A FAILURE TO RESOLVE IT FALLS BACK, DELIBERATELY. A host installing billet
    # for the first time may sit behind a proxy that serves raw.githubusercontent
    # badly, and refusing to install at all over a pointer is worse than installing
    # what GitHub calls latest — this script's integrity check is the checksum
    # below, which is unchanged either way. What the channel buys here is naming
    # the right release, not proving it; `billet host-upgrade` is the path that
    # verifies a signature, because by then there is a billet to do it with.
    if [ "${VERSION}" = "latest" ]; then
        resolved="$(resolve_channel "${CHANNEL}")"
        if [ -n "${resolved}" ]; then
            echo "The ${CHANNEL} channel names ${resolved}."
            VERSION="${resolved}"
            base="https://github.com/${REPO}/releases/download/${resolved}"
        else
            echo "Could not read the ${CHANNEL} channel; using GitHub's latest release." >&2
            base="https://github.com/${REPO}/releases/latest/download"
        fi
    else
        base="https://github.com/${REPO}/releases/download/${VERSION}"
    fi

    tmp="$(mktemp -d)"
    staged=
    install_prefix=
    cleanup() {
        if [ -n "${staged}" ]; then
            ${install_prefix} rm -f "${staged}" >/dev/null 2>&1 || true
        fi
        rm -rf "${tmp}" >/dev/null 2>&1 || true
    }
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    echo "Fetching the checksums..."
    ${CURL} "${base}/checksums.txt" -o "${tmp}/checksums.txt" ||
        die "could not fetch ${base}/checksums.txt — is ${VERSION} a release that exists?"

    # THE ARCHIVE NAME COMES OUT OF THE CHECKSUM FILE rather than being
    # reconstructed here. The name carries the version, so building it locally
    # would mean this script has to know how versions are formatted — and it
    # would silently stop matching the day that changes. The checksum file is the
    # release's own statement of what it contains.
    archive="$(grep -E "_${platform}\.tar\.gz\$" "${tmp}/checksums.txt" | head -n1 | sed 's/.*  *//')"
    [ -n "${archive}" ] ||
        die "this release has no build for ${platform}"
    case "${archive}" in
        */* | *[!A-Za-z0-9._-]*) die "release metadata contains an unsafe archive name: ${archive}" ;;
    esac

    # THE RELEASE MANIFEST, IF THIS RELEASE PUBLISHES ONE. It is what a rollout
    # decides on: one immutable document naming every artifact, so a host can say
    # which BYTES it is running rather than only which version it was built as.
    # Releases cut before manifests existed have none, and this carries on without
    # one — the host then reports nothing, which a rollout reads as "cannot tell".
    manifest=
    if ${CURL} "${base}/release-manifest.json" -o "${tmp}/release-manifest.json" 2>/dev/null &&
        [ -s "${tmp}/release-manifest.json" ]; then
        manifest="${tmp}/release-manifest.json"
    fi

    echo "Downloading ${archive}..."
    payload="${tmp}/payload.tar.gz"
    ${CURL} "${base}/${archive}" -o "${payload}" ||
        die "could not download ${base}/${archive}"

    # VERIFIED BEFORE IT IS UNPACKED. A tarball fetched over the network and
    # extracted unchecked is a remote code execution waiting for a bad day, and
    # the checksum costs nothing.
    want="$(grep "  ${archive}\$" "${tmp}/checksums.txt" | cut -d' ' -f1)"
    got="$(checksum_of "${payload}")"

    [ -n "${want}" ] || die "no checksum published for ${archive}"

    if [ "${want}" != "${got}" ]; then
        die "checksum mismatch for ${archive}
  published: ${want}
  got:       ${got}
This is not the file the release says it is. Do not install it."
    fi

    # STREAM ONLY THE EXPECTED MEMBER. Extracting the whole archive would let a
    # checksum-listed release write links or traversal entries outside this
    # temporary directory before billet ever inspected the result.
    extracted="${tmp}/${BIN}"
    tar -xOzf "${payload}" "${BIN}" > "${extracted}" ||
        die "the archive did not contain a readable ${BIN} file"
    [ -s "${extracted}" ] || die "the archive contained an empty ${BIN} file"

    chmod +x "${extracted}"

    case "${INSTALL_DIR}" in
        /*) ;;
        *) die "BILLET_INSTALL_DIR must be an absolute path, got ${INSTALL_DIR}" ;;
    esac

    [ -d "${INSTALL_DIR}" ] ||
        die "${INSTALL_DIR} does not exist. Create it, or set BILLET_INSTALL_DIR
to somewhere that does."
    destination="${INSTALL_DIR}/${BIN}"
    [ ! -d "${destination}" ] ||
        die "${destination} is a directory; refusing to install a file into it"

    # RENAMED INTO PLACE FROM THE SAME DIRECTORY, so the replacement is atomic.
    #
    # mv from a temporary directory is usually /tmp, which is usually a different
    # filesystem — and a cross-filesystem mv is a copy followed by a delete. An
    # interruption or a full disk in the middle of that leaves a truncated
    # executable where a working billet used to be, which is a worse outcome than
    # any failure this script is trying to avoid.
    if [ -w "${INSTALL_DIR}" ]; then
        install_prefix=
    elif command -v sudo >/dev/null 2>&1; then
        echo "Installing to ${INSTALL_DIR} (needs sudo)..."
        install_prefix=sudo
    else
        die "${INSTALL_DIR} is not writable and sudo is not available.
Set BILLET_INSTALL_DIR to somewhere you can write."
    fi

    staged="$(${install_prefix} mktemp "${INSTALL_DIR}/.${BIN}.incoming.XXXXXX")" ||
        die "could not create a staging file in ${INSTALL_DIR}"
    ${install_prefix} cp "${extracted}" "${staged}" &&
        ${install_prefix} chmod 0755 "${staged}" ||
        die "could not stage billet in ${INSTALL_DIR}"

    installed_version=
    if [ "${native}" = true ]; then
        version_output="$("${staged}" version)" ||
            die "the staged ${platform} binary could not run on this host; the existing billet was not replaced"
        installed_version="$(printf '%s\n' "${version_output}" | sed -n '1p')"
        [ -n "${installed_version}" ] ||
            die "the staged ${platform} binary reported no version; the existing billet was not replaced"
    fi

    ${install_prefix} mv "${staged}" "${destination}" ||
        die "could not install to ${INSTALL_DIR}"
    [ -f "${destination}" ] ||
        die "the final path ${destination} is not the installed billet file"
    staged=

    # WHICH MANIFEST PRODUCED THIS, RECORDED BY BILLET RATHER THAN BY THIS SCRIPT.
    #
    # A rollout treats the record as PROOF — it blocks a host whose manifest
    # disagrees with the decision — so writing one means attesting that these bytes
    # came from that manifest. This script cannot establish that: it verifies the
    # archive against a checksums file fetched from the same place as the manifest,
    # so a manifest served beside a different archive would produce a host that
    # converges a rollout as PROVED on bytes the manifest never named. `billet
    # release record` parses the manifest, finds the entry for this platform, and
    # refuses unless the archive hashes to what that entry says.
    #
    # AFTER THE RENAME, so the record describes the file that is actually
    # installed, and only when the staged binary could run here — a cross-platform
    # install has no billet on this machine able to check anything.
    if [ -n "${manifest}" ] && [ "${native}" = true ]; then
        ${install_prefix} "${destination}" release record \
            --manifest "${manifest}" \
            --archive "${payload}" \
            --binary "${destination}" >/dev/null 2>&1 ||
            printf '%s\n' "Note: this install could not be tied to a release manifest, so a rollout will read this host's version rather than the bytes it is running."
    fi

    echo
    if [ "${native}" = true ]; then
        printf 'Installed: %s\n' "${installed_version}"
    else
        printf 'Installed %s billet to %s\n' "${platform}" "${destination}"
    fi
    echo
    if [ "${native}" = true ]; then
        echo "Next:
  billet init --org YOUR-ORG --runner-group YOUR-GROUP \\
    --workflow YOUR-ORG/REPO/.github/workflows/ci.yml@refs/heads/main \\
    --config ./billet.yaml
  billet github-app create --org YOUR-ORG --config ./billet.yaml
  billet check --config ./billet.yaml

init generates a config that runs and needs no hand edits; github-app
create writes the App identity into that same file. The runner group is
made in GitHub first — a container shares the host kernel, so billet only
runs workflows an organization runner group explicitly allows.

The whole path, including the GitHub side:
  https://github.com/junioryono/billet/blob/main/docs/first-deployment.md

For a machine that should run jobs across reboots, install the package"
        echo "instead — it ships systemd units and a config skeleton:"
        echo "  https://github.com/${REPO}/releases/latest"
    else
        echo "Next:"
        echo "  Supply ${destination} to your provisioning tool, or copy it to the"
        echo "  ${platform} target. This binary cannot run on this host."
    fi
}

main "$@"
