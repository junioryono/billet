# Guest images

A Firecracker job boots a **golden image**: an Ubuntu 24.04 rootfs carrying Docker, the GitHub Actions runner, and a small agent that reads the runner registration out of the metadata service and starts the runner with it. It lives in Ceph as an RBD image with immutable named snapshots called **generations**, and every job gets a copy-on-write clone of one, grown to the tier's `disk` before boot and discarded with the guest. billet builds the image once, centrally, and a deployment pulls it.

## What is in it

GitHub does not publish the image its hosted runners boot; it publishes the recipe. billet vendors GitHub's own `toolset-2404.json`, pinned by digest, and builds from it, so a workflow that assumes something a hosted runner has (`openssh-client`, a particular Python minor, five JDKs, the node/go/Python/PyPy/Ruby/CodeQL toolcache) finds it here, on x64 and arm64 alike. The image is 15 GiB used; [ADR-005](../reference/decisions/adr-005-runner-image-parity.md) records what parity costs and what is still missing (the heavyweight software: .NET, Android, browsers).

## Pull, verify, promote

```bash
billet images pull ubuntu-2404-x64          # signed manifest, staged and verified, then imported
billet images verify ubuntu-2404-x64@<gen>  # boot one, make the guest prove it, record it
billet images compatible                    # every configured image speaks this binary's contract
billet images list                          # what exists, what is verified, what tiers boot
billet images due                           # is the image old enough to rebuild (exit 2: no)
billet images reap --keep 3                 # remove generations nothing needs
billet images promote|unpromote <image>@<gen>
```

**A pull verifies the signature before it parses anything.** Every asset is checked against a digest the manifest names, which is worth nothing on its own: a manifest somebody else serves names digests of bytes they chose. So the pull first verifies the Sigstore signature over the manifest bytes against a pinned identity, billet's own publication workflow on its main branch (pinned to the workflow and the ref, not the repository, because pinning the repository alone would accept a signature from a workflow a pull request added). The trust root is embedded in the binary rather than fetched, because a node may be air-gapped. It stages to disk and verifies before importing, because streaming unverified bytes into shared storage is a cluster operation to undo. A pull also installs the paired kernel durably before it publishes the generation, so a power loss cannot leave a published generation whose kernel vanished.

**A source that is not billet's must say what makes it trustworthy.** Pointing at your own mirror and configuring nothing is refused rather than silently unverified: set `images.signing_identity` and `images.signing_issuer`, or pass `--skip-signature-verification` deliberately. Sideloading with `--from <dir>` applies the same policy; a directory is not more trustworthy than a download.

**`@verified` is what lets a fleet take up a new image with no config edit.** A tier names one of two things:

```yaml
image: ubuntu-2404-x64@g20260814145813   # exactly this, forever
image: ubuntu-2404-x64@verified          # the newest one proved to boot
```

Verification records itself, the next launch resolves to it, and rollback is `billet images unpromote`, one command against the cluster rather than an edit on every node. A bare image name is refused: naming `@verified` is the decision. If nothing has passed verification, `@verified` refuses rather than booting something unproven, and a generation whose snapshot is gone is never resolved. The launch log names the generation, not the word. `@verified` is contract-relative, so during a rolling upgrade an older binary keeps resolving its newest compatible generation while the new binary selects the one it just proved.

**Verification records which kernel proved it**, under the same cluster lock reaping takes, because a generation the whole fleet takes up with no kernel recorded would boot against whatever each node happens to be configured with. A pull keeps the kernel in `/var/lib/billet/kernels` named by version and digest; `images reap` collects kernels no surviving generation names and refuses while any generation's kernel is unknown.

## Where images come from

The built-in source reads a signed pointer from billet's `release-channel` branch, verifies that the main publication workflow signed a still-current pointer (it expires after ten days, so rewriting the branch cannot pin a fleet to an old genuine channel) naming an immutable dated `guest-YYYYMMDD-HHMMSS` prerelease, then downloads that release. The rootfs is published in parts because a release asset caps at 2 GiB. Every image is built weekly by `.github/workflows/guest-image.yml` at whatever `actions/runner` GitHub has published, and passes two gates before it is published: its **contents**, by loop-mounting the filesystem (the runner is the version the manifest claims, Docker with Buildx and Compose, the agent's contract, the units enabled, root locked), and that it **boots** under Firecracker on the runner that built it, proving the kernel, systemd, the network, the metadata service and the agent in one sentence on the console.

**The kernel and the filesystem are a matched pair.** A guest booted with a different kernel fails in the middle of somebody's job, so they are published together and the generation records which kernel it was paired with. The kernel is built from `scripts/guest-kernel.config`, every option in `scripts/kernel/required-builtins.txt` built in (a microVM has no initramfs), and checked against moby's vendored `check-config.sh` before publication.

`scripts/build-guest-image.sh` remains, for a custom image or an air-gapped build. It is no longer the normal path; a per-node rebuild timer was removed because it made N machines do N builds of a byte-identical artifact.

## The runner-release deadline

GitHub refuses a self-hosted runner about thirty days after a newer `actions/runner` release, and billet's runners cannot update themselves (every JIT registration disables auto-update). `billet runner check` reports how close the runner on your image is to being refused: exit 0 while there is nothing to do, 2 once a rebuild is due, 3 once GitHub is already refusing. Failing to reach GitHub is an error, not one of the three.

**The clock starts at the first release newer than yours, not at the newest one.** Counting from the newest moves a deadline that has already passed every time something else ships: 2.334.0 went out of date when 2.335.0 was published on 2026-06-08, and counting from 2.336.0 on 2026-07-20 described a fleet GitHub had refused for six weeks as having a month in hand. The check reads the runner version the image itself recorded, because the image is the only thing that knows what the fleet is running. It never states a deadline it cannot place, and thirty days is the ordinary window rather than a promise, since GitHub may enforce a critical release at once. `images pull` asks the same question about the image it imports and refuses only a proved expiry (`--allow-stale` overrides); image age is reported as maintenance information and decides nothing. A daily workflow opens a pull request when a runner release lands, and merging it is the gate.

## On EC2

The `ec2` backend boots an AMI built by `billet ami build` from the same declaration, verified by booting it, and stamped with a contract tag only after the boot proved the properties. An AMI is region-scoped. [AWS with EC2](../deploying/aws-ec2.md) covers it; `billet images` does not manage AMIs.

## On a Mac

A tart node pulls what its tiers name with `billet images pull`, into tart's own per-user store; a launch refuses an image that is not present rather than fetching tens of gigabytes inside a job. Guest images billet builds for a Mac are still open ([Run jobs on a Mac](../deploying/mac-tart.md)).
