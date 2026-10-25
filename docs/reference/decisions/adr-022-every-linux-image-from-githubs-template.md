# ADR-022: Every Linux image is GitHub's template build, on both architectures, and one release carries both

Accepted, 2026-10-25. Issues #24 and #25. Extends [ADR-014](adr-014-guest-image-from-githubs-template.md) from the x64 Firecracker guest to an arm64 guest and to the EC2 AMI on both architectures. Supersedes what remained of [ADR-005](adr-005-runner-image-parity.md), which governed the AMI.

## Context

ADR-014 made the x64 Firecracker guest GitHub's own build: `actions/runner-images` vendored at one commit, its template run under `systemd-nspawn`, and every difference in one reviewed file. Two images were left on other routes.

**The EC2 AMI** still builds from GitHub's declaration with billet's own installers (`internal/runnerimages/install-toolcache.sh`). Those installers carry the declaration and a first set of what GitHub installs outside it (#211), including the Azure and Google Cloud CLIs. They do not carry:
- Rust, Swift, Haskell, Kotlin and Julia;
- Miniconda, vcpkg, Homebrew and nvm;
- the browsers and their drivers.

ADR-005 lists these as still to come (#25). #212 reimplemented nine of them and was closed as superseded by ADR-014, which covered only the guest. A workflow that calls `cargo` with no setup step passes on GitHub and on a Firecracker tier, and fails on an EC2 tier.

**arm64** has an AMI, built with the same installers, and no published Firecracker guest. At the commit billet pins (`e75633902841`, the `ubuntu24/20260920.314` image release), GitHub's repository carries an arm64 build of its own. Read from the vendored tree and from upstream at that commit, 2026-10-25:
- `images/ubuntu/templates/build.ubuntu-24_04-arm64.pkr.hcl` reads `images/ubuntu/toolsets/toolset-2404-arm64.json`. billet vendors that toolset nowhere yet; it vendors only `toolset-2404.json`, into `internal/runnerimages`.
- The arm64 template runs the x64 template's steps except nine: Bicep, the CodeQL bundle, Microsoft Edge, Google Chrome, Haskell, Miniconda, Julia, the Android SDK and PyPy.
- 23 of the 83 build scripts branch on the architecture (`is_arm64`, `Test-IsArm64` or `aarch64`), mostly to pick a download. The rest are architecture-neutral.
- GitHub's tests skip Chrome, Edge, Chromium, Android, Haskell and Bicep on arm64 (`Test-IsArm64`). Firefox's are not skipped.

So "parity on arm64" has a definition GitHub wrote: its own arm64 image, which carries Rust and Swift and has no Haskell.

**One release, one manifest.**
- A guest release holds one `manifest.json`, and the signed channel pointer names one release.
- `Manifest.Usable` already refuses an image built for another architecture.
- Every deployed reader fetches `manifest.json` by that name and refuses a schema it does not know, so changing either the manifest's schema or the channel's would be a flag day (`billet-guest-images`).
- A Ceph generation records its guest contract and not its architecture. `@verified` resolves among generations by contract alone.

## Decision

**Every Linux image billet builds runs GitHub's template for its architecture.**
- **Where it runs.**
  - The x64 and arm64 Firecracker guests run under nspawn, on Ubuntu's cloud root filesystem for their architecture, as ADR-014 describes. Each base is pinned by its own checksum line from a signature-checked `SHA256SUMS`.
  - The x64 and arm64 AMIs run the template directly on the EC2 builder. The builder is an Ubuntu 24.04 virtual machine, which is the setting GitHub's own Packer build runs it in on Azure.
  - The arm64 toolset is vendored at the pinned commit beside the x64 one, under the same MIT notice, with its own digest. Each target gets a plan derived from its template.
- **One difference list, with a scope.** `differences.tsv` gains a column naming where each difference applies: `guest-x64`, `guest-arm64`, `ami-x64`, `ami-arm64`, or every target.
  - The guest's own differences apply only to the guest: GRUB's absence, the virtio-disk and kernel-command-line tests, snapd. The AMI boots through GRUB on NVMe and runs snapd.
  - The Azure agent's differences, Docker's classic store and the build user Homebrew runs as apply everywhere.
  - An entry naming a step that one of its targets' plans lacks stops the build before anything runs, as it does today.
- **The AMI builder has a stated end state.**
  - The template reboots once, mid-build. The build runs as a oneshot systemd unit that knows a build identity from its user data: a nonce billet invents per build.
  - Before the reboot, the build writes that identity, the next step and the phase it is in. At the next boot it continues from that record.
  - A boot that finds the unit enabled and no record carrying its identity is an unplanned restart. The build reports failure and stops.
  - It ends one of two ways. It prints a nonce-bound verdict to the console and powers off. Or it prints a nonce-bound failure and halts without powering off, so billet's poll reads the failure while the machine is still there to debug.
  - Before the success verdict, it disables and removes its own unit and inputs. An instance launched from the AMI therefore never runs the build.
  - billet images a stopped builder only after reading the success verdict for this build's nonce on its console. A stop alone is no longer success. The template's own output goes to the journal and not to the serial console, so it cannot push the verdict out of EC2's 64 KiB console tail, and nothing it prints carries the nonce.
  - The builder is a Nitro type, which serves the live console read the verifier already uses.
- **What the AMI is checked with.** GitHub's own Pester suite, run by the template during the build, checks what GitHub's image carries. It runs with the template's environment, though, and a job on an AMI starts under `env -i` with `/etc/billet-image-env`.
  - The boot verifier therefore keeps a check through the job's own path: on a machine booted from the AMI, `billet_as_runner` runs each architecture's toolchains (`rustc`, `cargo`, `swift`, `go`, `node`, `python3`, `java`, `dotnet`, `pwsh`, `ghc` on x64) and each accepted component's binary.
  - `/etc/billet-image-env` is written from the template's `/etc/environment` by `write-image-env.sh`, the guest's one writer of it.
- **What is retired.** The tree reaches the builder in the S3 payload that already carries the AMI's installers. `install-toolcache.sh`, its unpublished-entry record and the declaration-driven toolcache gate are retired, because nothing builds with them any more.

**One switch decides the left-out components on every backend: `images.accept`.**
- **The two lists.** The published guest leaves out what billet may not redistribute (#584). On x64 that is the Android SDK, CodeQL, Chrome, Edge and Firefox. On arm64 it is Firefox alone, since GitHub's arm64 template installs none of the other four.
- **The AMI follows the same switch.** An AMI is not redistributed, because `billet ami build` makes it in the operator's own account. Its build still installs a component only when `images.accept` names it, by running the completer the guest carries, so a deployment has the same components on both backends.
  - For the Android SDK, CodeQL, Chrome and Edge, writing the name is accepting their licences. The AMI build used to accept the Android SDK's licence on everyone's behalf; it no longer does.
  - Firefox's restriction concerns redistributing an altered build, which an AMI is not. Naming it is a choice of contents, kept on the same switch so the two backends do not drift.
- **What the AMI records.** The components it carries and their versions go in a tag beside the contract tag. `AMIContract` becomes 4.
- **Migration.** A contract-3 AMI carries the Android SDK, and on x64 CodeQL, whatever `images.accept` says. For such an image, `billet check` names the components a rebuild drops unless the deployment accepts them.

**One release carries both architectures, inside the schemas every deployed reader already reads.**
- **Names.**
  - `manifest.json` stays the x86_64 image, exactly what an older billet expects.
  - The aarch64 image is described by `manifest-aarch64.json` and signed by `manifest-aarch64.sigstore.json` in the same dated release.
  - Its assets carry the architecture in their names: `rootfs-aarch64.img.zst.partNNN`, `vmlinux-billet-aarch64`, `image-sources-aarch64.*`.
  - The channel pointer is unchanged.
- **No flag day.** Neither schema changes. An older billet on arm64 reads `manifest.json` and `Usable` refuses it, as it does today.
- **How a newer billet reads it.** It reads the manifest named for its own architecture.
  - A release is immutable once the channel names it, so a release that has no such file never will. The answer is final: "the channel's current release carries no image for aarch64".
  - Any other failure is retried as today, and then is could-not-tell.
- **Generations record their architecture.**
  - An import records the manifest's architecture on the generation. `@verified` and an exact generation resolve only among generations of the node's own architecture.
  - A pull refuses to import into an image whose existing generations are another architecture's, so one site's x64 and arm64 nodes cannot share a Ceph image by naming the same one.
  - The node's image is `ubuntu-2404-arm64`, and a tier names `ubuntu-2404-arm64@verified`.
  - A generation imported before this change, which records nothing, is read as x86_64, the only architecture billet has published.

**The x64 build gates the release; the arm64 build joins it, or its last good image is carried forward.**
- **The builders.** Both legs run in one workflow run, each on a builder of its own architecture: `billet-image-builder-ubuntu-2404`, and `billet-image-builder-ubuntu-2404-arm64`, an EC2 tier on the reference deployment.
- **x64 fails.** Nothing is published.
- **arm64 fails.** The run is red, and the release carries the previous release's aarch64 set unchanged: its signed manifest, its bundle, its parts and its corresponding source, each held to the manifest's digests before upload. An arm64 node or a new one still finds the last good image through the one channel. The image's `built_at` says how old it is, and `billet check` reports its runner's window. When no previous release carries an aarch64 set, x64 is published alone.
- **Boot checks.** Each leg passes its own architecture's gates. Each image is proved to boot by `billet images pull --verify` on a node of its architecture before any tier resolves `@verified` to it. On arm64 that is a Firecracker host with KVM, which on AWS means a metal Graviton instance. Virtualised Graviton instances expose no `/dev/kvm`.

**The arm64 guest kernel is built for Firecracker's aarch64 boot protocol.** Firecracker loads a PE `Image` on aarch64 and an ELF `vmlinux` on x86_64 (Firecracker's `docs/rootfs-and-kernel-setup.md`). The aarch64 kernel starts from Firecracker's own aarch64 CI config and carries the same `required-builtins.txt`, plus a per-architecture file that the build and the gate both read.

## Consequences

- A workflow moved from `ubuntu-24.04` or `ubuntu-24.04-arm` to a billet tier of either backend finds what it found on GitHub, apart from the components its deployment has not accepted. `docs/operating/compatibility.md` names those.
- The AMI build runs GitHub's whole template, so it takes hours rather than the hour it took with billet's installers. Its default timeout and builder disk are set from the first real builds and recorded with them. A build still ends with a boot of the produced image before the contract tag is written.
- Bumping the template pin now moves four images at once. A pin bump's review covers four plans and the scoped difference list, and the first build of each target after a bump is its test.
- `install-toolcache.sh`, its three-state unpublished record and the AMI's toolcache gate go with ADR-005's route. GitHub's Pester suite and the verifier's check through the job's own path take their place.
- A release that carried an arm64 set forward costs a second upload of that set, about the size of the image's parts.
- Ubuntu 22.04 was considered (#24) and is not built here. Upstream carries its template and toolsets at the pinned commit, though billet vendors neither, so adding it is a new set of targets, not a new route.

## Alternatives rejected

- **Reimplement the missing software for the AMI** (#212, and #25's first brief). This is the route ADR-014 left because every gap was found by a user's workflow. It would also leave the AMI and the guest reaching parity by different code.
- **Build the AMI from the published guest image.** It would need a bootloader, an EC2 kernel and cloud-init grafted onto a filesystem built to boot without them, and the left-out components would still need completing. Running the template on the builder gives the AMI GitHub's own result for a VM, which is what an EC2 instance is.
- **A matrix over `arch` that changes only `runs-on`.** The template, toolset, base filesystem, kernel, runner tarball and left-out components all differ between the architectures. A guest built from the x64 plan on an arm64 machine passes no gate written for either.
- **A second channel pointer for arm64, or a new manifest schema.** A second pointer would keep older x64 readers unchanged too. But it is a second signed, expiring document for the publisher to advance and the reader to check, and carrying the last good aarch64 set forward gives an arm64 node the same continuity through the one pointer. A new schema is a flag day for every deployed reader.
