# ADR-014: The guest image is GitHub's own build, run on Ubuntu's own base

Accepted, 2026-09-28. Issue #250. Supersedes, for the Firecracker guest image, ADR-005's decision that parity is a rebuild from GitHub's declaration with billet's own installers; ADR-005 still governs the EC2 AMI.

## Context

ADR-005 established that GitHub publishes the Packer template that builds its hosted image and never the image itself, and chose to read GitHub's declaration (`toolset-2404.json`) and reimplement the install scripts. That reached 15GiB of parity and then kept finding what the declaration does not name: the GitHub CLI, the Azure and Google CLIs, the default Go on `PATH`, `bc`, each discovered by a workflow failing on the fleet where it had passed on a hosted runner. Every one was a script GitHub runs that billet had not copied, and the rule the maintainer set is that someone must be able to switch from GitHub's runners to billet and never have an issue.

The reason ADR-005 gave for not running the scripts was that they need Packer's environment, `$HELPER_SCRIPTS`, PowerShell for their tests, and a booted machine, none of which a `debootstrap` chroot provides. All of that can be provided: the environment is a list in the template, PowerShell is something the scripts install themselves, and `systemd-nspawn --boot` gives a booted machine without KVM.

## Decision

**The guest build runs GitHub's template.** `scripts/runner-images/upstream/` vendors `actions/runner-images` at one commit, every file held to its git blob id and every directory to its tree id, the commit equal to the toolset pin. `plan.json` is the template's provisioner list, derived by a test-side reader and pinned. `scripts/run-runner-images.sh` runs it step by step with Packer's environment, against a machine booted with `systemd-nspawn --boot` from Ubuntu's pinned cloud root filesystem (its checksum list's signature checked against Canonical's key), inside the image file the build writes through. billet's own layer (the runner, the agent, the cache helpers, the network and boot configuration) goes on after.

**Everything billet does differently is one file.** `scripts/runner-images/differences.tsv` lists each `skip` and each `prepare:<name>` the runner performs before a step, with its reason: the Azure agent's deprovisioning and `waagent.conf`, snapd, the build user GitHub's Homebrew step runs as, Docker's classic store for billet's fenced `/var/lib/docker`, the one system test that asserts Azure's `sd*` disks, and a pipx pin GitHub made after the vendored commit. An entry naming a step the plan does not have stops the build before anything runs.

**The build runs on a fleet tier.** GitHub's image does not fit a hosted runner's disk. `guest-image.yml` runs on `billet-image-builder-ubuntu-2404` (16 vCPU, 64GiB, 320GiB), a Firecracker guest with no `/dev/kvm`, which nspawn does not need. Every generation is still booted by `billet images pull --verify` before a tier can resolve `@verified` to it.

## Measured

- The image: `contents: 53779M used, 22176M free of 81920M`, the same within a few megabytes on three builds on the builder (2026-09-28). ADR-005's build measured 15.0GiB without the Android SDK.
- The first four trials ran 22, 66, 71 and 84 of the template's 84 steps. What stopped them: a TLS stall to one Azure blob host that did not recur (the path MTU from the builder is a clean 1500); a script that installs pipx unpinned, fixed upstream after the pin; and nspawn's own `/tmp`, a tmpfs capped at a tenth of memory, which the toolset's downloads fill.
- The fit-to-publish gate then failed checks written for the old build: tools GitHub installs to `/usr/bin` rather than where billet's installers put them, pipx as a command rather than a package, dotnet tools under `/etc/skel`, and a chroot whose `/dev` and home a booted guest would have had.

## Consequences

- The image is about 3.6 times larger. Capacity is not the constraint: the reference cluster's Ceph pool has 3.2TiB available, and a job's disk is a copy-on-write clone.
- A pinned template drifts from what its scripts download, so a pin that built GitHub's image in July can fail in September. Each such failure is carried as a difference copying GitHub's own later fix, and moving the pin retires it. Moving the pin also moves the toolset the EC2 build reads.
- Upstream bugs are parity too. `install-bazel.sh` writes `USE_BAZEL_FALLBACK_VERSION=silent:` with no version, and so does GitHub's image.
- The EC2 AMI still builds from the declaration with the shared toolcache installers, so the two backends now reach parity by different routes.

## Alternatives rejected

- **Keep reimplementing.** Every gap found so far was a script not copied, and the next one is found by a user's workflow.
- **Run the Packer template itself.** Its only builder is `azure-arm`.
- **Boot the base in Firecracker in CI.** The builder is a Firecracker guest without KVM, and nspawn needs none.
