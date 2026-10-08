---
name: billet-guest-images
description: "Load when touching the vendored runner-images template or its nspawn build (scripts/runner-images, run-runner-images.sh, runner-images-nspawn.sh), internal/runnerimages, the guest image and kernel builds in scripts/, the kernel config gate, internal/imagesource, internal/provenance, `billet images`, internal/runnerrelease or `billet runner check`, the EC2 AMI, the guest-image or runner-release workflows, or a tier's `image:`; or when an image fails to verify, pull, boot or be reaped."
---

# Guest images, kernels and the runner deadline

## What this area is

A Firecracker job boots a golden image: an Ubuntu 24.04 rootfs with Docker, the Actions runner and a small agent that reads its registration from the metadata service. It lives in Ceph as an RBD image with immutable snapshots called generations, and every job gets a copy-on-write clone. billet builds it once, centrally (`.github/workflows/guest-image.yml`, weekly), signs the manifest with Sigstore, publishes a dated `guest-YYYYMMDD-HHMMSS` prerelease and advances a signed pointer on the `release-channel` branch; a deployment pulls it with `billet images pull`. The EC2 backend boots an AMI built by `billet ami build` from the same declaration. `scripts/runner-images` vendors GitHub's `actions/runner-images` tree at one commit and the guest build runs it; `internal/runnerimages` vendors GitHub's `toolset-2404.json` and the EC2 toolcache installer; `internal/imagesource` fetches and verifies; `internal/runnerrelease` knows which `actions/runner` is installed and how close to refusal it is. `docs/reference/decisions/adr-005-runner-image-parity.md` records what parity costs.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Building the image: [references/image-build.md](references/image-build.md)

- **The guest image is GitHub's own build, run on Ubuntu's own base (#250).**
- **The template drifts from what it downloads, and each drift is carried as a difference until the pin moves.**
- **The nspawn driver proves what it stops, three ways, before the workspace is touched.**
- **The build runs on a fleet tier, not a hosted runner.**
- **The build job checks out the release the stable channel names, never `main`.**
- **The guest carries billet itself, for the build caches (#226).**

### The kernel and the gates: [references/kernel-and-gates.md](references/kernel-and-gates.md)

- **The kernel and the filesystem are a matched pair.**
- **The kernel gate's verdict is its exit status, and billet's own rule is the artifact-level claim.**
- **Two halves of a gate before an image is published.**

### Publication, verification and pulling: [references/publication-and-pull.md](references/publication-and-pull.md)

- **The manifest is the only thing that needs signing, and it is verified before it is parsed.**
- **Manifest schema 2 splits the rootfs into ordered parts, and the reader shipped before the writer.**
- **A tier says `@verified` or an exact generation, never a bare name.**
- **A node takes up a newer published image by itself, and the same image is never imported twice.**
- **A per-node rebuild timer is gone on purpose.**
- **The AMI is the same contract on the other backend.**

### The runner-release deadline: [references/runner-deadline.md](references/runner-deadline.md)

- **The runner deadline starts at the first release newer than yours.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A change to the image build.** Every difference from GitHub's template is carried as a difference until the pin moves ([references/image-build.md](references/image-build.md)), and both halves of the gate must pass before anything is published ([references/kernel-and-gates.md](references/kernel-and-gates.md)). `billet-shell-gates` holds the rules for the scripts themselves.

## Where the tests are

- `internal/runnerimages/*_test.go`, `internal/imagesource/*_test.go` (policy, multipart, trust root), `internal/runnerrelease/*_test.go` (the window rules), `internal/provenance/*_test.go`.
- `internal/ops/images/images*_test.go`, `imageprobe_test.go`, `imagescompatible_test.go`, `imagesreap_test.go`, `runnercheck_test.go`, `verifypair_test.go`, `tartimages_test.go`, `ami_test.go`; `cmd/billet/nodename_images_test.go` (the node name a certificate gives, through `billet ca issue`).
- `scripts/kernel_gate_test.go`, `scripts/image_manifest_test.go`, `scripts/movingmajor_test.go`, `scripts/guest_image_*_test.go`.

## Related skills

`billet-shell-gates` (the build scripts), `billet-backup-restore` (the durable kernel install and lock), `billet-storage-and-cache` (the Ceph generations), `billet-providers-local` (the kernel pair at launch), `billet-providers-aws` (the AMI), `billet-releases-and-upgrades` (channels and signing).
