# ADR-021: A Tart tier boots an image billet verified, never one billet published

Accepted, 2026-10-25. Issue #24.

## Context

A Firecracker tier boots an image billet builds, gates, signs and publishes, and `@verified` resolves to the newest generation a node proved to boot (`billet-guest-images`). A Tart tier boots whatever OCI reference it names. The documented tiers name third-party images, a macOS image with Xcode and an arm64 Ubuntu image with the runner and Docker ([Run jobs on a Mac](../../deploying/mac-tart.md)).

For such a reference, billet does three things:
- `billet images pull` runs `tart pull`.
- The daily refresh pulls an image only when it is absent, and never pulls a tag again that has moved (`internal/ops/images/imagesrefresh.go`).
- A launch pins the tag to the digest it resolves to in the local store (`tart fqn`) before it clones.

Nothing boots the image before a job does. Nothing checks that its runner is inside GitHub's thirty days. Nothing records which digest a deployment decided to trust.

Billet could publish Tart images of its own, as it does for Firecracker, or verify the images a deployment already uses. The two questions behind that choice are what billet may redistribute and what it costs to build.

**macOS.** The macOS Tahoe 26 software license agreement (read from Apple's published PDF on 2026-10-25) allows, in section 2B(iii), "up to two (2) additional copies or instances of the Apple Software ... within virtual operating system environments on each Apple-branded computer you own or control that is already running the Apple Software", for software development, testing during software development, macOS Server or personal use. Among its restrictions on use it says: "Except as expressly permitted in Section 3, you may not rent, lease, lend, sell, redistribute or sublicense the Apple Software." The Xcode and Apple SDKs Agreement (read the same day) says use of the Apple Software "is authorized only for execution on an Apple-branded product running macOS", and that the licensee agrees "not to rent, lease, lend, upload to or host on any website or server, sell, redistribute, or sublicense the Apple Software and Apple Services, in whole or in part, or to enable others to do so". A macOS image with Xcode, published as a release asset of this repository, would be billet redistributing both. The reasoning #584 applied to the Android SDK ([ADR-005](adr-005-runner-image-parity.md)) applies here: what an operator may run on their own Mac, billet may not hand them.

**Linux.** Nothing in the licences stops billet publishing an arm64 Linux image, and ADR-022 publishes one for Firecracker. A Tart image is a different artifact, though.
- Tart's Linux VMs boot through EFI. Virtualization.framework also has a direct kernel boot loader, but tart does not use it. A Tart image therefore needs a partitioned disk, a bootloader and a kernel with the virtio drivers Virtualization.framework presents. billet's Firecracker guest is a bare ext4 filesystem with a microVM kernel beside it.
- A Tart guest takes its registration over `tart exec`, which needs tart's guest agent in the image.
- A Tart image is published by pushing to an OCI registry in tart's own layout, which `tart push` does only on macOS.

That makes it a second image pipeline, with a Mac in its publish path, for one tier shape that runs only on Apple hardware.

## Decision

**billet publishes no macOS image and no Tart Linux image.** A Tart tier names an image by its registry reference: a third party's, or one the operator built and pushed to a registry of their own. Whether a third party may distribute the image is between that party and the software's licensors. Whether the operator may run it on their Mac is the operator's own acceptance of the macOS licence and, for Xcode, the Xcode agreement. billet hosts, mirrors and bundles none of it, and its documentation names example images without vouching for their licensing.

**A Tart tier may name `<reference>@verified`, and then boots only an image billet booted and recorded.**
- **Spelling.** A reference ending in `@verified`, for example `registry.example/runner-images/ubuntu-arm64:latest@verified`, is the spelling a Firecracker tier already uses. Validation refuses `@verified` combined with an `@sha256:` digest, and on a local VM name, which has no digest to record. The reference before `@verified` is normalised the way tart resolves it (registry, repository, and the default tag `latest`) before it is used as a key.
- **What `billet images verify` does on a Tart node.** It resolves the pulled reference to `<repository>@sha256:<digest>` and clones exactly that, using the launch path's ownership order: a staging name, the marker, the store lock and the rename. The probe gets the network flags of the trust class it is verified for.
- **What the probe checks.** Through `tart exec` it asks the guest what a job depends on. On both operating systems, the runner answers `--version`. On Linux, Docker with Buildx and Compose answers. On macOS, `xcodebuild -version` answers.
- **Recording.** The probe is destroyed and proved stopped before anything is recorded. The record holds the digest, the runner version and the time, keyed by the normalised reference, in a file under `node.state_dir` written through `durablefile`.
- **A macOS probe needs a stopped node.** Apple's grant allows two macOS guests per Mac, and the control plane counts macOS guests per node only through the leases it places (`macos_vm_limit`). A probe holds no lease, so a probe started while the node runs could be the third macOS VM a job's launch then hits. `billet images verify` therefore refuses a macOS reference unless the node's own ending record proves the node stopped (`internal/nodeending`). A Linux probe runs beside a running node, as the Firecracker probe does, and its CPU and memory are not reserved: a Linux verification may briefly overcommit the Mac.
- **Two questions, answered at different times.** Whether the image boots is decided once, by the verification, and never changes for a digest. Whether its runner is inside GitHub's window changes without the digest changing.
  - The verification refuses a runner `internal/runnerrelease` proves expired. An answer of "could not tell" is recorded as unknown rather than as inside the window.
  - The daily refresh and `billet check` evaluate every recorded digest again, mark the ones proved expired, and report the unknown ones as unknown.
- **What a launch resolves `@verified` to.** The newest recorded digest of that reference. A launch refuses, naming the command that would fix it, if that digest is marked expired or is no longer in tart's store. tart prunes its own cache when an operation needs room. The launch never falls back to an older record, whose runner is older still.
- **Records under concurrency.** One lock file beside the record serialises verify, unpromote and refresh, and a launch reads the record under it. A verification takes a generation number from the record when it starts. It commits only if no unpromote of that reference ran in between, so an unpromote is never undone by a probe that was already in flight. A launch that has resolved its digest before an unpromote proceeds, as a Firecracker launch does after `images unpromote`.
- **Unpromote.** `images unpromote <reference>@sha256:<digest>` removes a record.

**Only `@verified` references move automatically.** The refresh pulls a moving tag again only when every tier on the node that names the reference uses `@verified`. It then verifies a new Linux digest. For a new macOS digest, it reports that one is waiting for a verification on a stopped node. A tag also named by an unverified tier is left as it is today, pulled only when absent and pinned at each launch, so an existing deployment's tiers keep booting what they booted. This reverses today's rule only for references that opted in, and it costs a second copy of the image in tart's store while the old digest is still recorded.

**An older node is never sent a reference it cannot read.** The tier's image reaches a node as a plain string, and a node from before this change would hand `…@verified` to `tart fqn`. The control plane sends a `@verified` Tart tier only to a node whose wire version reads it, gated as `billet-add-wire-change` describes. A node that holds no usable record for a tier's reference refuses that tier's launch with the command that would verify one, as a Firecracker node with no verified generation does.

## Consequences

- What billet asserts about a Tart image is what it can prove from inside a booted guest: the runner, its window, and the toolchain entry points a job uses. It does not assert parity with GitHub's own macOS or arm64 Linux image. GitHub publishes the template for its arm64 Linux image (ADR-022), but billet has no route that turns it into a Tart image, and none is measured.
- Verifying a macOS image is a maintenance window: drain and stop the node, verify, start it again. Xcode images change when an operator chooses a new Xcode, so the window is rare, and `@verified` keeps resolving to the previous digest until it happens.
- Records are per node. Two Macs that name the same reference each verify it, as two Firecracker nodes at different sites each verify their own generation.
- A reference without `@verified` keeps working, and `billet check` names every Tart tier that boots an image nobody verified.
- status.md's Tart cells move when a deployment's Tart tiers boot `@verified` images and the conformance workflow's arm64 run passes on one, not when this lands.

## Alternatives rejected

- **Publish a macOS image.** The macOS and Xcode licences forbid billet redistributing either.
- **Publish a Tart Linux image built from billet's arm64 guest.** It needs an EFI disk, a second kernel, tart's guest agent, a registry push from a Mac, and a completion route for what the image leaves out, all for one tier shape. A verified third-party or operator-built image gives the same guarantee, a boot billet observed, at a fraction of the cost. It can be reopened if a deployment needs GitHub's arm64 image contents on a Mac rather than in an arm64 Firecracker microVM (ADR-022).
- **Ship tooling that builds a macOS image on the operator's own Mac.** This keeps the image out of billet's distribution, but the build needs the operator's Apple account to download Xcode, takes hours, and would be a second image pipeline billet maintains for Apple's formats. An operator who builds their own image pushes it to a registry they control and verifies it like any other.
- **Pin a digest in the tier instead of `@verified`.** `<repository>@sha256:…` already works and stays allowed. But a digest an operator copied proves nothing was booted, and a fleet that pins by hand stops taking up runner releases until someone edits every tier. That is the thirty-day outage `@verified` exists to prevent on Firecracker.
