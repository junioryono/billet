---
name: billet-releases-and-upgrades
description: "Load when tagging a release; touching .goreleaser.yaml, the release or cut-release workflows, deploy/, internal/releasesource, internal/provenance, internal/rollout, internal/hostupgrade, the converge guard, `billet release inspect`, `billet node migrate-endpoint` or `receipt`, or internal/ops/host's rollout*, hostupgrade*, upgrade* and convergeguard* files; or when answering how a release, a rollout or a host upgrade behaves."
---

# Releases, rollouts and host upgrades

## What this area is

A release is a semantic-version tag (`vX.Y.Z`, because Go resolves nothing else; staying on `v0` keeps the module path free of a suffix) cut from a `release/vX.Y` branch by the `cut-release.yml` workflow, built by `release.yml` through GoReleaser (`.goreleaser.yaml`), and published as immutable archives, `.deb`/`.rpm` packages, a signed `release-manifest.json` and a moving `v0` tag. `internal/releasesource` reads manifests and channel statements; `internal/provenance` records which manifest produced the installed binary. `internal/rollout` is one durable fleet decision and its coordinator; `internal/hostupgrade` is the journaled transaction that replaces billet on one machine; `internal/ops/host/upgradedecision.go` is the fence between them. `docs/reference/decisions/adr-006-rollouts.md` and `docs/operating/upgrades.md` are the design and the operator page.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### Publishing: [references/publishing.md](references/publishing.md)

- **What a release publishes.** CGO-free builds for the three supported targets, archives named for `install.sh`, deb and rpm that deliberately never package `/var/lib/billet`.
- **The manifest is built and signed inside GoReleaser's `signs` stage, and that ordering is forced.** An immutable release refuses any upload after publication.
- **A release manifest is signed as `release.yml@refs/heads/main`, and the policy says so.** `releasesource.PublishRefPattern` accepts that identity and a hand-pushed hotfix tag, nothing else.
- **`cut-release.yml` calls `release.yml` rather than relying on the tag push.** A ref pushed with `GITHUB_TOKEN` starts no workflow.
- **Installing the package directly is not a transactional upgrade.**

### Automatic updates: [references/automatic-updates.md](references/automatic-updates.md)

- **A deployment updates itself unless `release.automatic` is written `false`.** The starter never starts an older release or a digest an operator aborted.
- **The controller's own upgrade is a root timer, and `--from-rollout` is what it runs.** The server is unprivileged; a completed rollout is taken once per host, through the settled mark.
- **The ledger records the newest release that has served it, and every open refuses a proved older binary.** `version.Compare` orders both spellings of a release; a downgrade is always named.

### Rollouts: [references/rollouts.md](references/rollouts.md)

- **A rollout resolves the channel once and persists the manifest's digest.** Every transition comes from an observation, never from elapsed time, and the controller goes first.
- **A registration epoch is the only causal evidence the coordinator has.**
- **A refused dispatch records its reason, and `rollout status --json` is the ledger's own account.**
- **The upgrade dispatch carries the whole spec.**
- **A version is a name and the manifest is the bytes.** A host's provenance is proved, blocked, or converged with the absence recorded.

### The host-upgrade transaction: [references/host-upgrade.md](references/host-upgrade.md)

- **`billet host-upgrade` is a separate detached program with everything on disk.** `claimed → staged → stopped → imaged → fenced → snapshotted → installed → migrated → probed → committed | rolled_back`; the order is the safety content.
- **One transaction, two service managers, three ledgers.**
- **Two answers over a Unix socket, and a spawn is not an answer.**
- **The probe answers by exiting or by saying it is ready, the candidate says which, and the fence is cleared under the reason it was written with.**
- **The claim bounds concurrency; the decision mark fences generations; the transaction lock is a lock.**

### The upgrade root and the converge guard: [references/upgrade-root-and-guard.md](references/upgrade-root-and-guard.md)

- **The claim is one name with three shapes, and a converge holds it as a directory.** Nothing expires a guard.
- **The upgrade root's trust boundary is proved through descriptors before the lock, and again under it.**
- **A version line names a build or the read is an error.**
- **The guard record carries a `note`.**

### Controller retirement

Retiring a controller (`billet server retire`, the guard's retirement marker, the journal and the tail) is its own skill: `billet-controller-retirement`.

### Host inspection and endpoint moves: [references/host-inspection.md](references/host-inspection.md)

- **`billet release inspect --json` is the host's own account of what it runs, and every field is a value or an explicit unknown with a reason.** It never guesses and never changes the host.
- **A node's endpoint is moved by three commands, and the role calls them.**

### The Ansible transaction: [references/ansible-transaction.md](references/ansible-transaction.md)

- **The Ansible host role is the second implementation of the transaction, and it runs inside the guard the role's preparation holds.**
- **The Ansible role gives way to the rollout.**

### Guest images

**Guest images use the same immutability and publish only dated prereleases.** See `billet-guest-images`; the channel branch is `release-channel`.

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Changing the host-upgrade transaction.** Read [references/host-upgrade.md](references/host-upgrade.md) whole. A step that moves changes `TestAHealthyUpgradeRunsInOrder` in the same commit. The Ansible role's transaction is the second implementation, so change it to match and run `make host-upgrade-order unit-parity emitted-block-check`. Before a release that carries it, run `make rollout-rehearsal`, which needs the rehearsal App.

**Adding a member to a JSON answer** (`release inspect`, `rollout status`, the guard record, `server retire`). The field set is pinned by name in the producer's test, and the fixtures a role reads are written by the producer's tests with `BILLET_UPDATE_FIXTURES=1`, never by hand. A guard member is also read by `guard_fallback.py`, whose `OPTIONAL_MEMBERS` must admit it.

**Touching controller retirement.** Load `billet-controller-retirement`.

## Where the tests are

- `internal/rollout/*_test.go` (`dispatchguard_test.go`: no host is disturbed by a pass that could not settle), `internal/hostupgrade/upgrade_test.go` (fourteen ordering tests), `internal/releasesource/*_test.go`, `internal/provenance/*_test.go`.
- `internal/ops/host/upgradedecision_test.go`, `upgradefence_test.go`, `upgradetxlock_test.go`, `hostupgradegate_test.go`, `releaserecord_test.go`, `releaserecordbounds_test.go`, `rollout*_test.go`; `cmd/billet/upgradeprobe_test.go`; `internal/app/proofs_test.go`, `rolloutstarterwiring_test.go`.
- `scripts/releasemanifest_test.go`, `scripts/movingmajor_test.go`, `scripts/scripts_test.go` (install.sh), the Ansible scenario targets.
- `scripts/rollout-rehearsal.sh` (`make rollout-rehearsal`): two packaged hosts moved from one published release to the next by a rollout under real systemd, the controller through `billet-upgrade.timer` (or, on a FROM before v0.6.0, the operator's `host-upgrade`), then a downgrade candidate that cannot open the migrated ledger proved to roll back; the first real run of `hostupgrade.go`'s doing rather than its ordering, recorded in `docs/reference/records/host-rehearsals.md`.

## Related skills

`billet-node-wire` (server-first and the version range), `billet-state` (the schema version and the fence), `billet-lifecycle` (the units and packages), `billet-guest-images` (the guest channel), `billet-git-flow` (branching for a hotfix).
