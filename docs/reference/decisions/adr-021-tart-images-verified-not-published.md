# ADR-021: A Tart tier boots an image billet verified, never one billet published

Accepted, 2026-10-25. Issue #24.

## Context

A Firecracker tier boots an image billet builds, gates, signs and publishes, and `@verified` resolves to the newest generation a node proved to boot (`billet-guest-images`). A Tart tier boots whatever reference it names. The documented tiers name third-party images, a macOS image with Xcode and an arm64 Ubuntu image with the runner and Docker ([Run jobs on a Mac](../../deploying/mac-tart.md)).

For a remote reference, billet does three things:
- `billet images pull` runs `tart pull`.
- The daily refresh pulls an image only when it is absent and never pulls a tag again that has moved (`internal/ops/images/imagesrefresh.go`).
- A launch pins the tag to the digest it resolves to in the local store (`tart fqn`) before it clones.

Nothing boots the image before a job does. Nothing checks that its runner is inside GitHub's thirty days. Nothing records which image a deployment decided to trust.

Three properties of tart 2.37.0, read from its source on 2026-10-25, decide how such a record can be kept:
- **A clone may fetch.** `tart clone` of a remote reference pulls it when its cache entry is missing. `Registry` does not compare the manifest it receives with a digest it asked for. So a clone of `<repository>@sha256:<d>` is not, by itself, a guarantee of those bytes.
- **Moving a tag can delete.** Re-pointing a tag runs the OCI cache's garbage collection. That removes a digest no tag names any more, unless it was pulled by digest.
- **One store per user.** The OCI cache and the VMs live in one store per user (`TART_HOME`), which every deployment running as that user on the Mac shares. When an operation needs room, tart prunes the OCI and IPSW caches by itself, unless `TART_NO_AUTO_PRUNE` is set. It never prunes a local VM by itself.

Billet could publish Tart images of its own, as it does for Firecracker, or verify the images a deployment already uses. The two questions behind that choice are what billet may redistribute and what it costs to build.

**macOS.** The macOS Tahoe 26 software license agreement (read from Apple's published PDF on 2026-10-25) allows, in section 2B(iii), "up to two (2) additional copies or instances of the Apple Software ... within virtual operating system environments on each Apple-branded computer you own or control that is already running the Apple Software", for software development, testing during software development, macOS Server or personal use. Among its restrictions on use it says: "Except as expressly permitted in Section 3, you may not rent, lease, lend, sell, redistribute or sublicense the Apple Software." The Xcode and Apple SDKs Agreement (read the same day) says use of the Apple Software "is authorized only for execution on an Apple-branded product running macOS". It also says the licensee agrees "not to rent, lease, lend, upload to or host on any website or server, sell, redistribute, or sublicense the Apple Software and Apple Services, in whole or in part, or to enable others to do so". A macOS image with Xcode, published as a release asset of this repository, would be billet redistributing both. The reasoning #584 applied to the Android SDK ([ADR-005](adr-005-runner-image-parity.md)) applies here: what an operator may run on their own Mac, billet may not hand them.

**Linux.** Nothing in the licences stops billet publishing an arm64 Linux image, and ADR-022 publishes one for Firecracker. A Tart image is a different artifact, though.
- Tart's Linux VMs boot through EFI. Virtualization.framework also has a direct kernel boot loader, but tart does not use it. So a Tart image needs a partitioned disk, a bootloader, and a kernel with the virtio drivers Virtualization.framework presents. billet's Firecracker guest is a bare ext4 filesystem with a microVM kernel beside it.
- A Tart guest takes its registration over `tart exec`, which needs tart's guest agent in the image.
- A Tart image is published by pushing to an OCI registry in tart's own layout, which `tart push` does only on macOS.

That makes it a second image pipeline, with a Mac in its publish path, for one tier shape that runs only on Apple hardware.

## Decision

**billet publishes no macOS image and no Tart Linux image.** A Tart tier names a third party's registry reference, or a VM the operator built on the Mac itself.
- Whether a third party may distribute an image is between that party and the software's licensors.
- Whether the operator may run it, or host an image of their own on a registry, is their own position under the macOS licence and, for Xcode, the Xcode agreement, whose restriction on hosting is not limited to public servers.
- billet hosts, mirrors and bundles none of it. Its documentation names example images without vouching for their licensing.

**A Tart tier may name `<image>@verified`, and then boots only a copy of what billet booted.**
- **Spelling.** `<image>` is a remote reference or a local VM name. The spelling is the one a Firecracker tier already uses. Validation refuses `@verified` combined with an `@sha256:` digest. A remote reference is normalised the way tart resolves it (registry, repository, and the default tag `latest`) before it is used as a key.
- **Pulling without moving tags or pruning.** For a remote reference, `billet images verify` and the refresh ask the registry for the tag's current digest (the OCI distribution API, anonymously where the registry allows it). They then pull `<repository>@sha256:<d>`. Every tart command a verification runs, and every clone of a base, runs with `TART_NO_AUTO_PRUNE` set: tart prunes after a clone as well as before a pull. A pull by digest moves no tag, so tag-move garbage collection removes nothing, and with pruning off nothing billet runs for `@verified` evicts another cache entry, which another deployment's unverified tier may be using. A pull that does not fit the disk fails and is reported.
- **The verified copy is a base VM.** The verification clones the pulled digest, or the operator's local VM, into a local VM billet owns: `billetbase-<deployment>-<image>-<digest>`, the image and digest each as a short hash. The name is outside the `billet-` prefix that `provider.LeaseOf` reads as a lease, so no node's `List`, orphan sweep or inventory treats it as compute, and the deployment in the name keeps two deployments' bases apart. The clone uses the launch path's ownership order: a staging name, the deployment's marker (of a kind `List` never inventories), the store lock and the rename. tart's automatic pruning reclaims only the OCI and IPSW caches, never a local VM, so the bytes stay on the Mac whatever happens to the cache or the tag, and on APFS a clone shares blocks with its source until either is written. A base removed by hand (`tart delete`, or `tart prune --entries vms`) is gone, and the launch refuses as below.
- **One verification of an image at a time.** A verification holds a lock for that image, in this deployment, for its whole life. Under it, before anything else, it removes what an earlier verification of this deployment left behind: a probe, a base staging clone, and a finished base of this image that no record names (a verification that failed, yielded or died after the base was made). Each base is removed under its own lock.
- **What is probed is the base.** The probe is a clone of the base VM, named `billetprobe-<deployment>-<hash>` for the same reason as the base, booted with the network flags of the trust class it is verified for. Through `tart exec` it asks the guest what a job depends on:
  - on both operating systems, the runner answers `--version`;
  - on Linux, Docker with Buildx and Compose answer;
  - on macOS, `xcodebuild -version` answers.

  The probe is destroyed and proved stopped before anything is recorded. Because every launch clones the same base, what a job boots is a copy of what was probed, whatever tart's registry client did or did not check.
- **Records.** A record holds the image key, the base VM, the digest (remote references only), the runner version, the time, and the runner's deadline. It lives in a file under `node.state_dir`, written through `durablefile`.
- **How long bases are kept.** The two newest bases of each image are kept. Older ones are deleted when a newer one is recorded, and an unpromote deletes its base. Each base has a lock of its own. A launch holds it shared from resolving the base until its clone is complete, and a delete takes it exclusively, so a base is never deleted under a clone that has resolved it.
- **Launch.** `@verified` resolves to the newest record of that image, and the launch clones its base VM: a local clone, with no network and no fallback. A launch refuses, naming the command that would fix it, when the base is gone or the runner's deadline has passed.
- **A probe holds a macOS slot, and yields it to a job.** Apple's grant allows two macOS guests per Mac. The control plane counts macOS guests per node only through the leases it places (`macos_vm_limit`), and a probe holds no lease.
  - A macOS probe takes an exclusive lock in the store (`$TART_HOME/.billet/macos.lock`) for its whole life. It starts only if at most one macOS VM is running in the store, from any deployment. `tart list` reports which VMs run but not their operating system, so each running VM is asked with `tart get --format json`. A VM whose answer cannot be read counts as macOS.
  - Every billet macOS launch takes the same lock, shared, from before `tart run` until the guest is proved running. If a probe holds it, the launch writes a yield request beside the lock, naming itself and the time, and waits at most two minutes. It removes its request on every way out, and a probe ignores a request older than that wait.
  - A probe checks for a yield request every few seconds. When it sees one, it stops its guest, proved stopped within tart's thirty-second stop window, deletes it and releases the lock. The verification ends as "yielded to a job", and the next refresh or the operator runs it again. So a launch waits about a minute, well inside the node's ten-minute command timeout, and the node's queue is not held behind a whole probe.
  - A probe whose verifier died leaves its guest running and its lock released. A macOS launch that takes the lock first destroys a `billetprobe-` guest of its own deployment, proved stopped, before it starts its own, but only if that guest is macOS and an exclusive try of its image's verification lock succeeds, which proves no verifier still owns it. A Linux probe is never touched by a launch. A surviving probe of another deployment still counts against Apple's limit, which billet cannot release on that deployment's behalf.
  - On a Mac whose macOS slots are always busy, a probe may yield every time. `billet check` reports an image whose verification keeps yielding beside its runner's deadline. The path that always finishes is to drain the node (`billet drain`) and run `billet images verify --no-yield`, which an operator asks for knowing that no job will be placed meanwhile.
  - The lock and the count cover one store, which is how billet installs a Mac node: one service account and one `TART_HOME`. A VM another account starts on the same Mac is invisible to both, as it already is to `macos_vm_limit`. So is a deployment on the same store running a billet from before this change.
  - A Linux probe takes no slot. Its CPU and memory are not reserved, as the Firecracker probe's are not, so a Linux verification may briefly overcommit the Mac.
- **Two questions, answered at different times.** Whether the image boots is decided once, by the verification, and never changes for a base. Whether its runner is inside GitHub's window changes without the image changing.
  - The verification refuses a runner `internal/runnerrelease` proves expired.
  - Once a newer runner release exists, the window has a date, and the record stores that deadline. An answer of "could not tell" never erases a deadline already stored.
  - The launch compares the stored deadline with the clock, so it needs neither the network nor a refresh.
  - The daily refresh and `billet check` ask again for every record whatever `release.automatic` says, and report a runner whose window is unknown as unknown.
- **Records under concurrency.** One lock file beside the record serialises the record's reads and commits, and is never held across a probe. A verification takes the record's generation number when it starts, and commits only if no unpromote of that image ran in between. An unpromote is never undone by a probe already in flight. A launch that has resolved its base before an unpromote still completes its clone, under the base's lock.
- **Unpromote.** `images unpromote <image>@<base>` removes a record and its base.

**The refresh verifies new images only for `@verified` references.**
- For a remote `@verified` reference whose tag now names a new digest, the refresh pulls that digest and verifies it, macOS included, under the slot lock.
- A reference no tier names with `@verified` is left exactly as today: pulled only when absent, pinned at each launch.
- A local VM is verified again only when an operator asks, since nothing tells billet it changed.

**An older node is never sent a reference it cannot read.** The tier's image reaches a node as a plain string, and a node from before this change would hand `…@verified` to `tart fqn`. So the control plane sends a `@verified` Tart tier only to a node whose wire version reads it, gated as `billet-add-wire-change` describes. A node with no usable record for a tier's image refuses that tier's launch with the command that would verify one, as a Firecracker node with no verified generation does.

## Consequences

- What billet asserts about a Tart image is what it can prove from inside a booted guest: the runner, its window, and the toolchain entry points a job uses. It does not assert parity with GitHub's own macOS or arm64 Linux image. GitHub publishes the template for its arm64 Linux image (ADR-022), but billet has no route that turns it into a Tart image, and none is measured.
- A verified image costs a base VM on the Mac's disk. It shares blocks with the cache while both exist, and keeps them when the cache is pruned. Two bases are kept per image. Because billet's clones of a base run without automatic pruning, a `@verified` launch on a full disk fails rather than evicting cache entries as an unverified launch's clone may. `billet check` reports free space against the newest base's size.
- A macOS launch can wait about a minute for a probe to yield, and a macOS verification can be cut short by a job and run again. A probe refuses to start beside two running macOS guests.
- Records and bases are per node. Two Macs that name the same image each verify it, as two Firecracker nodes at different sites each verify their own generation.
- A reference without `@verified` keeps working, and `billet check` names every Tart tier that boots an image nobody verified.
- status.md's Tart cells move when a deployment's Tart tiers boot `@verified` images and the conformance workflow's arm64 run passes on one, not when this lands.

## Alternatives rejected

- **Publish a macOS image.** The macOS and Xcode licences forbid billet redistributing either.
- **Publish a Tart Linux image built from billet's arm64 guest.** It needs an EFI disk, a second kernel, tart's guest agent, a registry push from a Mac, and a completion route for what the image leaves out, all for one tier shape. A verified third-party or operator-built image gives the same guarantee, a boot billet observed, at a fraction of the cost. It can be reopened if a deployment needs GitHub's arm64 image contents on a Mac rather than in an arm64 Firecracker microVM (ADR-022).
- **Record the digest and clone the remote reference at each launch.** That is what the first draft of this record said. A clone of a missing cache entry fetches whatever the registry serves, and a tag move can delete the verified digest. Only a local copy keeps what was probed.
- **Require the node stopped for a macOS probe.** The proof of a stop reads systemd and not launchd, it says nothing about the moment after it was taken, and a stopped node can leave guests running. A lock that launches share is an exclusion rather than an observation.
- **Ship tooling that builds a macOS image on the operator's Mac.** It needs the operator's Apple account to download Xcode, takes hours, and would be a second image pipeline billet maintains for Apple's formats. An operator who builds their own VM verifies it by name.
- **Pin a digest in the tier instead of `@verified`.** `<repository>@sha256:…` already works and stays allowed. But a digest an operator copied proves nothing was booted, and a fleet that pins by hand stops taking up runner releases until someone edits every tier. That is the thirty-day outage `@verified` exists to prevent on Firecracker.
