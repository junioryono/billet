# ADR-022: Every Linux image is GitHub's template build, on both architectures, and one release carries both

Accepted, 2026-10-25. Issues #24 and #25. Extends [ADR-014](adr-014-guest-image-from-githubs-template.md) from the x64 Firecracker guest to the arm64 guest and to the EC2 AMI on both architectures, and supersedes what remained of [ADR-005](adr-005-runner-image-parity.md), which governed the AMI.

## Context

ADR-014 made the x64 Firecracker guest GitHub's own build. It vendored `actions/runner-images` at one commit and ran its template under `systemd-nspawn`, with every difference in one reviewed file. Two images were left on other routes.

**The EC2 AMI** still builds from GitHub's declaration with billet's own installers (`internal/runnerimages/install-toolcache.sh`), so it lacks everything the declaration does not name: Rust, Swift, Haskell, Kotlin, Julia, Miniconda, vcpkg, Homebrew, nvm, the Azure and Google Cloud CLIs, and the browsers with their drivers (#25). #212 reimplemented nine of those for the shared installer. It was closed as superseded by ADR-014, which covered only the guest. A workflow that calls `cargo` with no setup step passes on GitHub and on a Firecracker tier and fails on an EC2 tier.

**arm64** had no route at all. At the commit billet pins (`e75633902841`, the `ubuntu24/20260920.314` image release), the vendored tree carries it (read from the tree, 2026-10-25):
- `images/ubuntu/templates/build.ubuntu-24_04-arm64.pkr.hcl` reads `toolsets/toolset-2404-arm64.json`, and every installer branches on `is_arm64`.
- The arm64 template runs the x64 template's steps except nine: Bicep, the CodeQL bundle, Microsoft Edge, Google Chrome, Haskell, Miniconda, Julia, the Android SDK and PyPy.
- GitHub's tests skip the browsers, Chromium, Android, Haskell and Bicep on arm64 (`Test-IsArm64`).

So "parity on arm64" has a definition GitHub wrote: its own arm64 image, which carries Rust and Swift and has no Haskell.

**One release, one manifest.** A guest release holds one `manifest.json`, and the signed channel pointer names one release. `Manifest.Usable` already refuses an image built for another architecture, so a reader is ready for a second architecture as long as it can find it. Every deployed reader fetches `manifest.json` by that name and refuses a schema it does not know, so any change to the manifest's schema or to the channel's would be a flag day (`billet-guest-images`).

## Decision

**Every Linux image billet builds runs GitHub's template for its architecture.** The x64 and arm64 Firecracker guests run under nspawn on Ubuntu's cloud root filesystem for that architecture, as ADR-014 describes. The x64 and arm64 AMIs run the same template directly on the EC2 builder, an Ubuntu 24.04 virtual machine, which is the setting GitHub's own Packer build runs it in on Azure.
- **One runner, scoped differences.** `scripts/run-runner-images.sh` runs a plan derived from each template, against the toolset that template reads. `differences.tsv` gains a column naming where each difference applies: `guest-x64`, `guest-arm64`, `ami-x64`, `ami-arm64`, or every target.
  - The guest's own differences apply only to the guest: GRUB's absence, the virtio-disk and kernel-command-line tests, snapd. The AMI boots through GRUB on NVMe and runs snapd.
  - The Azure agent's differences and Docker's classic store apply everywhere.
  - An entry naming a step that one of its targets' plans lacks stops the build before anything runs, as it does today.
- **The AMI's reboot.** The template reboots once, mid-build. On the EC2 builder the runner is a oneshot systemd unit that records the next step before it reboots and continues from it at the next boot. Its verdict reaches billet through the console, as the verifier's already does, so a failed build is reported when it fails rather than when `--timeout` expires.
- **The tree reaches the builder in the S3 payload** that already carries the AMI's installers. `install-toolcache.sh` and the AMI's declaration-driven gates are retired, because nothing builds with them any more.

**What a published image leaves out, an AMI installs only when the deployment accepts it.** The guest leaves out what billet may not redistribute (#584). For x64 that is the Android SDK, CodeQL, Chrome, Edge and Firefox. For arm64 it is Firefox alone, since GitHub's arm64 template installs none of the other four.
- An AMI is not redistributed: `billet ami build` makes it in the operator's own account. Its build still installs a component only when `images.accept` names it, by running the same completer the guest carries. Writing the name is the deployment accepting that licence, on both backends. The AMI build used to accept the Android SDK licence on everyone's behalf; it no longer does.
- The AMI records which components it carries and at which versions in a tag beside its contract tag. `AMIContract` becomes 4.

**One release carries both architectures, inside the schemas every deployed reader already reads.**
- `manifest.json` stays the x86_64 image, byte for byte what an older billet expects.
- The aarch64 image is described by `manifest-aarch64.json` and signed by `manifest-aarch64.sigstore.json` in the same dated release. Its assets carry the architecture in their names: `rootfs-aarch64.img.zst.partNNN`, `vmlinux-billet-aarch64`, `image-sources-aarch64.*`.
- The channel pointer is unchanged and still names one release. Neither schema changes, so this is not a flag day. An older billet on arm64 reads `manifest.json` and is refused by `Usable`, as it is today.
- A billet with this change reads the manifest named for its architecture. When the release has no such file, the answer is "the channel carries no image for aarch64". Any other failure is could-not-tell.
- The node's Ceph image is `ubuntu-2404-arm64`, so a tier names `ubuntu-2404-arm64@verified`.

**The x64 build gates the release; the arm64 build joins it.** Both build legs run in one workflow run, each on a builder of its own architecture: `billet-image-builder-ubuntu-2404` and `billet-image-builder-ubuntu-2404-arm64`, the latter an EC2 tier on the reference deployment. If x64 fails, nothing is published. If arm64 fails, x64 is published alone and the run is red, because a fleet on x64 must not lose its weekly runner update to the other architecture. Each leg passes its architecture's gates. Each image is proved to boot by `billet images pull --verify` on a node of its architecture before any tier resolves `@verified` to it, as for x64.

**The arm64 guest kernel is built for Firecracker's aarch64 boot protocol.** Firecracker loads a PE `Image` on aarch64 and an ELF `vmlinux` on x86_64 (Firecracker's `docs/rootfs-and-kernel-setup.md`). The aarch64 kernel starts from Firecracker's own aarch64 CI config and carries the same `required-builtins.txt`, plus a per-architecture file the build and the gate both read.

## Consequences

- A workflow moved from `ubuntu-24.04` or `ubuntu-24.04-arm` to a billet tier of either backend finds what it found on GitHub, apart from the five components a deployment has not accepted. `docs/operating/compatibility.md` names those.
- The AMI build runs GitHub's whole template, so it takes hours rather than the hour it took with billet's installers. Its default timeout and builder disk are set from the first real builds and recorded with them. A build still ends in a boot of the produced image before the contract tag is written.
- Bumping the template pin now moves four images at once. A pin bump's review covers four plans and the scoped difference list, and the first build of each target after a bump is its test.
- `install-toolcache.sh`, its three-state unpublished record and the AMI's toolcache gate are gone with ADR-005's route. GitHub's own Pester suite, which the template runs during the build, is the parity gate on every target.
- Ubuntu 22.04 was considered (#24) and is not built here. Its template and toolsets are in the same tree, so adding it is a new set of targets, not a new route.

## Alternatives rejected

- **Reimplement the missing software for the AMI** (#212, and #25's first brief). This is the route ADR-014 left because every gap was found by a user's workflow. It would also leave the AMI and the guest reaching parity by different code.
- **Build the AMI from the published guest image.** It would need a bootloader, an EC2 kernel and cloud-init grafted onto a filesystem built to boot without them, and the five components would still need completing. Running the template on the builder gives the AMI GitHub's own result for a VM, which is what an EC2 instance is.
- **A matrix over `arch` that changes only `runs-on`.** The template, toolset, base filesystem, kernel, runner tarball and left-out components all differ between the architectures, and a guest built from the x64 plan on an arm64 machine passes no gate written for either.
- **A second channel, or a new manifest schema, for arm64.** Either one is a flag day for every deployed reader, or a second pointer to keep in step with the first. A second manifest beside the first changes nothing an older reader reads.
