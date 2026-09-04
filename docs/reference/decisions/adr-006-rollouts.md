# ADR-006: Upgrading a fleet is one durable decision

## Status

Accepted. Implemented. [ADR-010](adr-010-automatic-updates.md) adds the half this record left to an operator: the decision is now made by the control plane when the channel advances, and the controller's own upgrade is carried out by a scheduled root process.

## Context

Before this, updating billet was a per-host act with nothing recording what the fleet was converging on. An operator ran the Ansible role against each machine in turn; a control-plane restart midway lost the intent entirely, and there was no answer to "which hosts are done". Two facts made that worse than it sounds.

**A job may run for days, and GitHub does not requeue one whose runner vanished after it started.** So every step of an upgrade that touches a host running work is a step that can silently fail somebody's build, and the only correct wait is an unbounded one.

**The control plane cannot install its own successor.** The moment it stops, whatever was going to finish the job has stopped too — so a machine left with the old binary hidden and the new one half-installed has no process on it that knows what was happening.

## Decision

### One decision, resolved once, pinned to a digest

`billet rollout start` resolves a signed channel to exactly one immutable release and persists that manifest's **digest**. Everything downstream reads the digest; nothing consults the channel again.

The alternative — each component following the channel itself — produces a fleet on two releases from one operator instruction, because the channel can advance between the first host and the last. Recording the version rather than the digest has the same failure in a narrower form: a tag can be moved, and release immutability making that unlikely is a reason to *verify* rather than a reason not to.

The digest travels with the instruction, and `billet host-upgrade` refuses when its own resolution of the channel disagrees with it. That is the fence: a node that re-resolved the channel for itself would defeat the whole arrangement the moment the channel advanced mid-rollout.

**Convergence is proved against the manifest, not against a name.** A node's registration carries the version string its binary was built with and nothing about the bytes behind it — so for a while a host upgraded out of band, or rebuilt under the same name, converged a rollout on evidence weaker than the decision it converged. A host now records which signed manifest produced its binary, bound to the sha256 of that binary, and reports it at registration (`internal/provenance`, node wire 16). The rollout compares it against the decision it persisted.

**The binding to the bytes is what makes the record evidence.** A record naming only a version is defeated by the exact case the digest exists to catch: two builds carrying one version string, which is what a moved tag produces. So the node hashes its own executable before reporting, and a binary replaced by hand afterwards reports **nothing** rather than inheriting the last upgrade's provenance — because a rollout reads "nothing" correctly and reads a wrong digest as certainty.

**Three answers, and the middle one is why this is not a refusal.** A host naming the decided manifest is converged and proved. A host naming a DIFFERENT manifest is **blocked**: it is running the right version from bytes this decision did not name, which is billet failing to prove something rather than a host that failed. A host naming NOTHING converges on its version, and the rollout records that nothing proved it — which is the entire installed fleet on the day the field ships, including the hosts that would deliver the build able to name one. Refusing there would be a rollout that can never complete, and a weaker answer honestly recorded beats a mechanism nobody can adopt.

**What is still not covered**, and cannot be from here: a host that never runs a billet-driven upgrade and was not installed by `install.sh` — a hand-built binary, a fork — reports no manifest and converges on its version. That is the reported case rather than a gap, and `billet rollout status` names it under `PROVED BY` so an operator can see the strength of what they have.

### A restart waits for its own message session, because GitHub will not hand it over

**Measured against a real organization on 2026-08-30, not assumed.** A message session is single-holder, and a successor cannot displace one an abandoned control plane left behind: opening a second answers

```
409 Conflict ... RunnerScaleSetSessionConflictException ...
The runner scale set <name> already has an active session for owner <name>.
```

So the recovery path a restart depends on is **not** "take over"; it is "wait for the abandoned session to expire". Until this was run, `runTier` returned that error — which means a control plane killed and restarted, *which is every upgrade, every crash, and every `systemctl restart`*, failed to start and took its tier's listener with it, saying nothing an operator could act on. `server.openSession` now waits and says what it is waiting for.

Nothing is lost in that window. GitHub queues a job for 24 hours when no runner is available, and compute already running is held by its node and re-adopted through `ServiceableRunnerLeaseIDs`; what the wait costs is scheduling latency, which is the trade ADR-001 already makes by choosing recovery in minutes over HA. How long GitHub takes to expire an abandoned session is bracketed rather than known: seven runs on 2026-09-04 found a successor under the original owner still refused at 60 seconds and open at 91 or 92, and the instant in between is not observed, so `openSession` waits in a loop rather than sleeping for a number.

This is exactly why `internal/integration/sessionreplacement_test.go` exists, and it found the defect on its first real run.

### A dispatched instruction is acknowledged, not assumed

The node does not run the upgrade: it execs a detached `billet host-upgrade`, because the updater's job is to stop the service that started it. Returning as soon as that process starts makes every refusal it can make — a disagreeing digest, an incompatible candidate, a superseded generation, a claim already held — invisible to the control plane, which records the host as draining and waits forever. Nothing corrects it: the host keeps running, stays live, reports the same release, and no registration ever contradicts the belief.

So the updater answers one line on an inherited descriptor at the moment its claim and journal become durable — the last point at which it has touched nothing — and the node waits for that and nothing else. EOF with no answer is a refusal, because that is exactly the case a silent spawn hid. What comes after the answer (an archive download, then an unbounded drain) is never waited for.

The active claim bounds concurrency and cannot fence a generation: it is released as soon as an upgrade commits or completes a rollback, so a delayed instruction from a replaced decision arriving a second later finds nothing in its way. A durable high-water mark of the newest decision this machine has acted on is what refuses it, written before the transaction starts because the window it protects is the whole unbounded length of that transaction.

### Compatibility is decided before anything stops

The manifest carries the node-wire range the release speaks, the ledger schema it expects, the guest contract its images must satisfy, and the release it can be rolled back to. All four are read out of the packages that define them at build time, never typed into YAML.

This is the part that has to happen while the deployment is still running. A candidate that shares no wire version with the fleet cannot be discovered after the switch: by then the old binary is hidden and both services are down, and every node's registration is refused with `ErrRefused` — which is not something a node retries. Same for a ledger schema behind the installed one: the binary starts, refuses its own database, and the control plane is down with nothing installed that can read it.

### A phase, not a position in a function

Where each component has got to is durable. Every step is irreversible in a different way and the recovery for each differs, so a script that fails partway leaves a machine in a state only the person reading the script can classify.

Two edges in the state machine carry the safety, and both are asserted structurally over the transition table rather than left to the code that walks it:

- **`installing` is reachable only from `ready_to_install`.** That phase records that there are no active workload obligations, which is the proof authorising the replacement. Without the edge constraint, an installer could check and act — checking on one side of a restart and acting on the other.
- **`committed` is reachable only from `verifying`.** The only thing separating "the binary is in place" from "the component works" is the verification.

### Elapsed time never authorises a teardown

`draining` is unbounded and nothing in the rollout can give it a deadline. A maintenance window bounds when a rollout may *begin* and cannot stop one; a window that could interrupt a drain would be a clock authorising exactly the teardown the rest of this refuses.

This is the same rule `internal/node`'s custody already holds, extended to the fleet: time warns, it does not authorise.

### Completion is convergence or an explicit decision

A rollout completes when every required component reports the target, **or** an operator recorded an exemption or a decommission. "Most nodes updated" is not success, and neither is "nothing left to do" — an exempted host is still running the old release and still holding its protocol open.

Exemption and decommission are separate for that reason. One says "this host is not part of this rollout"; the other says "this host is gone". Collapsing them lets a rollout complete while a live machine keeps an old protocol open with nothing recording that anybody decided so.

### The host transaction is a journal, and its order is the safety content

`billet host-upgrade` runs outside the process it replaces. The ordering is fixed and each position is chosen by what would go wrong if it moved:

1. Everything that can fail without consequence — resolving, verifying, downloading — happens before anything stops.
2. The node stops before the server, so compute drains while the control plane can still record what happened to it.
3. The binary is hidden before the ledger is touched, so no operator command can enter through either version mid-swap.
4. The ledger is snapshotted before it is migrated. A migration is the one step putting the old binary back cannot undo.
5. Readiness is *proved*, under probe units that poll nothing and accept no workload. Process liveness is not readiness.
6. The commit record is written last, and after it nothing may restore — the fence may already have opened and admitted writes against the new ledger.

A rollback unwinds exactly what the journal says was reached, ledger first for the reason in (4). **An unprovable rollback is its own outcome**: the host is cordoned with its journal intact, because nothing about it is then known and reporting it as rolled back would have the rollout return it to service.

### The node is told, and does not decide

`CommandUpgrade` carries an exact version and a fenced rollout generation — never a channel, because a node resolving one itself reintroduces the split fleet. The node starts the updater detached and reports only that it started: a node executes commands one at a time and each command's timeout starts when it is queued, so an inline upgrade would hold the single slot for the length of the drain and every other command to that host would expire behind it, including the destroys that let the drain finish.

Wire version 14 adds the command without moving `MinVersion`. A 12- or 13-speaking node is never sent one and keeps working exactly as before, which is what makes those versions ones a 14 control plane genuinely speaks rather than merely tolerates.

## Consequences

**A mixed-version fleet is normal, not a failure state.** The node wire is a negotiated range and a rollout is server-first; `billet status` under `protocol` is what says when an old version may be dropped.

**A fleet can be left mid-rollout indefinitely**, by an unreachable host or a cordoned one, and that is the correct behaviour rather than a stall. Both are visible and both have a named operator decision that resolves them.

**Two implementations of the host ordering exist for now.** The Ansible `host` role has ~4,200 lines of tested transaction logic, and `billet host-upgrade` implements the same ordering in Go for the path a rollout drives. Consolidating them means running the role's test suite against the Go command first; replacing a proven implementation with an unproven one to remove a duplicate is the wrong trade, and the duplicate is recorded here rather than left to be discovered.

**One thing is no longer assumed.** Whether GitHub redelivers an unacknowledged message to a new session after a controller restart was undocumented and unmeasured until `TestLiveSessionReplacement` ran against a real organization on 2026-09-04. It does: a session holding one `JobAssigned` message was abandoned without being closed, and the successor was handed the same message id back, with the same one assigned job. A successor cannot open while the abandoned session is outstanding, under the old holder's own name or under another host's, so the wait is the recovery path either way; the abandoned session was still refused at 60 seconds on each of seven runs and open at 91 on three of them and 92 on the other four, which brackets the expiry rather than naming it. Every recovery path stays written to be safe whether or not a message comes back, because idempotence is what makes a redelivery harmless rather than what makes it necessary, and `TestARedeliveredAssignmentDoesNotStartASecondRunner` pins that a redelivered exchange after a restart starts no second runner. The crash-point suite still asserts what the *ledger* holds rather than what GitHub does.

## Alternatives considered

**A per-host desired version.** Rejected: it makes an upgrade a dozen decisions that can each be made differently, which is the shape the whole issue exists to remove.

**Nodes following the channel themselves.** Rejected: the channel advances between the first host and the last, so one operator instruction produces a fleet on two releases.

**Bounding the drain.** Rejected, again — the no-timeout drain removed the last timer that could destroy running work, and reintroducing one under a different name is the same defect.

**Letting the controller replace its own binary.** Impossible rather than rejected: nothing survives to finish the transaction.
