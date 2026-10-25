# ADR-021: A Tart tier boots an image billet verified, never one billet published

Accepted, 2026-10-25. Issue #24.

## Context

A Firecracker tier boots an image billet builds, gates, signs and publishes, and `@verified` resolves to the newest generation a node proved to boot (`billet-guest-images`). A Tart tier boots whatever OCI reference it names. The documented tiers name `ghcr.io/cirruslabs/macos-tahoe-xcode:latest` and `ghcr.io/cirruslabs/ubuntu-runner-arm64:latest` ([Run jobs on a Mac](../../deploying/mac-tart.md)). For such a reference billet does two things: `billet images pull` runs `tart pull`, and a launch pins a moving tag to the digest it resolves to in the local store (`tart fqn`) before it clones. Nothing boots the image before a job does, nothing checks that its runner is inside GitHub's thirty days, and nothing records which digest a fleet decided to trust. A tag that moves under a pull, and the next refresh takes up, is a new image no one looked at.

Two routes would close that. billet could publish Tart images of its own, as it does for Firecracker. Or it could verify a third-party image and pin what it verified. For macOS, the licences decide between them. For Linux, the size of the work decides.

**macOS.** The macOS Tahoe 26 software license agreement (read from Apple's published PDF on 2026-10-25) allows, in section 2B(iii), "up to two (2) additional copies or instances of the Apple Software ... within virtual operating system environments on each Apple-branded computer you own or control that is already running the Apple Software", for software development, testing during software development, macOS Server or personal use. Among its restrictions on use it says: "Except as expressly permitted in Section 3, you may not rent, lease, lend, sell, redistribute or sublicense the Apple Software." The Xcode and Apple SDKs Agreement (read the same day) says use of the Apple Software "is authorized only for execution on an Apple-branded product running macOS", and that the licensee agrees "not to rent, lease, lend, upload to or host on any website or server, sell, redistribute, or sublicense the Apple Software and Apple Services, in whole or in part, or to enable others to do so". A macOS image with Xcode, published as a release asset of this repository, would be billet redistributing both. The reasoning #584 applied to the Android SDK ([ADR-005](adr-005-runner-image-parity.md)) applies here too: what an operator may run on their own Mac, billet may not hand them.

**Linux.** Nothing in the licences stops billet publishing an arm64 Linux image; the guest image already does for Firecracker. A Tart VM is a different artifact, though. Apple's Virtualization.framework boots a Linux guest through EFI, so the image needs a partitioned disk, a bootloader and a kernel with the virtio drivers Virtualization.framework presents. billet's Firecracker guest is a bare ext4 filesystem with a microVM kernel beside it. A Tart guest takes its registration over `tart exec`, which needs tart's guest agent in the image. And a Tart image is published by pushing to an OCI registry in tart's own layout, which `tart push` does only on macOS. That would make it a second image pipeline with a Mac in its publish path, built for one tier shape that runs on Apple hardware alone.

## Decision

**billet publishes no macOS image and no Tart Linux image.** A Tart tier names a third-party image by its registry reference.

**billet verifies the image a Tart tier names, and the tier boots only what was verified.**
- A Tart tier's image may end in `@verified` (`ghcr.io/cirruslabs/ubuntu-runner-arm64:latest@verified`), the spelling a Firecracker tier already uses.
- `billet images verify` on a Tart node resolves the pulled reference to its digest, then clones and boots a probe VM. The probe uses the launch path's ownership order: a staging name, the marker, the store lock and the rename. It also uses the trust class's network flags, and counts against `macos_vm_limit`.
- Through `tart exec`, the probe asks the guest what a job depends on:
  - the runner answers `--version`, and its version is inside GitHub's window by `internal/runnerrelease`'s rule, the one `images pull` applies;
  - on Linux, Docker with Buildx and Compose answer;
  - on macOS, `xcodebuild -version` answers.
- The probe is destroyed and proved stopped. Only then is the digest recorded, in a file under `node.state_dir` written through `durablefile`, with the reference, the runner version and the time.
- At launch, `@verified` resolves to the newest recorded digest of that reference that tart's store still holds. With none, the launch refuses and names the command that would verify one.
- `images unpromote` removes a digest. `images refresh` pulls a moving tag again and verifies whatever new digest it brings.

**A reference without `@verified` keeps today's behaviour.** The pulled tag is pinned to its digest at each launch, unverified. That way an existing deployment's config does not stop working on upgrade, and `billet check` names every such tier as one that boots an image nobody verified.

## Consequences

- What billet asserts about a Tart image is what it can prove from inside a booted guest: the runner, its window, and the job's toolchain entry points. It does not assert parity with GitHub's own macOS or arm64 Linux image, which neither billet nor anyone else can build from GitHub's template on Apple hardware today.
- A verification boots a guest, and a macOS guest counts against Apple's two-instance grant. On the reference Mac, which runs two macOS guests at its limit, a verification waits for one to end. That wait is part of the cost of the licence, not something billet works around.
- The recorded digest is per node. Two Macs that name the same reference each verify it, as two Firecracker nodes at different sites each verify their own generation.
- status.md's Tart cells move when a fleet's Tart tiers boot `@verified` images and the conformance workflow's arm64 run passes on one, not when this lands.

## Alternatives rejected

- **Publish a macOS image.** The macOS and Xcode licences forbid billet redistributing either.
- **Publish a Tart Linux image built from billet's arm64 guest.** It needs an EFI disk, a second kernel, tart's guest agent, a registry push from a Mac, and a completion route for the components the image would leave out, all for one tier shape. Verifying a third-party image gives the same guarantee, a boot billet observed, at a fraction of the cost. This can be reopened if a deployment needs GitHub's arm64 image contents on a Mac rather than in an arm64 Firecracker microVM (ADR-022).
- **Pin a digest in the tier instead of `@verified`.** `ghcr.io/...@sha256:...` already works and stays allowed. But a digest an operator copied proves nothing was booted, and a fleet that pins by hand stops taking up runner releases until someone edits every tier. That is the thirty-day outage `@verified` exists to prevent on Firecracker.
