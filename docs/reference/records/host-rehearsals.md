# Host rehearsals

Four operations were, until 2026-09-04, proved only against fakes: the host upgrade transaction (`cmd/billet/hostupgrade.go` stops a service, replaces a binary and migrates a ledger, and every test of it supplied a fake host and asserted the order), `billet local recover` over a deployment a real control plane had served, a CA rotation across nodes that renew, and a PostgreSQL promotion under a real partition. This records the harness that now drives each of them on packaged hosts under real systemd, what its first run found, and what has and has not run.

## The harness

`scripts/rehearsal-lib.sh` starts privileged `ubuntu:24.04` containers with systemd as PID 1, the shape `scripts/test-systemd-lifecycle.sh` already trusts, installs a `.deb` into each, issues node certificates as the service account, and asserts on unit state, journals and files. Four drivers, each a `make` target and a job of `.github/workflows/rehearsals.yml` (weekly, and on dispatch; never on `pull_request`, because every one starts a real control plane with a real App's key):

| Target | What it drives | Package |
|---|---|---|
| `make rollout-rehearsal` | two hosts on release FROM moved to release TO by `billet rollout start` alone, the controller through `billet-upgrade.timer` (on a FROM before v0.6.0, the operator's `host-upgrade`), the node through the coordinator's dispatch; then a downgrade candidate that cannot open the migrated ledger proved to roll back with the hosts still on TO | the two published releases |
| `make recover-rehearsal` | a served deployment backed up, moved on, stopped with `local down`, recovered as root; the recovered plane starts sealed with an operator's seal, the node re-registers, `billet resume` lifts the seal, nothing root put back stays root-owned | the tree's own (`make dist`) |
| `make ca-rotation-rehearsal` | two nodes on twenty-minute leaves (`billet ca issue --lifetime`), `ca rotate`, the control plane restarted to present the overlap, both nodes proved renewed onto the new authority, `ca retire`, both re-registering with the old authority gone | the tree's own |
| `make promotion-rehearsal` | an active-passive pair on one PostgreSQL ledger with short server keepalives, the leader cut off with `docker network disconnect`, the standby's promotion timed, the node re-registering, the old leader proved to stop and return as a standby when the partition heals | the tree's own |

Three rules the harness holds, each written down because a review found the rehearsal passing without it: a registration is proved by the controller journal's `node registered node=<name>` line after a clock mark taken before the boundary, never by the name appearing in `billet status`, which prints every node the ledger knows whether or not one has connected; a negative assertion captures the producer's exit status and greps a here-string, because `cmd | grep -q` under `pipefail` and `|| true` both turn "could not tell" into the absence asserted; and a teardown that fails fails the run, since a green rehearsal that left a scale set behind hides the one thing an operator has to act on.

Every rehearsal needs a config whose `github:` block names a working App and the key it points at (`BILLET_REHEARSAL_APP_CONFIG`, `BILLET_REHEARSAL_APP_KEY`) and skips without them; the workflow uses the dedicated acceptance App described in [AWS acceptance](aws-acceptance.md). The architecture is the docker daemon's, not `uname -m`'s, so the harness runs on a Mac's Docker Desktop as well as on `ubuntu-latest`.

## What the first run found

**2026-09-04 (UTC), `make rollout-rehearsal` with FROM=v0.5.0 and TO=v0.6.0, on arm64 under Docker Desktop.** Both packaged hosts came up under real systemd on v0.5.0, the controller claimed its ledger and created the scale set, and the node registered over the real mTLS wire at once. The rollout's first command then refused:

```
billet: imagesource: the manifest's signature does not satisfy this source's policy
  (identity ^https://github\.com/junioryono/billet/\.github/workflows/release\.yml@refs/(heads/release/v[0-9]+\.[0-9]+|tags/v[0-9]+\.[0-9]+\.[0-9]+)$ ...):
  expected SAN value to match regex ..., got "https://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/main"
```

Every release manifest billet has published is signed as `release.yml@refs/heads/main`, because the cut button runs on `main` and calls the release workflow, and a Fulcio certificate names the requesting workflow under the ref of the run that requested it. The policy in every binary through v0.6.0 accepted only a release-branch or tag ref, on the belief that the workflow runs against the tag. So no shipped billet could verify any release: `billet rollout start`, the automatic starter that v0.6.0 introduced and `billet host-upgrade --version` all stop at that line. The teardown removed the scale set and the run exited non-zero, which is the harness doing what it is for. The fix is the pattern naming the two refs the workflow can run under and a test pinning the measured SAN; it ships as v0.6.1, and a fleet on v0.5.0 or v0.6.0 is moved by the package or the Ansible role once, which install from the release's checksums rather than its manifest.

The finding cost one release cut and had never been visible to a test, because every fixture signs its manifests with whatever identity the test names. It is the kind of defect the rehearsal exists for.

## What the first recover runs found

**2026-09-04 (UTC), `make recover-rehearsal` three times on this Mac, each stopped by something it exposed, none yet reaching the recovery itself.** The first run died in the harness before a host was up: `aliases[@]: unbound variable`, an empty array expanded under `set -u` on the Mac's `/bin/bash` 3.2, and it exited 0, because on that shell a `set -u` abort reaches the EXIT trap with `$?` of 0 (bash 5.2 and 5.3 report 1; measured with a probe). The harness now initialises a sentinel to 0 before its traps, sets it to 1 as the script's last statement, and fails any run that reaches the trap with status 0 without it. The second run got both hosts up and was refused by `billet local up`: the acceptance App had been given `actions: write` so the acceptance lane could dispatch a workflow, and `billet check` refuses an App whose installation holds a permission billet never requested; the dispatch is a second App's job now and the acceptance App is back to the two permissions the manifest flow asked for. The third run commissioned the deployment, registered the node, took the backup and moved the ledger on, then `local down` was refused: `the ledger is fenced for host maintenance`. The journal explains it: a minute after boot the control plane logged `started a rollout, because the stable channel advanced` from `0.0.0-SNAPSHOT-…` to `v0.6.0`, because a snapshot build reports a release the channel is not on and the deployment said nothing about `release:`, so it was on automatic updates; the packaged root `billet-upgrade.timer` then began the host transaction, fenced the ledger and started replacing `/usr/bin/billet`, which is also why the teardown's `billet teardown` found the binary unrunnable and left the scale set behind (removed by hand with `billet teardown --tier … --runner-group default`). The recover, CA rotation and promotion rehearsals now write `release: {automatic: false}`, since the thing they rehearse is not a rollout, and `billet acceptance up` writes it into every derived config for the same reason: an acceptance deployment proves the tree it was built from, never the channel. The rollout rehearsal keeps the default, because the default is what it rehearses.

**The fourth run passed, 2026-09-04 05:31 to 05:37 UTC, on arm64 under Docker Desktop, package `0.0.0-SNAPSHOT-6fa05f3`.** Both packaged hosts up under real systemd; the deployment commissioned by `billet local up` with the acceptance App, the node registered over the real wire within a second of starting; `billet local backup` took the archive; the ledger moved on; `billet local down` asked the node what it was running (an empty answer twice, the compute barrier's proof), stopped and disabled the service; `billet local recover --old-controller-fenced` as root planned and then replaced the ledger, keeping the one that was there as `billet.db.superseded-2026-09-04T05-32-12Z-2c512640`, and left the deployment sealed by an operator's seal; `billet local up` started it sealed, enabled and armed the backup and upgrade timers, and left the seal alone, as the restore rehearsal said it must; the node that had trusted the old ledger registered with the recovered one at once; `billet resume` lifted the seal. Nothing in the state directory was left root-owned. 388 seconds from the first container to the PASSED line, and the teardown removed the run's scale set. What this proves that the in-process suite could not: the recovery runs as root over a directory a real service account owns, on a ledger a real control plane served, and the plane that comes back is the sealed one the design says it must be.

## What has run and what has not

| Rehearsal | Status |
|---|---|
| Rollout | ran to its first step and found the signing-identity defect above; the full run (both hosts moved, the downgrade rolled back) follows on v0.6.1 |
| Recover | **passed** on 2026-09-04 (fourth run, 388s, arm64) after three runs that each stopped at something they exposed (above) |
| CA rotation | not yet run against a real App |
| Promotion | not yet run against a real App |

The `status.md` cells these rehearsals exist to move stay at "Not yet" until each has run; a harness is not a proof. The recover cell moved on 2026-09-04.

## What the harness does not prove

- The hosts are containers on one machine, not two machines: power, network and disk failure domains are shared, and the promotion's partition is a network disconnect rather than a lost link.
- The promotion rehearsal copies the CA directory between controllers by hand; `billet ca sync` needs an identity store the rehearsal does not have.
- No `--abandon` of an interrupted recovery is staged: nothing in `deployarchive` stops a publication mid-way deterministically.
- The rollout's failing candidate is a downgrade, because a rehearsal that runs on the day a release is cut has exactly two releases to work with.
