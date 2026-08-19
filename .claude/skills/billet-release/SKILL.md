---
name: billet-release
description: "Cutting a billet release and updating a running host — coverage expectations, the tag-driven release pipeline, what the packages install, and how an operator upgrades without killing running jobs. Use when tagging a release, changing .goreleaser.yml or the systemd units, or answering how an upgrade behaves."
---

# Releases and upgrades

## Releases

Semantic version tags, because Go gives no choice: a module version must be `vX.Y.Z`, so a date-stamped tag is not a version Go will ever resolve. Staying on `v0` also keeps the module path free of a `/vN` suffix, which is required from major version 2 onwards.

One branch per MINOR version, carrying every patch on it:

```
main ──●──●──●──●──●──●──▶
        \              \
         \              release/v0.4 ── v0.4.0
          release/v0.3 ─┬─ v0.3.0   (cut)
                        ├─ v0.3.1   (hotfix)
                        └─ v0.3.2   (hotfix)
```

Cut with the **Cut Release** workflow (Actions → Run workflow). Blank version cuts the next minor; supply one to bump deliberately. A hotfix is a commit on the existing `release/vX.Y` branch and a press of the same button with the patch version typed in — then **merge that branch back into main**, or the next release reverts the fix.

`cut-release.yml` creates the tag and CALLS `release.yml` rather than relying on the tag push to trigger it. Before tagging, it rewrites both the internal composite-action refs and `ansible_collections/junioryono/billet/galaxy.yml` to the exact release version; a collection installed from a Git tag still reports the version in that file. A ref pushed with `GITHUB_TOKEN` does not start another workflow — GitHub's recursion guard — which is why a release button usually needs a PAT or an App. A `workflow_call` is not an event, so billet needs no repository secrets.

GitHub release immutability is enabled on the repository and is part of the release contract, not an optional hardening step. It locks the published tag and assets and creates GitHub's release attestation. `release.yml` first proves the release is immutable, then polls the repository attestation API for GitHub-initiated bundle URLs at the tag ref's digest and asks `gh release verify --format json` to fetch, decompress, cryptographically verify and exact-tag-filter the bundle. The REST response carries `bundle_url`, not the bundle itself; treating it as inline JSON makes the gate fail after publication. The exact-tag filter matters for lightweight tags because several tags can share one commit digest and another release's older attestation is not propagation of this one. The ref digest is an annotated tag object when the tag is annotated and the target commit when it is lightweight; forcing `^{tag}` rejects a lightweight hotfix only after its release has already published. An authenticated HTTP 404, a valid response with no GitHub release bundle, or `gh release verify` reporting that no bundle matches this exact tag is retried within the wall-clock deadline; authentication, authorization, throttling, server, transport, malformed-response and cryptographic-verification failures stop immediately. The verifier is checked out from `github.workflow_sha`, the immutable commit that defined the caller workflow, because a workflow called from main may build a maintained release branch whose tree predates the helper; billet's caller and reusable release workflow are always resolved from the same repository commit. The setting applies only to releases created after it was enabled; do not describe an older release as immutable merely because the repository is protected now.

Guest images use the same repository-wide immutability and therefore publish only dated `guest-YYYYMMDD-HHMMSS` prereleases. Never recreate a rolling guest Release: its tag and assets freeze on first publication. The publisher creates the dated tag at the exact built commit and pushes it before `gh release create --verify-tag`, so a conflicting pre-existing tag fails rather than relabeling the build. After proving the dated release immutable, the workflow signs a ten-day channel statement carrying that fact and fast-forwards `current.json` plus its Sigstore bundle on `guest-channel`; pullers authenticate and freshness-check the statement through raw.githubusercontent.com without sharing GitHub's anonymous REST limit. The two files can briefly come from different CDN generations while the branch advances, so a signature mismatch refetches the pair once before failing closed. The branch is only transport—the signature, expiry and immutable assertion are the trust boundary, so an unsigned edit or an old signed replay is refused. The unprivileged build job has read-only contents access, persists no checkout credential and holds no OIDC permission; only the main-guarded publication job downloads its checked artifact, signs it, publishes it and advances the channel. Marking guest releases as prereleases keeps the repository-wide latest alias reserved for binary installation.

## Updating a running host

Use the `junioryono.billet.host` role for an in-place release upgrade. It stages the candidate binary without touching the running installation, proves the selected guest image satisfies the candidate's guest contract, and downloads and boot-verifies a compatible immutable guest only when the recorded contract is missing or different. The transaction then stops the node first so compute drains, stops the server after custody is settled, preserves the stopped control-plane ledger and prior binary in a unique `/var/lib/billet/upgrades/` recovery directory, installs the candidate, validates and migrates with the new binary as the only ledger writer, and starts the server before the node.

If validation, migration, or restart fails, the role stops the new processes before restoring the prior ledger and binary, removes only the guest generation promoted by that failed attempt, and restarts only services that were active before the transaction. A second converge with the same binary is read-only for the transaction: it validates the host without draining or restarting either service. Do not move binary installation before the server stop or database migration after the new server start; those orderings violate the single-writer invariant and make rollback unable to restore the old schema safely.

The CLI pieces used by the transaction are intentionally useful outside Ansible. `billet images compatible` returns success when the exact selected generation records the candidate guest contract, boot-verifies once to backfill older verified generations that lack that metadata, and returns status 2 when a compatible guest must be pulled. `billet images pull --verify --result-file PATH` imports, boot-verifies, promotes, and atomically records the exact new generation only after all of those steps succeed, giving another provisioner a safe rollback handle. `@verified` resolution is contract-relative, so publishing a new-contract generation does not make nodes still running the prior binary select it during a rolling upgrade.

Installing the package directly remains available, but the package replaces the binary and units and deliberately does not enable or start either service. A manual operator is responsible for reproducing the same drain, compatibility, backup, validation, migration, restart, and rollback order; installing a new binary over a running control plane is not a supported transactional upgrade.

The node drain is safe because SIGTERM stops it taking new work and waits for the jobs already running. It is NOT instant: it takes as long as the longest job still running, up to `drain_timeout`. A second signal (`systemctl kill --kill-whom=main --signal=SIGTERM billet-node`) stops the waiting and tears down properly — which DESTROYS the jobs still running and fails those builds, because GitHub does not requeue a job whose runner vanished mid-execution. A third gives up where it stands.

## Deployment

`deploy/` holds the systemd units and the packaged config; GoReleaser's `nfpms` section turns them into `.deb` and `.rpm` (not `.apk`: Alpine uses BusyBox `adduser` and OpenRC, so a package built from these scripts and units could not install). Three things there are load-bearing and were each found by installing a package rather than by reading one:

- **`--config /etc/billet/billet.yaml` is explicit in both units.** billet's default config path is per-user and deliberately never reads the working directory; a unit that relied on the default would find nothing.
- **`lock_dir` is set in the packaged config.** billet derives its default from `HOME`, which a service does not have, so without this billet refuses to start rather than run without the lock that stops two processes managing one host's containers.
- **`TimeoutStopSec` is sized from `drain_timeout` plus the teardown.** systemd's expiry is a SIGKILL through the middle of the shutdown. Lower `drain_timeout` first, then the unit; never the unit alone.
- **The node unit runs as root; the server does not.** Docker socket access is already host-root, while Firecracker additionally needs TAP creation, device nodes, cgroups, chroots and verified process signalling. The jailer drops each VMM to its per-guest uid; running the host custodian as `billet` makes a valid Firecracker config fail only when its first job arrives. Keep `/srv/jailer` writable through the unit's `ProtectSystem=strict` sandbox.

The package **does not enable or start anything**. `/var/lib/billet` is created by postinstall rather than shipped, so a package removal cannot delete the deployment identity and the mTLS CA.
