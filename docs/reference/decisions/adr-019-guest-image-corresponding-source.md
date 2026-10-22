# ADR-019: The guest image's corresponding source is published beside it

Accepted, 2026-10-21. Issue #584 (step 3).

## Context

billet publishes its Firecracker guest image as a dated `guest-*` release of this public repository, so it redistributes everything GitHub's runner-images template installs. Read as `/opt/billet/IMAGE_SOURCES.tsv` records it (#622), the first image built that way carried 1106 packages installed with apt, 857 of them with a copyright file that names a GNU license, and a Linux kernel billet builds itself. The GPL and LGPL ask that whoever distributes the binaries make their corresponding source available.

The two versions ask differently:

- **GPLv2, section 3.** Its options are to accompany the binaries with the source (3a), to accompany them with a written offer valid for three years (3b), or, for noncommercial distribution only, to pass on the offer received (3c). Its closing paragraph counts "offering equivalent access to copy the source code from the same place" as distribution of the source.
- **GPLv3, section 6(d).** It allows the source to sit on a different server, "operated by you or a third party", provided there are clear directions to it and the distributor ensures it stays available for as long as the object code does.

The image holds both. git and Linux are GPLv2, and so is much of Ubuntu.

Where a source version can still be found differs by where it came from. Measured on 2026-10-21 through Launchpad's API:

- **Ubuntu's archive.** A superseded version stays downloadable from `launchpad.net/ubuntu/+archive/primary/+sourcefiles/` after the archive removes it from its pool.
- **A PPA.** A superseded version in the git-core PPA does not: 2.54.0, superseded 2026-08-11, answered 404.
- **Removed sources.** The template removes the git and Firefox PPAs after installing from them. It also leaves Adoptium's, PostgreSQL's and Docker's packages behind sources the image no longer names.

## Decision

**Every guest release carries the corresponding source of what it redistributes under a GNU license, as assets of the same release.** `scripts/fetch-image-sources.py` runs in the build job after the image is packed. Every source package `IMAGE_SOURCES.tsv` marks `gnu` or `unclassified` is downloaded at its exact version, from where that version is kept:

| Origin | Where its source is fetched from |
|---|---|
| Ubuntu's archive | Launchpad, which keeps every published version |
| The template's PPAs | Launchpad, which keeps only the published version, so the build fetches it then |
| PostgreSQL's packages | `apt-archive.postgresql.org`, which keeps every version |
| Temurin's JDKs | the source tarball Adoptium publishes beside each Temurin release |
| The guest kernel | the Linux tarball `scripts/kernel/linux.pin` names, with the configuration the kernel was built from |

Each file is checked against the digest its own `.dsc`, Adoptium's checksum file or the pin gives. A source that cannot be fetched and verified fails the build. Everything goes into one tar, split as the image is, because the sources are more files than a release's thousand assets.

- **`image-sources.parts`** names the whole tar with its sha256 and size, then each part, `.part000` upwards, with its own.
- **`image-sources-index.tsv`** accounts for every source the manifest needs: each file fetched, with its source, version, sha256 and size, and each source reviewed as needing none.

Before it tags, the publisher checks four things:

- that the parts the list names, joined in order, are the whole tar it describes;
- that the published index is the one inside the tar, byte for byte;
- that the index accounts for exactly the source packages `image-sources.tsv` marks `gnu` or `unclassified`, with none of the `gnu` ones marked reviewed;
- that the index names the guest kernel's source once. The scripts that built the image and the kernel are this repository at the commit the release names, which GitHub serves as the release's source archive.

**A package with no readable copyright file is reviewed rather than assumed.** It is classified `unclassified`, never `other`. Six vendor packages ship no copyright file and are reviewed by hand in `scripts/runner-images/reviewed-licenses.tsv`, with the license their own repositories state and the date: docker-ce, gh, kubectl, powershell, session-manager-plugin, adoptium-ca-certificates. They are Apache-2.0 or MIT, and the fetcher leaves them out. Any other `unclassified` package has its source fetched. A package that drops out of the review has its source fetched too, or fails the build.

**No written offer is made.** The source is published with the binary, so nothing binds the maintainer to supply it later. Deleting a guest release deletes its source with it, at the same moment its binaries stop being distributed.

## Measured

The first full fetch, on 2026-10-21, ran against the manifest of guest image build 39574003320 on the shared test host. It fetched and verified 440 source packages and the kernel: 1342 files, 3,406,676,484 bytes, in 5 minutes 13 seconds with a 47 MB resident set. The largest were `mysql-8.0` (493 MB), `linux` (the Ubuntu kernel package the cloud base carries, 240 MB), five LLVM toolchains (137 to 227 MB each) and three GCCs (90 to 99 MB each). A release grows from about 15 GB of image parts to about 18.5 GB. The build job had 267 GB free against the 126 GB the image needs.

## Rejected

**Pointing at Launchpad and the vendors instead of publishing.** It costs nothing and satisfies GPLv3's 6(d) for an archive that keeps every version. It does not satisfy GPLv2's "same place" for the GPLv2 packages. A PPA's superseded source vanishes within weeks, so the pointer would break while the image is still published.

**A written offer.** GPLv2's 3(b) allows it, and many vendors of embedded images use it. But it is a three-year promise by the maintainer to supply source on request. That source would have to be kept anyway, outliving the release it was offered for, so it is the same work with an obligation added.

**Publishing no image, as GitHub does.** GitHub publishes only the Packer template and lets nobody download its image, and Blacksmith runs its images as a service and publishes none. Either way leaves no corresponding-source question. billet's design builds one image centrally rather than once per node, which ADR-005 and ADR-014 record. Each deployment rebuilding the image would cost every operator the hours-long build to avoid 3.4 GB of source per release.

**Publishing source only for `gnu` rows.** A package with no copyright file could be under anything. Treating could-not-tell as not GNU is the collapse billet's checks refuse elsewhere.

## Not covered

**Software installed outside apt that bundles an LGPL library.** For example, a toolchain tarball linking libgmp. billet can be exact only about what apt installed and the kernel it built. The notices for tools installed outside apt are #621's.

**Components the image leaves out and a deployment installs on its own nodes** (#602). These are not redistributed by billet.

**Releases published before this decision.** They carry no source. Their removal is recorded in `docs/reference/records/guest-release-removal.md`.
