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
- Many build scripts pick an architecture-specific download through `is_arm64`. Some download x64 artifacts with no such branch, such as CodeQL and Miniconda, which is why the arm64 template leaves them out.
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
  - The template reboots once, mid-build. The build runs as a oneshot systemd unit that knows a build identity from its user data: a nonce billet invents per build. The nonce guards against accidental output. It is not a secret from a hostile build step, which as root can read the user data.
  - The build keeps a record of its identity, its phase (`started`, `rebooting`, `resumed`), the step it reached and the boot it ran in. cloud-init runs the user data on the first boot only, and the user data starts the unit without enabling it. The step before the reboot writes the phase `rebooting` and the next step, and only then enables the unit. The next boot continues only from `rebooting`, and records `resumed` with its own boot id. The unit stays enabled until the build's last step disables it.
  - Any other boot that finds the unit enabled is an unplanned restart, and the build reports failure and stops: a record in another phase, or a phase of `resumed` under a different boot. A crash before the reboot step leaves the unit disabled, so that boot runs nothing and billet's deadline, not a resumed build, ends it.
  - The build ends one of two ways. On success it prints a nonce-bound verdict to the console, again and again for a dwell, as the verifier does, and then powers off. On failure it prints a nonce-bound failure and halts without powering off, so billet's poll reads the failure while the machine is still there to debug.
  - Before the success verdict, the build disables and removes its own unit, its inputs and cloud-init's copies of the user data. An instance launched from the AMI therefore never runs the build.
  - billet images a stopped builder only after reading the success verdict for this build's nonce on its live console while the builder ran. A stop alone is no longer success. The template's own output goes to the journal and not to the serial console, so it cannot push the verdict out of EC2's 64 KiB console tail.
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
- **Migration.** A contract-3 AMI carries the Android SDK, and on x64 CodeQL, whatever `images.accept` says. A rebuild keeps a component only where the deployment accepts it and GitHub's template for that architecture installs it. On arm64 the Android SDK and PyPy are gone after a rebuild whatever is accepted, because GitHub's arm64 image carries neither. CodeQL was never on an arm64 AMI, since it publishes no arm64 bundle. For such an image, `billet check` names each component a rebuild drops, and says which ones need `images.accept` and which ones GitHub's arm64 image leaves out.

**One release carries both architectures, inside the schemas every deployed reader already reads.**
- **Names.**
  - `manifest.json` stays the x86_64 image, exactly what an older billet expects.
  - The aarch64 image is described by `manifest-aarch64.json` and signed by `manifest-aarch64.sigstore.json` in the same dated release.
  - Its assets carry the architecture in their names: `rootfs-aarch64.img.zst.partNNN`, `vmlinux-billet-aarch64`, `image-sources-aarch64.*`.
  - The signed channel pointer keeps schema 1 and gains one optional member, `architectures`, listing the manifests its release carries. A deployed reader decodes the pointer with `json.Unmarshal` into the members it knows, so it ignores the new one.
- **No flag day.** Neither schema number changes. An older billet on arm64 reads `manifest.json` and `Usable` refuses it, as it does today.
- **How a newer billet reads it.** It reads the manifest named for its own architecture.
  - Whether the channel's release carries an aarch64 set is read from the pointer's signed `architectures`: a list that leaves aarch64 out is final, "the channel's current release carries no image for aarch64". A pointer published before the member existed says nothing, and the reader then tries the manifest and reads a failure, a 404 included, as could-not-tell. A 404 can be temporary or cached, so it is never read as proof of absence. Nothing here asks GitHub's REST API, whose anonymous limit is shared by every node behind one address.
- **An operator's own source keeps working.** A deployment that points `images.source` or `--from` at a source of its own reads the manifest named for its architecture, and falls back to `manifest.json` when there is none. `Usable` judges whatever it read, so a mirror that publishes a single arm64 `manifest.json` keeps working, as it does today.
- **Generations record their architecture.**
  - An import records the manifest's architecture on the generation, under the image's publish lock. `@verified` and an exact generation never resolve to a generation recorded as another architecture than the node's.
  - An import is refused unless every existing generation of the image is known to be of the import's architecture, so one site's x64 and arm64 nodes cannot share a Ceph image by naming the same one, and an older billet, which resolves by contract alone, never meets a generation of another architecture in an image it uses. A new architecture starts in an image of its own.
  - A generation imported before this change records no architecture, and it may be of either: billet published only x86_64, but an operator's own source could feed an arm64 node an arm64 image. Such a generation keeps resolving exactly as it does today, on any node. Its architecture is learned from the generation itself. A pull or refresh that needs to know maps the generation's snapshot read-only, under the same host-wide lock the node's own read-only maps take. It reads the ELF header of `/usr/bin/env` with `debugfs` without mounting anything, and records `e_machine` (x86-64 or AArch64) under the publish lock. A crash's leftover mapping of an image-pool snapshot is swept at the start of the next pull or refresh, under that host-wide lock. The sweep matches only read-only mappings of a snapshot in the image pool whose name is a generation's (`ceph.ParseGeneration`), so it never unmaps an image head an import is writing, nor any of the cache's mappings. That changes nothing about the generation's verification or promotion. A generation whose binary cannot be read stays unknown and blocks the next import into its image. The refusal names the remedies: unpromote that generation and reap it, or start an image under a new name. `billet check` reports unknown generations before any import meets one.
  - A node's image for arm64 is `ubuntu-2404-arm64`, and a tier names `ubuntu-2404-arm64@verified`.
  - A pull or refresh chooses the images to fetch by architecture as well as by node, site and provider. In a file that names `arch`, it pulls only the images of tiers of its own architecture. A first import into an empty image refuses a manifest whose architecture differs from the `arch` of the tiers that name the image, so an x64 refresh at a site with arm64 tiers never seeds `ubuntu-2404-arm64` with x64 bytes.
- **Tiers are placed by architecture.** A tier may say `arch: x64` or `arch: arm64`. A tier that says nothing places as it does today, on a node of any architecture, so neither an x64 fleet nor an arm64 one fed by its own image source changes when it upgrades. A file in which any tier names an architecture must name one on every tier, so the first `arch` is an explicit migration of the rest rather than a default an existing tier silently takes, and placement by architecture never meets a tier that names none. Each node reports, when it registers, the architectures its provider can launch, from the wire version that carries it. A node whose provider runs on the host (Firecracker, Docker, Tart) reports its own. An EC2 node reports its shapes' architectures: each declared instance type carries one, written in the config or read from `DescribeInstanceTypes`. The allocator escrows only a shape of the tier's architecture. The EC2 node also refuses a launch, or a fallback shape, whose architecture differs from the tier's AMI. In a file with no `arch`, that refusal is the only check, as today, where such a launch fails at boot. `billet check` holds each EC2 tier's AMI architecture to its `arch`. A CodeBuild node reports those of its environments. The control plane places a tier that names an architecture only on nodes reporting it, beside the provider, site and wire checks it already makes when it chooses nodes. A node from before that wire version reports nothing, and is eligible only for tiers that name no architecture. A Firecracker node also refuses to launch a generation recorded as another architecture, so a tier placed wrongly fails closed.
- **Writing `arch` waits for every host.** A billet from before this change refuses a `billet.yaml` that carries a field it does not know, so an `arch` written into a file an older host reads stops that host from starting. Every host that reads the file is upgraded first, and `billet check` and `billet node config-change` refuse an `arch` while any registered node or controller predates it. A deployment's first image of a new architecture enters its files only with `arch` on every tier, so no billet from before this change, which chooses images to pull by node, site and provider alone, ever reads a file that names it, and none can seed an arm64 image with x64 bytes.

**The x64 build gates the release; the arm64 build joins it, or its last good image is carried forward.**
- **The builders.** Both legs run in one workflow run, each on a builder of its own architecture: `billet-image-builder-ubuntu-2404`, and `billet-image-builder-ubuntu-2404-arm64`, an EC2 tier on the reference deployment.
- **x64 fails.** Nothing is published.
- **arm64 fails.** The run is red, and the release carries the previous release's aarch64 set unchanged: its signed manifest, its bundle, its parts and its corresponding source. Before upload, the manifest is verified against `imagesource`'s signing policy, each image asset against the manifest's digests, and each source part against that set's own parts list and index. The aarch64 set's provenance stays the release it was first published in, whose tag points at the commit that built it. The new release's notes name that release, since the new tag points at the x64 build's commit. An arm64 node, or a new one, still finds the last good image through the one channel. The image's `built_at` says how old it is, and `billet check` reports its runner's window. When no previous release carries an aarch64 set, x64 is published alone.
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
- **A second channel pointer for arm64, or a new manifest schema.** A second pointer would keep older x64 readers unchanged too. But it is a second signed, expiring document for the publisher to advance and the reader to check. An optional member of the one pointer says which architectures its release carries, and carrying the last good aarch64 set forward gives an arm64 node continuity through it. A new schema is a flag day for every deployed reader.
