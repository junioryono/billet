# ADR-010: A deployment updates itself, and cannot go backwards by accident

## Status

Accepted. Implemented.

## Context

[ADR-006](adr-006-rollouts.md) made a fleet upgrade one durable decision and gave every node a transactional updater. What it left was the decision itself and the controller's half of carrying it out: `release.automatic` was parsed, validated and documented, and read by nothing, so no rollout ever began without an operator typing `billet rollout start`. A rollout that did begin could not converge unattended either. The coordinator is server-first, the control plane runs as an unprivileged account with an empty capability set, and a process cannot install its own successor, so nothing on a controller host replaced the binary the rollout decided on. Guest images were refreshed only by the Ansible host role's transaction, not by the Go one and not on any schedule, so a firecracker node upgraded by the Go path across a guest-contract change had `@verified` resolve to nothing it could boot. And the only thing standing between a deployment and a downgrade was the schema check, which is silent for every pair of releases that share a schema, which is most of them.

The deployment this was built for is a firecracker node in one place and a control plane on EC2 in another, converged by the Ansible role from release tarballs, with a Mac node to come. Every piece had to work on a host with no package, on a Mac with no root, and on a PostgreSQL controller with no ledger file.

## Decision

### Absence means on

`release.automatic` is a pointer whose nil is true. A deployment that says nothing follows the stable channel and updates itself; `automatic: false` is the one sentence that turns that off, and it turns off everything: the control plane starting rollouts, the scheduled updaters acting on one, and the guest-image refresh. This is the one zero value in the config that does not refuse, and it is on because the failure an unattended deployment actually meets is the update that never happens — a runner GitHub stops queueing to, a fix that shipped and never arrived. What makes that safe to default is the drain, the verified candidate and its rollback, and the release watermark below.

### The channel is read by the control plane, and nothing else decides

`internal/rollout.Starter` runs beside the coordinator, hourly. It is network-free, because a ledger writer may not reach the network; the channel is resolved and the manifest verified outside it, by the same two functions `billet rollout start` uses, through a `Resolver` handed in. Every step of a tick is an observation: the config allows it and the window is open; no rollout is open; the channel names a target this build could install; the fleet is not already on it (a host reporting no release counts as not on it); the target is not older than the running release; nobody aborted a rollout to exactly those bytes. Then it records the same `Store.Start` the command records, as `automatic (stable channel)`. An operator's abort beats the channel until the channel moves on, because a rollout abandoned with a reason must not be restarted an hour later with nothing new to go on. A resolution failure is logged once every six hours and waited out, not reported as a failed pass, because an expired channel statement is a condition the next tick may find changed.

### A root process on every controller host acts on the recorded decision

`billet host-upgrade --from-rollout` reads the fleet's decision through the operator handle, the open rollout or the last completed one, and, if this host is not on its target (or runs its version from another manifest), runs the host transaction with the rollout's own pin, digest, id and generation, under every fence the node's dispatch is under, re-reading the decision after the claim so an abort in the meantime refuses. A completed rollout is followed because a PostgreSQL standby is a controller host the rollout's controller phase says nothing about, and its timer may fire after the rollout closed. It consults no channel. The package enables `billet-upgrade.timer` to run it every five minutes; the Ansible role renders and enables the same unit; on a Mac `billet local up` installs the `sh.billet.upgrade` agent. This is the one exception to "the package enables nothing": that rule keeps an install from connecting a machine to GitHub before its config says something true, and this timer connects nothing, exits doing nothing with no ledger, no rollout or `automatic: false`, and is what makes an unattended deployment able to take the update it decided on. A standby PostgreSQL controller is not a node in a rollout, so it too runs the timer.

### Guest images follow the same rule

`billet images refresh`, run daily by `billet-images-refresh.timer` or the `sh.billet.images` agent, pulls, boot-verifies and promotes only when the signed image channel names an image built after the newest generation imported. A generation is named for the moment it was imported, which is always after the build it came from, so the comparison needs no new cluster metadata and the same image is never imported twice; a pull is followed by a reap to three verified generations per guest contract. The Go host transaction has the step the Ansible role always had, `imaged`: with the services stopped and before the ledger is fenced, the candidate asks which configured images speak its guest contract and pulls a generation for each that does not.

### The ledger refuses to be served backwards

Migration 48 adds a release watermark: the newest release that has served this ledger. Every open compares the running release with it and refuses a proved older one, whichever handle is asking — the control plane's, an operator command's, the upgrade probe's — with the two ways through named. Only a control plane records a newer release, and it does so in `ClaimController` after the deployment binding and the epoch have committed, never at open: `billet check` run from a laptop carrying a newer binary records nothing, and a newer server pointed at another deployment's ledger is refused by the binding with the mark untouched; a promoted standby records at promotion, through the same claim. An operator handle that was open before a newer control plane claimed re-reads the mark inside every write transaction and refuses, because two releases that share a schema pass the schema re-check alone. A release that cannot be ordered against a tag, a development build, neither refuses nor records. A deliberate downgrade is `--allow-downgrade` on `host-upgrade` or `rollout start`: the transaction lowers the mark inside its migrate step, after the snapshot that keeps the higher one, so a rollback of the downgrade restores the refusal; on an external ledger, which has neither, it lowers the mark through the operator handle just before the candidate is probed. Nodes record the highest release they have registered with and `billet status` reports a host running something older, as a note and never a refusal, because a rolled-back host re-registering on its previous release is exactly what the coordinator reads as the rollback.

### One transaction, two service managers, three ledgers

The host transaction's order is `internal/hostupgrade`'s and does not change. What changes is the vocabulary underneath it. On a Mac the `Host` is `launchdHost`: a stop is a bootout with every pid launchd named proved gone, a start is a bootstrap with the same pid proved to survive a settle window, the preserved files are the two plists beside the binary and config, and the paths are `/usr/local`'s. The updater runs as the operator, started detached by the node agent it then boots out; that a detached child outlives its agent's bootout was measured on macOS 26 rather than inferred from the guest case, which was measured on a node that was killed. The one privileged path is `/usr/local/bin`, which the Mac setup chowns to the operator and which `billet local up` and the updater both refuse without, before anything drains.

On a PostgreSQL ledger the transaction fences, snapshots and migrates nothing, because billet copies no PostgreSQL database and the migration is already the controller claim's right ([ADR-009](adr-009-controller-election.md)). The candidate is probed through the standby's open, allowed across the fence, which claims and writes nothing; the journal records the three skipped steps as reached so a resume runs the same shape; the rollback boundary is the candidate's start, and what lies past it is the database's own backup. Every controller host runs the timer, and in either order the pair upgrades in the follower-first shape the election was designed for. What that costs is stated: a leader upgraded before its standby that crashes in the minutes before the standby's timer moves it leaves an older standby refused as a downgrade until it is.

## Measured

- A `Setsid` child of a launch agent survives `launchctl bootout` of that agent and keeps running after the agent's process has exited (macOS 26, `TestASetsidChildSurvivesItsAgentsBootout`).
- A oneshot with `StartInterval` and no `KeepAlive` runs at load and stays loaded, without a process, after exiting 0 (macOS 26, `TestAnIntervalAgentRunsAtLoadAndStaysLoaded`).
- The PostgreSQL probe open verifies, claims nothing, refuses every write and leaves an unmigrated schema unmigrated (`TestThePostgresProbeVerifiesAndChangesNothing`, against a real server).

## Consequences

**A deployment that says nothing moves.** Within an hour of a release reaching the stable channel it has a rollout; within minutes more its controller hosts have moved; its nodes follow one at a time; its images follow within a day. `billet rollout status` and `billet status` say where it has got to.

**A stuck rollout blocks the next one, deliberately.** An unreachable host holds a rollout open, an open rollout stops the starter, and the channel advancing changes nothing about that. Both are visible, and both have a named operator decision that resolves them.

**The Ansible role and the rollout share a host, and the role gives way.** A converge that finds `billet host-upgrade`'s claim on the host refuses, naming `--status` and `--resume`. A pin that would move a host to an older release than it runs is refused before anything drains. The shape for a fleet on automatic updates is to pin `billet_version` for a host's first install and then leave the binary to rollouts.

## Alternatives considered

**Nodes and the controller each following the channel.** Rejected in ADR-006 and rejected again here for the controller: the channel advancing between hosts is a fleet on two releases from one decision.

**The server process starting the updater itself.** Impossible on Linux rather than rejected: it runs unprivileged and a detached child would be unprivileged too. On a Mac it would work, and the timer is used anyway so the two platforms share one mechanism.

**A `pg_dump` snapshot inside the transaction.** Rejected: a half-measure that copies rows through billet's own connection produces an archive that looks like a backup and is not, which is ADR-008's reasoning and still holds.

**Refusing a node that re-registers on an older release.** Rejected: that registration is the coordinator's only evidence of a rollback.
