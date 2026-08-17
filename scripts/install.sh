#!/bin/sh
#
# Install billet.
#
#   curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
#
# Downloads the latest release for this platform, VERIFIES ITS CHECKSUM, and
# installs the binary to /usr/local/bin. Set BILLET_OS and BILLET_ARCH when the
# target differs from the machine running this script. It does not create users,
# write config, or start anything — use the .deb or .rpm for the systemd units.
#
# POSIX sh, not bash: this runs on whatever a fresh host happens to have.
set -eu

REPO="junioryono/billet"
BIN="billet"
INSTALL_DIR="${BILLET_INSTALL_DIR:-/usr/local/bin}"
VERSION="${BILLET_VERSION:-latest}"

# HTTPS ONLY, INCLUDING ACROSS REDIRECTS. -L follows wherever it is sent, and a
# release asset redirect that downgraded to http would put the bytes this script
# is about to run on the wire in the clear. The checksum is fetched the same way,
# so an attacker who could rewrite one could rewrite both.
CURL="curl -fsSL --proto =https --proto-redir =https"

die() {
    echo "install: $*" >&2
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

    case "${os}" in
        Linux | linux) os=linux ;;
        Darwin | darwin) os=darwin ;;
        *) die "${os} is not a platform billet builds for (linux and darwin only)" ;;
    esac

    case "${arch}" in
        x86_64 | amd64) arch=amd64 ;;
        aarch64 | arm64) arch=arm64 ;;
        *) die "${arch} is not an architecture billet builds for (amd64 and arm64 only)" ;;
    esac

    # macOS on Intel is not built. Saying so beats a 404.
    if [ "${os}" = "darwin" ] && [ "${arch}" = "amd64" ]; then
        die "billet does not build for macOS on Intel; only Apple Silicon"
    fi

    echo "${os}_${arch}"
}

detect_host_platform() {
    normalize_platform "$(uname -s)" "$(uname -m)"
}

detect_platform() {
    if [ -n "${BILLET_OS:-}" ] || [ -n "${BILLET_ARCH:-}" ]; then
        [ -n "${BILLET_OS:-}" ] && [ -n "${BILLET_ARCH:-}" ] ||
            die "BILLET_OS and BILLET_ARCH must be set together"
        normalize_platform "${BILLET_OS}" "${BILLET_ARCH}"
        return
    fi

    detect_host_platform
}

report_install() {
    installed="$1"
    platform="$2"

    if [ "${platform}" = "$(detect_host_platform)" ]; then
        echo "Installed: $("${installed}" version | head -n1)"
    else
        echo "Installed ${platform} billet to ${installed}"
    fi
}

# sha256 has three spellings across the platforms billet supports.
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

    platform="$(detect_platform)"

    if [ "${VERSION}" = "latest" ]; then
        base="https://github.com/${REPO}/releases/latest/download"
    else
        base="https://github.com/${REPO}/releases/download/${VERSION}"
    fi

    tmp="$(mktemp -d)"
    trap 'rm -rf "${tmp}"' EXIT INT TERM

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

    echo "Downloading ${archive}..."
    ${CURL} "${base}/${archive}" -o "${tmp}/${archive}" ||
        die "could not download ${base}/${archive}"

    # VERIFIED BEFORE IT IS UNPACKED. A tarball fetched over the network and
    # extracted unchecked is a remote code execution waiting for a bad day, and
    # the checksum costs nothing.
    want="$(grep "  ${archive}\$" "${tmp}/checksums.txt" | cut -d' ' -f1)"
    got="$(checksum_of "${tmp}/${archive}")"

    [ -n "${want}" ] || die "no checksum published for ${archive}"

    if [ "${want}" != "${got}" ]; then
        die "checksum mismatch for ${archive}
  published: ${want}
  got:       ${got}
This is not the file the release says it is. Do not install it."
    fi

    tar -xzf "${tmp}/${archive}" -C "${tmp}"
    [ -f "${tmp}/${BIN}" ] || die "the archive did not contain ${BIN}"

    chmod +x "${tmp}/${BIN}"

    case "${INSTALL_DIR}" in
        /*) ;;
        *) die "BILLET_INSTALL_DIR must be an absolute path, got ${INSTALL_DIR}" ;;
    esac

    [ -d "${INSTALL_DIR}" ] ||
        die "${INSTALL_DIR} does not exist. Create it, or set BILLET_INSTALL_DIR
to somewhere that does."

    # RENAMED INTO PLACE FROM THE SAME DIRECTORY, so the replacement is atomic.
    #
    # mv from a temporary directory is usually /tmp, which is usually a different
    # filesystem — and a cross-filesystem mv is a copy followed by a delete. An
    # interruption or a full disk in the middle of that leaves a truncated
    # executable where a working billet used to be, which is a worse outcome than
    # any failure this script is trying to avoid.
    staged="${INSTALL_DIR}/.${BIN}.incoming"

    install_with() {
        $1 cp "${tmp}/${BIN}" "${staged}" &&
            $1 chmod 0755 "${staged}" &&
            $1 mv "${staged}" "${INSTALL_DIR}/${BIN}"
    }

    if [ -w "${INSTALL_DIR}" ]; then
        install_with "" || die "could not install to ${INSTALL_DIR}"
    elif command -v sudo >/dev/null 2>&1; then
        echo "Installing to ${INSTALL_DIR} (needs sudo)..."
        install_with sudo || die "could not install to ${INSTALL_DIR}"
    else
        die "${INSTALL_DIR} is not writable and sudo is not available.
Set BILLET_INSTALL_DIR to somewhere you can write."
    fi

    echo
    report_install "${INSTALL_DIR}/${BIN}" "${platform}"
    echo
    echo "Next:"
    echo "  billet github-app create --org YOUR-ORG"
    echo
    echo "That prints a github: block. Put it in a billet.yaml alongside your"
    echo "tiers — there is a fully commented example at"
    echo "  https://github.com/${REPO}/blob/main/billet.example.yaml"
    echo "then:"
    echo "  billet check --config /path/to/billet.yaml"
    echo
    echo "For a machine that should run jobs across reboots, install the package"
    echo "instead — it ships systemd units and a config skeleton:"
    echo "  https://github.com/${REPO}/releases/latest"
}

if [ "${BILLET_INSTALL_SH_TEST:-0}" != "1" ]; then
    main "$@"
fi
