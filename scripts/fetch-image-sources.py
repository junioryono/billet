#!/usr/bin/env python3
"""Fetch the corresponding source of a billet guest image (#584).

    fetch-image-sources.py MANIFEST KERNEL_VERSION KERNEL_CONFIG OUT

MANIFEST is the image's IMAGE_SOURCES.tsv. Every source package a row classified
`gnu` or `unclassified` names is downloaded at its exact version and checked
against the digests its own description gives, together with the Linux tarball
the guest kernel was built from (KERNEL_VERSION, held to scripts/kernel/linux.pin)
and the configuration it was built with (KERNEL_CONFIG), as the source
`guest-kernel`. Each lands in a directory of its own under OUT, which must be new or
empty, and OUT/index.tsv accounts for every source the manifest needs: one row per
file fetched (source, version, `fetched`, the file, its sha256 and size), and one
per source reviewed as under no GNU license (`reviewed`, and `-` for the rest).

WHERE EACH SOURCE IS KEPT, so a version stays downloadable after its archive
moves on (measured 2026-10-21): Ubuntu's archive, through Launchpad, which keeps
every published version; the PPAs the template adds and removes again, through
Launchpad, which keeps only the published one, so the build fetches it now; the
PostgreSQL archive, apt-archive.postgresql.org, which keeps every version; and
Adoptium's own source tarball beside each Temurin release.

A row classified `unclassified` (no readable copyright file) whose source
scripts/runner-images/reviewed-licenses.tsv names is a package reviewed by hand as
under no GNU license, and is left out. Any other source that cannot be fetched and
verified fails the run, naming each one: a published image without its source is
the thing this exists to prevent.

Environment, for tests only:
  BILLET_SOURCES_LAUNCHPAD   https://launchpad.net
  BILLET_SOURCES_PGDG        https://apt-archive.postgresql.org/pub/repos/apt
  BILLET_SOURCES_ADOPTIUM    https://github.com
  BILLET_SOURCES_KERNEL      https://cdn.kernel.org/pub/linux/kernel/v6.x
  BILLET_SOURCES_PIN         scripts/kernel/linux.pin
  BILLET_SOURCES_REVIEWED    scripts/runner-images/reviewed-licenses.tsv
"""

import concurrent.futures
import hashlib
import os
import re
import shutil
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
LAUNCHPAD = os.environ.get("BILLET_SOURCES_LAUNCHPAD", "https://launchpad.net")
PGDG = os.environ.get("BILLET_SOURCES_PGDG", "https://apt-archive.postgresql.org/pub/repos/apt")
ADOPTIUM = os.environ.get("BILLET_SOURCES_ADOPTIUM", "https://github.com")
KERNEL = os.environ.get("BILLET_SOURCES_KERNEL", "https://cdn.kernel.org/pub/linux/kernel/v6.x")
PIN = os.environ.get("BILLET_SOURCES_PIN", os.path.join(HERE, "kernel", "linux.pin"))
REVIEWED = os.environ.get("BILLET_SOURCES_REVIEWED", os.path.join(HERE, "runner-images", "reviewed-licenses.tsv"))

# THE PPAS GITHUB'S TEMPLATE ADDS FOR A PACKAGE AND REMOVES AGAIN, by the source
# package they serve: a version with `ppa` in it is fetched from there. The
# template's installers name them (install-git.sh's GIT_REPO); a test holds this
# table to them.
PPAS = {"git": "~git-core/+archive/ubuntu/ppa"}

TEMURIN = re.compile(r"^temurin-([0-9]+)-jdk$")
TEMURIN_VERSION = re.compile(r"^([0-9]+)\.0\.([0-9]+)\.([0-9]+)\.[0-9]+\+([0-9]+)(-[0-9A-Za-z.+~]+)?$")
SAFE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9.+~_-]*$")
ATTEMPTS = 5


class Failure(Exception):
    """A source that could not be fetched or verified."""


def retried(url, read):
    """READ applied to the response at URL, retried on what may pass; a 404 is final."""
    delay = 2
    for attempt in range(ATTEMPTS):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers={"User-Agent": "billet"}), timeout=300) as r:
                return read(r)
        except urllib.error.HTTPError as e:
            if e.code in (403, 404, 410) or attempt == ATTEMPTS - 1:
                raise Failure(f"{url}: HTTP {e.code}") from e
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            if attempt == ATTEMPTS - 1:
                raise Failure(f"{url}: {e}") from e
        time.sleep(delay)
        delay *= 2
    raise Failure(f"{url}: no answer")


def get(url):
    """The body at URL, for a description small enough to hold."""
    return retried(url, lambda r: r.read())


def save(directory, name, url, sha256, size=None):
    """Download URL to DIRECTORY/NAME and keep it only if it is the file SHA256 and
    SIZE describe. Streamed: a source tarball can be half a gigabyte."""
    if not SAFE.match(name):
        raise Failure(f"refusing the file name {name!r}")
    path = os.path.join(directory, name)

    def stream(r):
        digest, length = hashlib.sha256(), 0
        with open(path + ".part", "wb") as f:
            while chunk := r.read(1 << 20):
                digest.update(chunk)
                length += len(chunk)
                f.write(chunk)
        return digest.hexdigest(), length

    got, length = retried(url, stream)
    if got != sha256:
        os.remove(path + ".part")
        raise Failure(f"{name}: sha256 {got}, want {sha256}")
    if size is not None and length != size:
        os.remove(path + ".part")
        raise Failure(f"{name}: {length} bytes, want {size}")
    os.replace(path + ".part", path)


def file_sha256(path):
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        while chunk := f.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def dsc_files(text):
    """The (sha256, size, name) of each file a .dsc's Checksums-Sha256 names."""
    files, inside = [], False
    for line in text.splitlines():
        if line.startswith("Checksums-Sha256:"):
            inside = True
            continue
        if inside and line.startswith(" "):
            sha, size, name = line.split()
            files.append((sha, int(size), name))
            continue
        inside = False
    if not files:
        raise Failure("the .dsc names no file under Checksums-Sha256")
    return files


def debian_source(directory, base, source, version):
    """Fetch SOURCE at VERSION as its .dsc describes it, from the directory BASE."""
    upstream = version.split(":", 1)[-1]
    dsc_name = f"{source}_{upstream}.dsc"
    dsc = get(base + urllib.parse.quote(dsc_name))
    with open(os.path.join(directory, dsc_name), "wb") as f:
        f.write(dsc)
    for sha, size, name in dsc_files(dsc.decode("utf-8", "replace")):
        save(directory, name, base + urllib.parse.quote(name), sha, size)


def launchpad(archive, source, version):
    return f"{LAUNCHPAD}/{archive}/+sourcefiles/{urllib.parse.quote(source)}/{urllib.parse.quote(version)}/"


def pgdg(source):
    prefix = source[:4] if source.startswith("lib") else source[:1]
    return f"{PGDG}/pool/main/{prefix}/{urllib.parse.quote(source)}/"


def temurin(directory, major, version):
    """Fetch Adoptium's source tarball for the Temurin MAJOR at the deb VERSION."""
    m = TEMURIN_VERSION.match(version)
    if not m or m.group(1) != major:
        raise Failure(f"temurin-{major}-jdk {version} is not a Temurin version this reads")
    security, patch, build = m.group(2), m.group(3), m.group(4)
    if major == "8":
        tag = f"jdk8u{security}-b{int(build):02d}"
        asset = f"OpenJDK8U-jdk-sources_8u{security}b{int(build):02d}.tar.gz"
    else:
        release = f"{major}.0.{security}" + (f".{patch}" if patch != "0" else "")
        tag = f"jdk-{release}+{build}"
        asset = f"OpenJDK{major}U-jdk-sources_{release}_{build}.tar.gz"
    base = f"{ADOPTIUM}/adoptium/temurin{major}-binaries/releases/download/{urllib.parse.quote(tag)}/"
    sums = get(base + asset + ".sha256.txt").decode("utf-8", "replace").split()
    if len(sums) < 1 or not re.match(r"^[0-9a-f]{64}$", sums[0]):
        raise Failure(f"{asset}.sha256.txt names no sha256")
    save(directory, asset, base + asset, sums[0])


def fetch(out, source, version):
    """Fetch SOURCE at VERSION into a directory of its own under OUT, and return the
    directory.

    THE WHOLE VERSION NAMES THE DIRECTORY, epoch included: two versions that differ
    only in their epoch are two sources, and sharing a directory would let one
    overwrite the other while both report success."""
    if not SAFE.match(source) or not re.match(r"^[0-9A-Za-z.+~:_-]+$", version):
        raise Failure(f"refusing the source {source!r} {version!r}")
    directory = os.path.join(out, f"{source}_{version}")
    os.makedirs(directory)
    t = TEMURIN.match(source)
    if t:
        temurin(directory, t.group(1), version)
    elif ".pgdg" in version:
        debian_source(directory, pgdg(source), source, version)
    elif source in PPAS and "ppa" in version:
        debian_source(directory, launchpad(PPAS[source], source, version), source, version)
    else:
        debian_source(directory, launchpad("ubuntu/+archive/primary", source, version), source, version)
    return directory


def kernel(out, version, config):
    """Fetch the pinned Linux tarball and copy the configuration it was built with."""
    with open(PIN) as f:
        pinned, sha = f.read().split()
    if version != pinned:
        raise Failure(f"the image's kernel is {version} and scripts/kernel/linux.pin pins {pinned}")
    directory = os.path.join(out, f"guest-kernel_{version}")
    os.makedirs(directory)
    save(directory, f"linux-{version}.tar.xz", f"{KERNEL}/linux-{version}.tar.xz", sha)
    shutil.copyfile(config, os.path.join(directory, "config"))
    return directory


def needed(manifest, reviewed):
    """The (source, version) pairs MANIFEST needs the source of, and those it would
    but for the review."""
    with open(manifest, encoding="utf-8") as f:
        lines = f.read().splitlines()
    if not lines or lines[0] != "package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense":
        raise Failure(f"{manifest} is not a source manifest")
    pairs, left = set(), set()
    for line in lines[1:]:
        cells = line.split("\t")
        if len(cells) != 7:
            raise Failure(f"{manifest}: malformed row {line!r}")
        license = cells[6]
        if license == "gnu" or (license == "unclassified" and cells[3] not in reviewed):
            pairs.add((cells[3], cells[4]))
        elif license == "unclassified":
            left.add((cells[3], cells[4]))
        elif license != "other":
            raise Failure(f"{manifest}: unknown license class {license!r}")
    return sorted(pairs), sorted(left - pairs)


def read_reviewed():
    """The sources reviewed by hand as under no GNU license."""
    reviewed = set()
    with open(REVIEWED, encoding="utf-8") as f:
        for n, line in enumerate(f.read().splitlines()):
            if n == 0 or not line:
                continue
            cells = line.split("\t")
            if len(cells) != 4 or not all(cells):
                raise Failure(f"{REVIEWED}: malformed row {line!r}")
            reviewed.add(cells[0])
    return reviewed


def main(argv):
    if len(argv) != 5:
        print(__doc__.split("\n\n")[1], file=sys.stderr)
        return 2
    manifest, kernel_version, kernel_config, out = argv[1:]
    try:
        pairs, reviewed = needed(manifest, read_reviewed())
    except (Failure, OSError) as e:
        print(f"fetch-image-sources: {e}", file=sys.stderr)
        return 1
    # A FRESH DIRECTORY: whatever an earlier run left in one, a partial download
    # or a source an older manifest needed, would be indexed and published.
    if os.path.exists(out) and os.listdir(out):
        print(f"fetch-image-sources: {out} is not empty", file=sys.stderr)
        return 1
    os.makedirs(out, exist_ok=True)

    failures, fetched = [], []
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        jobs = {pool.submit(fetch, out, s, v): (s, v) for s, v in pairs}
        jobs[pool.submit(kernel, out, kernel_version, kernel_config)] = ("guest-kernel", kernel_version)
        for job in concurrent.futures.as_completed(jobs):
            source, version = jobs[job]
            try:
                fetched.append((source, version, job.result()))
            except (Failure, OSError, ValueError) as e:
                failures.append(f"{'linux' if source == 'guest-kernel' else source} {version}: {e}")
    if failures:
        for failure in sorted(failures):
            print(f"fetch-image-sources: {failure}", file=sys.stderr)
        print(f"fetch-image-sources: {len(failures)} of {len(jobs)} sources could not be fetched and verified", file=sys.stderr)
        return 1

    total, files = 0, 0
    rows = [(s, v, "reviewed", "-", "-", "-") for s, v in reviewed]
    for source, version, directory in fetched:
        for name in sorted(os.listdir(directory)):
            path = os.path.join(directory, name)
            size = os.path.getsize(path)
            total += size
            files += 1
            rows.append((source, version, "fetched", os.path.relpath(path, out), file_sha256(path), size))
    with open(os.path.join(out, "index.tsv"), "w", encoding="utf-8") as f:
        f.write("source\tversion\tstatus\tfile\tsha256\tsize\n")
        for row in sorted(rows):
            f.write("\t".join(str(c) for c in row) + "\n")
    print(f"fetch-image-sources: {len(jobs)} sources, {files} files, {total} bytes; {len(reviewed)} reviewed as needing none")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
