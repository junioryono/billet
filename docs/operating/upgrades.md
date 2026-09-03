# Upgrading billet

Updating billet is one durable decision that converges the whole deployment: the controller, every node, and the guest images they boot. This is what that looks like from an operator's chair — including the parts that go wrong.

The rule underneath all of it: **no ordinary update terminates a running job.** Jobs may run for days. A timeout may stop *you* waiting, and it never stops the work. The only thing in billet that ends a running job is `billet force-destroy`, which is a separate command, requires an explicit confirmation, and tells you exactly whose builds it is about to fail.

## The normal case

```
billet rollout start
billet rollout status
```

`start` resolves the signed `stable` channel to one immutable release, records its manifest digest as the target, and lists every registered host. `status` says where each one has got to. Nothing else is required: the control plane's rollout coordinator converges the fleet against that target, one host at a time by default.

**The controller goes first, and billet observes that half rather than recording it about itself.** A process cannot install its own successor, so the control plane's own upgrade is `billet host-upgrade` on its host. Under `release.automatic` the coordinator starts that program itself, exactly as a node does when it is told to upgrade — the same detached updater, journal, claim and decision fence, retried on a ten-minute backoff if the updater refuses — and the successor that comes up is what observes the result; on a PostgreSQL ledger the transactional upgrade is refused, so there the controller's host is still yours to run `billet host-upgrade` on, and the server says so at startup. Without `automatic`, running it is you, or whatever automation you already have. Either way the coordinator only *notices* when the running binary is the target, records it, and only then begins telling nodes. That ordering is not a preference: the node wire's bridge runs one way, so a node rolled ahead of its control plane is refused and stays refused.

A rollout is a decision, not a script. Run `start` twice and you get the rollout that is already running, not a second one. A control-plane restart resumes the same rollout against the same digest. If the channel advances while a rollout is underway, the rollout does not move — it is pinned to the digest it resolved, which is the whole reason the digest is what gets persisted.

To install an exact release instead, and follow nothing:

```
billet rollout start --version v0.4.0
```

## What a host actually does

Each host walks the same phases, and `billet rollout status` names them:

| Phase | What it means |
|---|---|
| `pending` | Not started. A host billet cannot reach stays here. |
| `draining` | Taking no new work, waiting for what it has. **Unbounded.** |
| `ready_to_install` | No active workload obligations. |
| `installing` | The binary is being replaced. |
| `verifying` | The replacement is proving it works. |
| `committed` | Running the target, healthy. Done. |
| `rolling_back` | Failed; restoring the previous release. |
| `rolled_back` | Healthy on its previous release. Can be retried. |
| `blocked` | Cordoned. Nothing can safely act on it. |
| `exempt` / `decommissioned` | You made a decision about it. |

`draining` has no timeout, and there is no setting that gives it one. A node with a six-hour job on it sits in `draining` for six hours, then installs. That is the design, not a stall.

A host is recorded as `committed` only while billet is **in contact with it** and it reports the target. `Release` is what a host said at its *last* registration, so a node that came up on the target, failed and disappeared reports the target forever; reading convergence off that alone marks a dead machine as done, and if it was the last one outstanding, closes the rollout over an offline fleet.

Each phase is its own transaction, so a control plane that restarts partway through a host's sequence resumes from the phase it finds rather than replaying from `draining` — which the state machine would refuse, wedging that host on every pass from then on.

`installing` is reachable only from `ready_to_install`, and `committed` only from `verifying`. Those two constraints are asserted structurally in the code, because they are what stop a binary being replaced under a running job and a host being recorded as converged without anything having checked it.

## When a host fails

A failed candidate rolls back automatically: the ledger snapshot goes back first (the old binary refuses a schema it has never heard of, so restoring the binary first produces a control plane that will not start), then the binary, units and configuration, then the services come back and are proved to stay up.

That leaves the host in `rolled_back` — healthy on its old release, and retryable:

```
billet rollout retry <node>
```

**How billet knows a rollback happened is worth stating, because it looks like it should be obvious and is not.** The host runs its own transaction; the control plane sees only the instruction going out and registrations coming back. A host that has not started yet and a host that installed the target, failed and restored its previous release are identical in every field the control plane has: both live, both reporting the old release. What separates them is the **registration epoch** — a fencing token that a registration bumps and nothing else does — so the rollout records the epoch the instruction was sent against and treats a *higher* one on the old release as proof that the host came back. Without that a rolled-back host sits in `draining` forever, occupying the cohort's only slot and never counting against the failure budget, which stops the whole rollout with nothing anywhere saying why.

That evidence is the strongest available and it is not perfect: a host restarted mid-drain by something *other* than the upgrade looks the same. The realistic case is the control plane restarting and forgetting a node that is three hours into a drain, at which point the node re-registers on its old release to stay reachable — exactly the shape of a rollback. The ambiguity is not resolvable from outside the host, so billet does not try to resolve it: it records what it saw, and **corrects itself if the host later reports the target**, which settles the question the inference could only guess at. Nothing is destroyed either way; the cost of a misreading is one spent unit of failure budget.

That correction applies to `rolled_back` and deliberately not to `blocked`. `rolled_back` is a phase the state machine says a component may leave; `blocked` exists because billet could not prove something, and only a person can supply what it could not.

**What "converged" proves, exactly.** A host is recorded as committed when it is live, reports the target **version**, and does not contradict the target **manifest**.

A host records which signed manifest produced its binary — written by `billet host-upgrade` when it commits, and by `billet release record` when `install.sh` installs — together with the sha256 of that binary.

**The installer does not attest on its own, and that is deliberate.** It verifies its download against a checksums file fetched from the same place as the manifest, so a manifest served beside a different archive would otherwise produce a host that converges a rollout as *proved* on bytes the manifest never named. `billet release record` checks every link of the chain rather than assuming any of it: the manifest parses under the same reader every other path uses, it names an artifact for this platform, the archive is the size and the hash that entry declares, and the binary being recorded is the one inside that archive — a single regular member called `billet`, never a link, which would resolve to whatever the extracting side already had. If any link fails the install still succeeds and the host simply reports nothing. At registration the node hashes its own executable and reports the manifest only if it still matches, so a binary replaced by hand reports nothing rather than the last upgrade's provenance.

`billet rollout status` shows what each host proved under `PROVED BY`:

| | |
|---|---|
| `manifest <digest>` | It named the manifest this rollout decided on. |
| `version only` | It reached the target version and could not say which bytes it installed. Every host does this until one billet-driven upgrade has run. |
| `-` | It has not converged. |

A host that names a **different** manifest is **blocked**, not converged: it is running the right version from bytes this decision did not name. Repairing it takes **two** steps, and the blocker says both:

```
billet host-upgrade --version <target> --manifest-sha256 <digest> --reinstall   # on that machine
billet rollout retry <node>                                                     # here
```

The second is not optional. `blocked` exists because billet could not prove something, and nothing automatic leaves it — a repaired host reaching the coordinator is refused by the phase machine, correctly, because only a person can supply what billet could not. `billet rollout exempt <node>` is the other way out, if the answer is that this host is not part of this rollout.

`--reinstall` is what makes the first step work whatever state the host's record is in. Without it, the "you are already on this version, nothing to do" shortcut also asks whether the installed manifest **disagrees** — a host whose record names something else is reinstalled, and a host that cannot say is left alone, because reinstalling on "cannot tell" would stop services and drain compute across a fleet to fix a diagnostic. But a record that has since been damaged answers "cannot tell" while the control plane still holds the disagreeing digest it reported earlier, so the host stays blocked while the command it was told to run decides there is nothing to do. `--reinstall` is a person asserting rather than billet inferring.

`billet status` reports the same thing per host beside the release, and distinguishes a host whose protocol cannot carry the answer from one that simply has no record.

**What this cannot prove**: a host that never runs a billet-driven upgrade and was not installed by `install.sh` — a hand-built binary — reports nothing and converges on its version. That is visible rather than hidden, which is the point.

A spent failure budget stops billet **starting** on more hosts. It does not stop it recording what the hosts already underway have done — otherwise one failed host would leave a fleet unable to finish updating the ones that had already succeeded.

**If the rollback itself could not be proved, the host is `blocked` instead, and that is a different situation.** Nothing about it is known: it may be on either release, its ledger may be either schema, and its compute may or may not exist. Its recovery journal is left in `/var/lib/billet/upgrades/` and the claim pointer is deliberately *not* released, so nothing starts a second transaction on top of the first. Go and look at the journal before doing anything else on that machine.

## When an updater refuses

A node does not run the upgrade itself: it execs a detached `billet host-upgrade` that outlives it, because the updater's whole job is to stop the service that started it. That means everything the updater refuses — a digest that disagrees with the rollout's decision, a candidate incompatible with this deployment, an instruction from a rollout the fleet has moved past, a claim another upgrade already holds — happens *after* the node has let go.

So the updater answers on an inherited descriptor before it touches anything, and the node waits for that one line. An updater that accepted has taken the job and everything from there is in its recovery journal; one that refused says why, and the node reports it, so the rollout backs off and retries rather than recording a host as draining that never heard anything. **A host that goes quiet after a refusal it could not report is the failure this prevents**: it keeps running, stays live, reports the same release, and nothing ever contradicts the rollout's belief that it is draining.

The wait is 90 seconds and bounds only the preflight. The download and the drain come after the answer, and neither is waited for. The node reads one line and stops there rather than waiting for the channel to close, because an updater that answers and carries on should not be reported as refusing ninety seconds later.

**Only one upgrade runs on a host at a time, and the claim is not what guarantees it.** The claim is a durable pointer that survives a crash — that is its whole job, because a machine that lost power mid-upgrade must still be able to find what it was running. Precisely because it survives, its presence cannot mean "somebody is working on this right now": `--resume` exists to pick up a claim whose owner is gone. A separate lock, dropped by the kernel when its holder dies, is what makes `--resume` refuse while a live updater is still going and what stops two resumes entering the same transaction. If you see `an upgrade is already running on this machine`, that is what it means; the wait it describes has no bound, because the other transaction may be draining somebody's job.

**A transaction interrupted before its generation was recorded records it on the way back in.** The claim and journal are published before the fence is raised, deliberately, so a crash always leaves something resumable — which means a crash in that window leaves a resumable transaction the fence has never heard of. A resume settles that first. If the fleet has moved past it *and* it never got past claiming, it is abandoned rather than finished, because finishing would install a release the deployment has left behind; anything further along is finished, because walking away can leave the host down.

Only `claimed` counts as "touched nothing", and the reason is worth knowing if you are reading a journal: **a step records what completed, so the work after it is already in flight.** Preserving the installed binary, stopping the node, stopping the server and hiding the binary all run before `stopped` is written, so a journal sitting at `staged` may describe a host with both services down and no billet on the path. The ambiguity is resolved toward finishing: a superseded release on one host is a rollout that dispatches again, while a host left stopped is one somebody has to go and find.

**If an updater claims the machine and then cannot write its recovery journal** — a full disk, or power lost in that window — nothing was staged, stopped or fenced, because the journal is written before any of that. `billet host-upgrade --resume` recognises that state, releases the claim, and says so; without it the host would be stuck between a `start` that refuses because a claim exists and a `--resume` with nothing to continue.

**A PostgreSQL ledger refuses the transactional upgrade outright, and that is deliberate.** The transaction works by snapshotting the state directory, doing the work, and putting the ledger back if anything fails — which an external ledger does not have. Rather than run a transaction whose rollback covers only half the deployment, `billet host-upgrade` and `billet check --maintenance-probe` refuse on that backend and say so. Upgrade such a controller with the service stopped; the fleet re-registers, and the ledger was never on that machine. See [PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md).

## When an instruction is superseded

`billet host-upgrade` records the newest fleet decision it has acted on under `/var/lib/billet/upgrades/`, and refuses an instruction from an older one. The active claim cannot do this job: it is released the moment an upgrade commits or completes a rollback, so a delayed instruction arriving a second later finds nothing in its way and would install the release the operator moved away from. A redelivery of the *same* decision is allowed — instructions are retried, and refusing that would turn ordinary unreliability into a host no rollout can move. An operator's own run carries no generation and is never fenced.

The mark is raised even when there is nothing to install: finding the release already present is still acting on a decision, and accepting without raising it lets a delayed older instruction downgrade the machine. It is also read again *under* the claim, because the first read is of a file nothing was holding still.

## After a restart, a control plane waits for its own session

A message session is single-holder, and GitHub does not let a successor displace one an abandoned control plane left behind — measured against a real organization, not assumed. So a control plane that was killed rather than stopped cleanly finds its own session still outstanding and **waits for GitHub to expire it**, logging:

```
this scale set still has the message session an earlier control plane left behind,
so this one cannot open its own yet; waiting for GitHub to expire it.
Queued jobs are not lost while this waits
```

That is not an error and there is nothing to do about it. GitHub queues a job for 24 hours when no runner is available, and compute already running is held by its node and re-adopted, so the wait costs scheduling latency and nothing else. A clean stop closes the session and skips this entirely.

**`billet host-upgrade --status` says what a machine is holding** — whether a transaction is running right now, what its journal says and which process claimed it, the newest fleet decision this host has acted on, and which manifest produced the binary that is here. It reports and changes nothing.

**An updater that hangs before answering leaves a process behind.** After ninety seconds the node reports a refusal, but the process and the goroutine waiting on it remain — killing it is not safe, because it may already be installing. What bounds it is that the transaction lock is taken before anything else, including the network: the one stuck process holds it, so every retry is refused immediately and at most one uncertain updater exists per host. If `billet rollout status` shows a host draining with a rising attempt count, look at `/var/lib/billet/upgrades/` on that machine.

**A mark billet cannot read refuses a fenced instruction.** "This machine has never taken a fenced upgrade" and "this machine's fence is not working" are different facts, and answering zero for both hands a stale instruction permission to overwrite the only evidence that a newer decision exists. The message names the file; an operator's own run is unaffected, which is what stops this wedging a host.

## When a host is unreachable

It stays `pending`, and it holds the rollout open. That is deliberate: a host billet cannot reach is not a host that is gone. Its compute may be running, and it will come back speaking whatever protocol it spoke before.

If it really is gone — the machine was decommissioned, the disk failed, it is never coming back — say so:

```
billet nodes decommission <node>
```

This refuses while the host still holds any lease — that one is not overridable, because a decommissioned host is excluded from what every tier's floor believes is already met while its capacity stays charged either way. `billet leases release --force` is how an operator settles a lease for a machine that is never coming back.

Reachability is different. It refuses a host billet can still talk to, and `--force` overrides that — but the exclusion is then recorded as **UNPROVEN**, permanently, and every later drain and `billet status` say so rather than reporting the fleet clear. That is the difference between billet knowing a machine is idle and an operator asserting it.

`billet rollout decommission <node>` does the same thing and additionally records the decision against the rollout, so a host nothing can reach stops holding it open. It tolerates one case the fleet-level command refuses: a name in the rollout with **no fleet row at all**, which is a machine something already removed — refusing that would leave a rollout nothing could ever resolve.

To record the decision inside the rollout as well:

```
billet rollout decommission <node> --reason "hardware failure, disks destroyed"
```

## When you want a host skipped, not removed

```
billet rollout exempt <node> --reason "held on v0.3 for the customer pilot"
```

An exemption is not a decommission. The machine is still there, still running, still on its old release — which means it is still holding open whichever node-wire protocol it speaks. `billet status` under `protocol` shows that. Collapsing the two would let a rollout report success while a live host kept an old protocol open with nothing recording that anybody decided so.

## Pausing and abandoning

```
billet rollout abort --reason "the candidate has a cache regression"
```

An abort ends the **decision**, not the machines. Hosts that already converged stay on the new release; the rest stay on the old one. Nothing is reverted — a command that also reverted hosts would be a second, undeclared rollout in the opposite direction, and it would do it to machines running jobs.

After an abort your fleet is on two versions. That is fine and it is visible: `billet status` under `protocol` says which is which, and the node wire is a negotiated range precisely so a mixed fleet keeps working.

## Upgrading one machine by hand

```
billet host-upgrade
billet host-upgrade --version v0.4.0
billet host-upgrade --resume
```

This is the same transaction a rollout drives, run directly. `--resume` continues or unwinds whatever is already on the machine, which is how an interrupted upgrade recovers — including one interrupted by the machine losing power.

It is a separate program on purpose. A control plane cannot install its own successor: the moment it stops, whatever was going to finish the job has stopped too.

## Ending a running job on purpose

Sometimes you genuinely need the compute gone — a runaway job, a host you must physically remove now, a wedged fleet. This is the only operation that does it:

```
billet drain --reason "emergency maintenance"
billet force-destroy --yes --reason "runaway job on epyc-1, disk full"
```

The drain first is required, not advisory: the command enumerates what it would destroy, shows it to you, and acts on your answer, so admission has to be closed across all three or a job accepted in between gets destroyed without ever appearing in the list you approved.

Without `--yes` it only reports. What it reports is every affected lease, its tier, its host, its GitHub run id and how long it has been running — because "7 leases" tells you nothing about whether to proceed.

**GitHub does not requeue a job whose runner vanishes after it has started.** Every build in that list fails and stays failed. That is the whole reason this is a separate command with a separate name, refuses without a reason, and cannot be reached by any timeout, signal, failed rollback or lost leadership.

Leases a *node* holds — `custody`, `teardown`, `quarantine` — are reported and not touched. Their compute is a node's proof obligation, and `billet leases release --force` is what resolves one, through the holder rather than underneath it.

## Configuration

```yaml
release:
  channel: stable        # or candidate
  automatic: false       # let the control plane start a rollout by itself
  maintenance_window:    # when an automatic rollout may BEGIN, UTC
    start: "02:00"
    end: "04:00"
```

Or pin, and follow nothing:

```yaml
release:
  version: v0.4.0
```

Setting both is an error rather than a precedence rule. A deployment that pinned a version *and* named a channel said two things, and guessing which you meant is how a deployment that believes itself pinned quietly follows a pointer.

`automatic` is off by default. An automatic rollout drains hosts and replaces binaries; a deployment that had not decided to allow that should not begin doing it because it upgraded to a billet that could.

With it on, the control plane asks the channel every ten minutes and, inside the window, records a rollout whenever the channel names a release that this control plane or any registered host is not on — the same decision `billet rollout start` records, with the same compatibility check in front of it, created by `release.automatic` so `billet rollout status` says who started it. It then upgrades its own host first, as above, and converges the nodes. A channel that will not resolve, a target the deployment cannot speak to, or a closed window each leave a line in the log and start nothing.

A maintenance window bounds when a rollout may **begin**. It never stops one — a window that could interrupt a rollout would be a clock authorising a teardown, which is the thing this whole area refuses. It is UTC because a fleet spans machines whose local time is not one thing, and because a local window is unstable across a DST transition in exactly the quiet hour you picked.

## The consumer paths

**The installer** follows the signed channel:

```
curl -fsSL .../install.sh | sh                       # stable
BILLET_CHANNEL=candidate curl -fsSL .../install.sh | sh
BILLET_VERSION=v0.4.0 curl -fsSL .../install.sh | sh  # exact, consults no channel
```

If it cannot read the channel it falls back to GitHub's `releases/latest` and says so. Refusing to install at all over a pointer would be worse on a machine that has no billet yet and so no way to be told why; the integrity check is the checksum, which is unchanged either way.

**The Ansible role** takes `billet_release_channel`, and it is opt-in against the role's default. The role's own rule is that a converge must be deterministic, and that rule is right: with a channel set, the same playbook installs different binaries on different days and does it through a real drain and restart. Prefer a rollout, which resolves once so every host converges on the same digest.

**The Actions** are versioned by the ref you write. `@v0` moves with each accepted release; `@v0.4.1` and a commit SHA never move. See [Action versioning](../reference/action-versioning.md) — the tradeoff is real and stated there rather than hidden.

**Terraform** does not install billet. The module provisions AWS infrastructure — VPC, IAM, the control-plane instance, the cache bucket — and the binary arrives through the installer or Ansible on the machines it created. There is no channel for the module to follow, and a `?ref=` in your own repository is resolved by `terraform init` on your machine and cannot update itself. That is a limit, stated, rather than a feature nobody implemented.

## What is proved and what is assumed

Worth knowing before you rely on any of it.

**Proved.** That the coordinator will not tell a node to move before the control plane is on the target, that it disturbs no more hosts at once than the cohort allows, that it leaves an unreachable host alone, and that it blocks — once — a host whose wire is too old to receive the command. The upgrade ORDERING, against a fake that records what it was asked to do — including that the ledger is snapshotted before it is migrated and the fence opens only after the commit record. The rollout state machine's two load-bearing edges. What a control-plane crash leaves in the ledger at each boundary of a job's life. That a manifest billet publishes is one billet accepts. That corrupt, unsigned, expired, replayed, wrong-platform and incompatible metadata are each refused before anything is replaced.

**Not proved, and stated so rather than implied.** No part of `cmd/billet/hostupgrade.go` has run against a real host — every method of it stops a service, replaces a binary or migrates a database, and what is tested is the sequence they are called in, not the doing. And billet does not yet know whether GitHub redelivers an unacknowledged message to a **new** session after a controller restart. The vendored client documents redelivery *within* a session and says nothing about one deleted and recreated. `TestLiveSessionReplacement` runs against a real organization and records what it meets (so far, the refusal to hand a session over); every recovery path here is written to be safe whether or not a message comes back, and none of them assumes one does.
