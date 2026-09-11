# Upgrading billet

Updating billet is one durable decision that converges the whole deployment: the controller, every node, and the guest images they boot. This is what that looks like from an operator's chair — including the parts that go wrong.

The rule underneath all of it: **no ordinary update terminates a running job.** Jobs may run for days. A timeout may stop *you* waiting, and it never stops the work. The only thing in billet that ends a running job is `billet force-destroy`, which is a separate command, requires an explicit confirmation, and tells you exactly whose builds it is about to fail.

## The normal case

Nothing. A deployment that says nothing in `release:` follows the signed `stable` channel and updates itself: within an hour of the channel advancing, the control plane records a rollout to that release, the scheduled updater on each controller host upgrades the controller, the coordinator converges every node, and the daily image refresh on each node takes up the guest image published for it. `billet rollout status` says where it has got to, and `billet status` reports every host's release beside its protocol.

Three things make that safe to leave alone. A rollout drains every host for as long as its work takes and never ends a job. A candidate is verified before it is committed and rolled back when it is not. And the ledger refuses to be served by a release older than the newest that has served it, so an unattended update cannot go backwards (see [Downgrades](#downgrades)).

To turn it off:

```yaml
release:
  automatic: false
```

With that, the channel is still followed but nothing acts on it: `billet rollout start` records the decision, and the updaters on the hosts leave a recorded rollout to an operator. To start one by hand, on a deployment with automatic updates on or off:

```
billet rollout start
billet rollout status
```

`start` resolves the signed `stable` channel to one immutable release, records its manifest digest as the target, and lists every registered host. `status` says where each one has got to. The control plane's rollout coordinator converges the fleet against that target, one host at a time by default. An automatic start does exactly what `start` does, through the same resolution and the same compatibility preflight, and records itself as `automatic (stable channel)`.

**The controller goes first, and a separate root process performs that half.** A process cannot install its own successor, and the control plane runs unprivileged, so on every host with a control plane the package enables `billet-upgrade.timer`, which every five minutes runs `billet host-upgrade --from-rollout`: it reads the rollout the ledger records and, if the target is a release this host is not running, runs the transaction below with the rollout's own digest and generation. On a Mac the same thing is the `sh.billet.upgrade` launch agent `billet local up` installs. It acts only on a decision the ledger holds and never on the channel, so `automatic: false` stops it too. What the coordinator does is *notice* when the running binary is the target, record it, and only then begin telling nodes. That ordering is not a preference: the node wire's bridge runs one way, so a node rolled ahead of its control plane is refused and stays refused.

**A systemd node starts its updater as a transient root unit of its own**, `billet-host-upgrade-<rollout>-g<generation>`, for the same reason the controller uses a root timer: the node's children inherit its read-only sandbox and are killed when its unit stops. Find the updater with `systemctl list-units 'billet-host-upgrade-*'` and read its output with `journalctl -u billet-host-upgrade-<rollout>-g<generation>`. The node waits only for the updater's answer over a Unix socket in its state directory, never for the oneshot's start job. On launchd the updater remains a detached `setsid` child and uses the same socket. On a host running both roles with a PostgreSQL ledger, the node forwards the configured `server.state.postgres.dsn_env` variable by name through `systemd-run --setenv=NAME`; the value travels over the service manager's bus and never appears in argv. The node also forwards `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN` by name when present, preserving the credentials a combined-role host's candidate server probe needs to read its App key from AWS SSM.

**Systemd nodes on releases through v0.9.2 need one manual move onto the release carrying this fix.** Run `billet host-upgrade --version <tag> --manifest-sha256 <digest>` as root outside the node service, or converge the host role once with `billet_version` pinned. The old binary dispatches the upgrade, so targeting a fixed release alone cannot repair that launch path. On 2026-09-06 the reference node refused at `/var/lib/billet/upgrades/transaction.lock: read-only file system`; it kept serving while the rollout retried ([Host rehearsals](../reference/records/host-rehearsals.md)).

A rollout is a decision, not a script. Run `start` twice and you get the rollout that is already running, not a second one. A control-plane restart resumes the same rollout against the same digest. If the channel advances while a rollout is underway, the rollout does not move — it is pinned to the digest it resolved, which is the whole reason the digest is what gets persisted, and the automatic starter starts nothing over an open one. The one exception is a rollout the fleet has been moved past: when the control plane runs a release newer than an open rollout's target (an operator moved it by hand while the rollout waited), that rollout can never end forwards, so the starter finishes it as aborted with the reason `superseded` on its record and the next tick starts the channel's target.

**What an automatic start refuses.** It never downgrades: a channel names only immutable releases and its publisher refuses to move it backwards, and a pin older than the running release is a decision `billet rollout start --version <tag> --allow-downgrade` makes by name. Before v0.9.2 that refusal, `billet host-upgrade`'s own and the ledger's watermark all held only for a development build: a release binary reported its version as `0.9.1` and every comparison against a channel's `v0.9.1` was "could not tell", so a stale rollout to v0.9.0 downgraded a control plane an operator had just moved to v0.9.1 (2026-09-05). From v0.9.2 a release reports itself as its tag and the two spellings order against each other. It never restarts a target an operator aborted: `billet rollout abort` records a reason against exactly those bytes, and the channel moving on to a different release is what ends that. It never starts over a fleet that already runs the target, and a host that reports no release counts as not on it. Each refusal is logged once every six hours rather than every tick.

To install an exact release instead, and follow nothing:

```
billet rollout start --version v0.4.0
```

**A maintenance window bounds when an automatic rollout may begin**, in UTC, and never stops one:

```yaml
release:
  maintenance_window:
    start: "02:00"
    end: "04:00"
```

A node checks at startup that its absolute `node.state_dir` path leaves room for the generated socket name: the whole Unix socket pathname must fit in 107 bytes on Linux or 103 bytes on macOS. With the current 39-byte suffix and separator, that leaves 68 or 64 bytes for the state directory. A longer path is refused with a diagnostic naming the limit before the node starts or its upgrade probe succeeds.

## Releases before v0.6.1 cannot verify a release

Every release manifest and channel statement billet publishes is signed by `release.yml` running on `main`, because the cut button calls that workflow from `main`. Binaries before v0.6.1 carry a policy that accepted only a release-branch or tag identity, so on them `billet rollout start`, the automatic starter and `billet host-upgrade --version` refuse every manifest with `the manifest's signature does not satisfy this source's policy`. A fleet on v0.5.0 or v0.6.0 is moved once by the package or the Ansible host role, which install from the release's checksums rather than its manifest; from v0.6.1 it can verify manifests, with the later host-upgrade limitations recorded above. The first rollout rehearsal found this on 2026-09-04 (UTC), in its first step.

## A value-scoped builder grant strands a build in flight

`billet ami build` tags its builder instance with an owner value that now carries the deployment id, and a per-deployment IAM policy (`billet init iam --deployment <id> --builder --payload-bucket <bucket>`, passed to `fleet-ec2` as `iam_policy_json`) admits only that form. A builder tagged the old way carries no id, so the narrowed policy denies every action on it: measured with `iam:SimulateCustomPolicy`, `CreateImage`, `TerminateInstances` and `GetConsoleOutput` all come back `implicitDeny`.

That matters only across the upgrade, and only for a build that is running while you replace the policy. Its instance cannot be imaged, terminated or read by the role that started it, so it runs to its own timeout and you clean it up by hand.

**Let any build finish before you replace a value-scoped policy**, then upgrade the binary, then the policy. Account-wide deployments (`--account-wide`, and every `fleet-ec2` rendering with no deployment id, which is what the module produces at apply time) are unaffected: their pattern matches both forms.

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

A host whose dispatch keeps being refused shows why under `DETAIL` (`last dispatch refused: ...`), from a column the coordinator writes on every refused dispatch and clears when a dispatch is accepted or the host converges, so it always means the refusal the host is still stuck on and never a history; a blocker, an exemption or a rollback result takes the column's place, because each says more about where the host is. `billet rollout status --json` prints the same facts as a document a machine can read, with the ledger's deployment binding and every registered host's current registration epoch and incarnation beside them, through a read-only open that changes nothing on the host.

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

A node does not run the upgrade itself: it starts a separate `billet host-upgrade` that outlives it, because the updater's whole job is to stop the service that started it. That means everything the updater refuses — a digest that disagrees with the rollout's decision, a candidate incompatible with this deployment, an instruction from a rollout the fleet has moved past, a claim another upgrade already holds — happens *after* the node has let go.

So the updater answers over a Unix socket in the node's private state directory before it touches anything, and the node waits for that one line. An updater that accepted has taken the job and everything from there is in its recovery journal; one that refused says why, and the node reports it, so the rollout backs off and retries rather than recording a host as draining that never heard anything. **A host that goes quiet after a refusal it could not report is the failure this prevents**: it keeps running, stays live, reports the same release, and nothing ever contradicts the rollout's belief that it is draining.

Instruction-validation, config-load and conflicting-flag errors are sent over the socket immediately after flags have parsed. The node allows 90 seconds for a connection and a further 90 seconds for the connected updater to finish its one-line answer; both bounds cover only the preflight. The download and the drain come after the answer, and neither is waited for. The node reads one line and stops there rather than waiting for the channel to close, because an updater that answers and carries on should not be reported as refusing ninety seconds later.

**Only one upgrade runs on a host at a time, and the claim is not what guarantees it.** The claim is a durable pointer that survives a crash — that is its whole job, because a machine that lost power mid-upgrade must still be able to find what it was running. Precisely because it survives, its presence cannot mean "somebody is working on this right now": `--resume` exists to pick up a claim whose owner is gone. A separate lock, dropped by the kernel when its holder dies, is what makes `--resume` refuse while a live updater is still going and what stops two resumes entering the same transaction. If you see `an upgrade is already running on this machine`, that is what it means; the wait it describes has no bound, because the other transaction may be draining somebody's job.

**A transaction interrupted before its generation was recorded records it on the way back in.** The claim and journal are published before the fence is raised, deliberately, so a crash always leaves something resumable — which means a crash in that window leaves a resumable transaction the fence has never heard of. A resume settles that first. If the fleet has moved past it *and* it never got past claiming, it is abandoned rather than finished, because finishing would install a release the deployment has left behind; anything further along is finished, because walking away can leave the host down.

Only `claimed` counts as "touched nothing", and the reason is worth knowing if you are reading a journal: **a step records what completed, so the work after it is already in flight.** Preserving the installed binary, stopping the node, stopping the server and hiding the binary all run before `stopped` is written, so a journal sitting at `staged` may describe a host with both services down and no billet on the path. The ambiguity is resolved toward finishing: a superseded release on one host is a rollout that dispatches again, while a host left stopped is one somebody has to go and find.

**If an updater claims the machine and then cannot write its recovery journal** — a full disk, or power lost in that window — nothing was staged, stopped or fenced, because the journal is written before any of that. `billet host-upgrade --resume` recognises that state, releases the claim, and says so; without it the host would be stuck between a `start` that refuses because a claim exists and a `--resume` with nothing to continue.

**A PostgreSQL ledger gets the transaction without its ledger steps.** billet copies no PostgreSQL database, so on that backend `billet host-upgrade` fences nothing, snapshots nothing and migrates nothing: it preserves the binary, units and config, drains the node, stops the server, installs the candidate, proves it can open what it inherits, and starts the services; the migration happens when the candidate takes the controller claim. The rollback boundary is therefore the candidate's start, and what lies past it is your database's own backup. A deliberate downgrade lowers the release watermark through the operator handle just before the candidate is probed, since there is no snapshot to lower it after. [PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md) has the whole shape, including why every controller host runs the upgrade timer.

## When a converge holds the host

A converge from CI and a rollout's transaction must not run on one host at once: the role stops services, stages a candidate and rewrites units, and a transaction landing in the middle would stop the services the role is starting or install over the candidate it is proving. Both now share one claim, `/var/lib/billet/upgrades/active`, whose SHAPE says what holds the host: a symlink is a Go transaction's pointer to its recovery directory, a regular file is a pre-R role transaction's pointer, and a directory is a **converge guard**, holding `guard.json` with the holder, the time it was claimed, the hostname, and the release executable the converge will run with its digest. A directory was chosen because every billet before this one refuses a claim it cannot read as a symlink and none of them can release it, so an older updater dispatched onto a guarded host refuses rather than proceeding.

`billet converge-guard hold --holder <run id>` publishes the guard under the transaction lock in one durable order, and `release --holder` removes it; the role holds through `billet converge-guard prepare` (below), whose record carries a public `id`, the acquiring invocation's `token` and a `preparing` flag that `settle` clears, and releases only a guard the same invocation acquired, inside its own preparation window, when a refusal ends the preparation before anything on the host changed. Otherwise the operator releases with the holder they exported, and the preparation prints the command. A hold or a validation by the holder that already holds touches nothing but the flushes an interruption owed. Another holder's guard refuses, naming it and how long it has been held, and **nothing expires a guard**: a converge that died holding one leaves it, `billet check` warns past a day, and an operator removes it with `billet converge-guard recover --holder H --old-driver-stopped`, the assertion that the driver which held it is stopped or cannot dispatch further work and that its remote work has finished. The process table is scanned for Ansible's markers as an extra refusal (a recovery run from inside the driver it asserts is stopped is refused with "run this over a direct SSH shell"), but the assertion carries the weight: an unmarked driver, or work dispatched after the scan, is invisible to it.

**The role prepares the exclusion before anything else, through one command under one lock.** `prepare-exclusion.yml` is the host role's second task (after the refusal of a billet-managed runner) and the first pre-task of every play of the fleet playbook, so `ssh_access`, the connectors and the development role are under it too. It needs a holder, `billet_converge_guard_holder`, read from `BILLET_CONVERGE_GUARD_HOLDER` on the controller or passed with `-e` (which wins); a converge without one refuses before it looks at anything, and a dry run needs none because it holds nothing, and it is answered by the executable a converge would be answered by: the managed binary, or, for a guard directory the managed binary cannot answer for (absent after an interrupted bootstrap, or from before the guard), the executable the guard records after the same verification, so an interrupted transaction under such a guard is reported as the interrupted transaction it is and the dry run refuses what the converge would refuse of that record; a report whose record cannot be read is shown with its problem, and a claim a converge would refuse (a Go transaction, an unpublished guard, an unknown shape) keeps that shape in the dry run's facts. On Linux it establishes the upgrade root (`/var/lib/billet` 0755 root, `/var/lib/billet/upgrades` 0700 root, refusing either when it exists as anything else, and the chain above them judged first), stats `active` without following it (a symlink refuses naming `billet host-upgrade --status`; a regular file is a pre-R role transaction the role recovers before ending the play, whatever the managed binary is; anything but a directory or nothing refuses by type), and then makes TWO CALLS of `billet converge-guard prepare --holder H --json`, each answering one JSON object the role reads through one parser (`guard-answer.yml`), which refuses a malformed member by name and a refusal in the command's words. The FIRST call, `prepare --validate`, runs before anything is decided or staged: over no claim it acquires a guard recording the managed binary (`acquired`, with a public `id` and a `token` for the acquiring invocation), over this holder's guard it validates it (`validated`, completing any flushes an interruption owed), and it refuses another holder's guard, a transaction pointer that is malformed, a recorded executable whose digest moved, an unpublished directory, or a shape it does not own. The staging (`stage-candidate.yml`) then runs UNDER THE GUARD: the pin or channel resolved, the candidate copied into a recovery directory allocated by an exclusive `mkdir`. The SECOND call declares the intent and is judged under the lock: `prepare --candidate <staged>` runs the staged copy's `version` and `converge-guard prepare --dry-run --json` (the capability floor asks the preparation protocol itself, because that is what every later converge and a recovery through the recorded candidate ask of the installed binary: a candidate that answers "unknown command", or whose dry run does not report the guard this command holds, is refused there; a proved downgrade is refused unless `billet_allow_downgrade`, both spellings of a release compared), then re-binds the record to the candidate (`rebound`) or validates a record that already names it; `prepare --no-change` validates a record naming the managed binary or a committed upgrade's candidate and refuses one naming a candidate that was never installed; `prepare --recovery` validates a guard whose pointer names this converge's interrupted transaction, which the role then recovers. `settle --token` closes the acquiring invocation's window as the preparation's last task. WHICH EXECUTABLE ANSWERS: the managed binary when it answers the command; when it predates the guard ("unknown command") or is absent and a guard exists, the executable the guard records, after `guard_fallback` proved every component from `/var/lib/billet` down owned by root and writable by nobody else, the record read as its five members plus the protocol's `id`, `token` and `preparing` (typed when present, a five-member record admitted as settled and adopted by the first call), and the digest matched, never run before that; on a bootstrap (no billet, no claim) or a pre-R managed binary with a candidate, the STAGED CANDIDATE answers, after the staging, with `--expect-bootstrap` requiring the managed path positively absent and the root holding nothing but recovery directories and the lock. A pre-R managed binary with nothing to stage runs the legacy protocol (the regular-file claim) unheld, and a host with no billet and nothing to install runs unheld; both re-examine the claim and the binary at their end and refuse a guard or a binary that appeared. A later inclusion of the same run carries `--expect-id`, which the command refuses under its lock when the guard it held is gone or was replaced, and never re-acquires; a fresh process over a settled or a preparing guard validates it and settles nothing.

**A refusal inside the acquirer's window releases the guard it acquired, and nothing else does.** Every task from the first call to the settlement runs in one block whose rescue runs `billet converge-guard release --holder H --cleanup --token T` exactly when this invocation's first call answered `acquired` and no transaction pointer exists, bounded like every command of the preparation (`timeout -k 10` on Linux, the task's async bound on a Mac), then re-examines `active` and fails again with the original refusal, what the cleanup did, and the remainder observed (`none`; `none` with a release that did not answer, "observed absent, durability not proved"; a guard naming H, with `release --holder H`; an unpublished directory, with `recover --unpublished` when it holds nothing but the publication's temporary; anything else named for the operator). The command refuses a cleanup of a settled guard ("the preparation window has closed"), of a guard with a pointer, or with a wrong token, so a rerun's guard and a transaction's are never released from here; a corrupted or lost FIRST answer leaves the guard (no token is known) and the last lines name `status` and the operator's `release --holder`. THE TOKEN is printed only in the `acquired` answer, kept in a fact no report prints, and passed in the argv of the second call, `settle` and the cleanup release under `no_log`; on a default kernel other local users can read that argv, and a party holding the token can do nothing to a root-owned guard without being root, and root needs no token: its one authorisation is the cleanup release inside the acquirer's window. A host unreachable or a task invalid runs no rescue and no always section, so the guard remains and the operator's `release` is the way out. Two drivers converging one host under one holder at once are not supported and not detected: a holder names one driver, and its own reruns and inclusions are what the protocol serves. A dry run (`--check`) runs `prepare --dry-run` before any shape dispatch, which takes no lock, needs no holder and reports every shape (a legacy file and a Go transaction included) with the guard's holder, whether it is preparing or settled and whether a pointer is inside it, then continues into the read-only staging (a channel resolved and reported, a moving pin refused, nothing fetched or held).

**Under a guard every transaction entry refuses, immediately after the lock and before the network.** `billet host-upgrade`, a node's dispatched updater, `--resume` and the timer's `--from-rollout` each classify the claim first: a guarded host answers `guarded by H since T`, which a node's acknowledgement carries back as a retryable refusal (the rollout records it under `last_refusal` and asks again after its backoff, and the refusal is cleared when the host converges), and the timer exits 0 saying there is nothing to do. A hold that never returned from its publication (`active` a directory with no `guard.json`) refuses too, naming `recover --unpublished`, which removes it only when it holds nothing but the publication's own temporary.

**A role transaction interrupted under a guard is taken over, not removed.** The role publishes its pointer `active/recovery` inside the guard before its first stop, and `release` refuses while that pointer exists. A converge that died mid-transaction leaves a guard with a pointer; the next driver takes it over with `billet converge-guard hold --holder NEW --recover-from OLD --old-driver-stopped`, which re-labels the record and keeps the pointer and the recorded executable, so the role's own recovery can finish or unwind the transaction under the new holder. `billet converge-guard status --json` says what a host holds without taking the lock or creating anything, including the guard's `id`, whether it is still `preparing`, whether a stray publication temporary sits beside the record, and whether the recorded executable still has the digest the hold recorded (it is never run to find out), and never the token; `billet host-upgrade --status` prints the same shape under `claim`.

**The upgrade root is a trust boundary and every command proves it first**, through descriptors: every directory on the way to `/var/lib/billet` reached by a walk from the filesystem root, one component relative to the one above it, each owned by root or by the account running billet's transactions (root on Linux, the launch agent's account on a Mac) and writable by nobody else unless the sticky bit is set, and a link on the way admitted only when the link itself is owned by one of them; `/var/lib/billet` and the root owned by that account and writable by nobody else; `active` 0700; `transaction.lock` a regular file; none a symlink; the root opened relative to its parent and the lock relative to the root, each without following a link; and, once the lock is held, the root's name still holding the directory the lock was taken inside. A root that does not exist is created inside the validated parent; a parent that does not exist is refused, because the package and the role make it. Every command that changes a guard validates the guard directory through the descriptor it then acts on, judges the record on its own descriptor (owned by that account, writable by nobody else), and examines a `guard.json.tmp` left behind through an identity descriptor before removing it by name, never opening it for writing.

## When a converge moves a node's endpoint

A node dials the control plane it was configured with, and a running node keeps dialling the endpoint its process normalised at startup whatever the file under it says now. Changing `node.server_addr` (or the TLS state that decides the scheme) is therefore not a configuration change the ordinary restart proves: the converge has to know what the running process dials, stop it under a deadline it owns, prove the stop, start the new invocation, and prove that the new process registered with the controller through the new endpoint. Three commands carry that, and the role is a thin caller of them.

**The decision is a dry run over the running node.** Before anything is rendered, `billet node migrate-endpoint --config /etc/billet/billet.yaml --desired - --dry-run --json` reads the rendering on stdin and answers `reported` with `planned` (true when the running node's effective endpoint differs from the rendering's OR the installed configuration's does; a node that is not running leaves only the installed one to compare), `from`, `to`, `effective`, `first_start` (no node installed and none running), `node_removed` (the rendering has no node section), `config` (`present` or `absent`) and `record`. The effective endpoint comes from the node's own registration record (`/run/billet/registration/current`, written by the process after every successful registration), read inside a bracket of unit observations so a restart under the read is retried rather than attributed; `record` says what was consulted: `current` (a usable record), `none` (the node positively not running), `unread` (a running process beside two configurations without a node section, so there is no endpoint to judge) or `absent-pre-r` (a running release from before the guard, positively identified by its image being the managed binary's and the managed binary answering "unknown command" to the guard's preparation, and only when the installed configuration has a node endpoint to decide from). A running node that has published no record within `--wait` and is not positively pre-R is could-not-tell (`unknown` `record`), never "not running": an R process that has not registered, or whose record write failed, is a reason to wait or restart it, not to migrate over it. A node still `deactivating` refuses (`stopping`) whatever the endpoints say, in the dry run and the action alike, and nothing restarts or cancels that stop.

**The action installs nothing.** The role's ordinary render installs the desired configuration first, then the units and the environment, and `billet node migrate-endpoint --config /etc/billet/billet.yaml --desired - --stop-timeout D --wait W --json` runs in place of the ordinary node restart, always, and judges afresh: `unchanged` (the record names the installed endpoint, the node is not running, or the rendering removes the node, each with `record` and `unit`) leaves the ordinary restart's gate alone; otherwise it observes the unit AFRESH immediately before the stop (the judgement's observation is from before the record wait): the configuration still the one judged, the same process the record was read from (`unknown` `process` when it moved), not `deactivating`, `LoadState=loaded` (a masked unit refuses `unit`), and `KillMode` `mixed` or `control-group` (a stop under `process` proves nothing about the compute the node leaves behind, `refused` `policy`); stops `billet-node.service` under the migration's own deadline, and reads the unit back: only `inactive`/`dead` with `Result=success` is the proof, because a stop job systemd's own `TimeoutStopSec` ended completes as done and `systemctl stop` exits zero over a failed service (`unknown` `unproved` with `state: stopped`, naming the triple it saw). When the migration's deadline expires first, the only thing signalled is the command's own `systemctl` subprocess (TERM, then KILL after ten seconds), never the node or its compute; the unit goes on stopping under systemd's bound, the answer is `unknown` `unproved` with `state: nothing`, and the next converge finds the unit `deactivating` and refuses with the time it has been stopping since. The configuration is examined again after the stop and before the start, so a file replaced under the stop leaves the node stopped (`unknown` `config` with `state: stopped`) rather than started on a file nobody judged. After the start the command reads the new `InvocationID` and waits `--wait` for a record under it whose endpoint is the installed one, and the record must be the started process's own (a restart under the wait is `unknown` `process` with `state: started`); `migrated` then carries `node`, `deployment`, `from`, `to`, `config_path`, `installed_sha256`, `invocation_id`, the registration `incarnation`, `registered_at` and the `stopped` triple. Every successful answer is closed against the configuration (identity, size and modification time since it was observed) and against the process the answer rests on (its `MainPID` and `InvocationID` unchanged, not `deactivating`); a `reported` or `unchanged` whose process moved is judged again from the unit's first observation, three times, then `unknown` `process`; a `migrated` is never retried, because a retry would stop the node again (`unknown` `process` with `state: started`). `planned` is deliberately conservative: it is true when EITHER the running node's effective endpoint OR the installed configuration's differs from the rendering's, so a node already dialling the new endpoint beside an installed configuration that still names the old one reports the change the converge involves (the render will rewrite the file), and only a node, a file and a rendering that agree report none.

**The controller confirms the registration.** `billet rollout registration --node N --incarnation I --wait W --json` on the controller opens the ledger read-only (as `rollout status` does, re-executed as the ledger's owner when run as root with the child's exit preserved) and polls one snapshot at a time until the node's current registration carries that incarnation and is live: `confirmed` with the epoch and the ledger's binding, `timeout` with the binding and the last row the last completed poll saw (`last: null` when that poll held no row for the node; exit 3; every poll runs under the wait, so a query the ledger holds past it is ended by it), `unknown` `unexamined` when the wait ended before any poll completed (nothing was proved either way), or `refused` with its reason: `unbound` when the ledger is bound to no deployment at all, and `trust` when the binding is not this host's identity (re-read at every poll, so an identity that moved under the polls is refused at the poll that sees it) or this host has no identity (an identity found ABSENT at any poll confirms nothing). The epoch is reported and never compared: a re-registration under the same incarnation bumps it, and a comparison would read a re-registration as news.

**The receipt is the durable proof, and every converge keeps it current.** `billet node receipt --evidence E --confirmation C --config /etc/billet/billet.yaml --run H --json` reads the migration's answer and the controller's strictly (one object, one value per member, the member names exactly the producers' at every depth, since Go's decoder would match `LIVE` for `live` and let the last one win, the booleans by their bytes), requires them to agree on the node, the incarnation and the deployment, requires `--config` to be the path the evidence was taken over and the installed configuration to be the one the migration judged (its digest and its endpoint), re-verifies the running node's record under the bracket (a node restarted since is refused, and the refresh is its way to a receipt), and writes `/var/lib/billet/node/endpoint-migration.json`: root 0600, ten members (`schema`, `run`, `node`, `deployment`, `installed_sha256`, `installed_endpoint`, `effective_endpoint`, `invocation_id`, `incarnation`, `written_at`), through the durable installer, in a directory created root 0700 with its parent flushed before anything is written into it. `billet node receipt --refresh --config /etc/billet/billet.yaml --desired - --run H --json` ends every ordinary converge, migrated hosts included: it waits for the running node's current record, compares its effective endpoint with the installed configuration's and the rendering's under one representation (scheme from `node.tls`, the host a canonical literal address or a lower-cased name with its rootedness kept, the port a number, so `127.0.0.1:7717` in a file and `http://127.0.0.1:7717` in a record are one endpoint), and rewrites the receipt for the current invocation, digest and incarnation, or answers `current` when a valid receipt already says so (its directory and parent flushed all the same, which completes a flush an interrupted write owed). The parent must be a root-owned directory writable by nobody else and the receipt directory root's and 0700, existing or just created (anything else refuses, and the reader reports such a directory's file as `invalid`); the directory examined is the directory written into and named by the answer (a directory swapped for a link after the examination is could-not-tell, before the write or after it), the file the shortcut names is the file that was read (removed or replaced after the read, could-not-tell), a regular file at the name that is not a receipt is replaced by the durable rename after the closing checks and never removed first (a removal would open a window in which a receipt another converge had just installed was the one removed; two publishers end with the last rename's receipt, both valid), a link or a special file at the name, at the read or immediately before the write, is a positive refusal naming the removal by hand, a `current` answer is closed after its flushes, and a `written` answer is given only after the receipt is read back from the examined directory as what was written and the name still holds that inode with the owner and mode billet wrote. A running node whose record names another endpoint than the installed one refuses with `migration`, naming the command that owes it; a node that is not running refuses with `not-running`; `--dry-run` reports what a converge would write and writes nothing. `billet release inspect --json` reports the receipt under `host.endpoint_receipt` as `present` (with its members), `absent` (the positive ENOENT), `invalid` (a file billet did not write, with why) or `unknown` (a failed examination), and the reader never opens a link or a special file at the name.

**The role is a thin caller of the three commands.** The host role runs `endpoint-decision.yml` before the configuration is staged or rendered and before the transaction's first stop (the dry run over the rendering on stdin, refused when it plans a change inside a binary upgrade, under the legacy protocol, under a policy that will not start the node in this converge, or with no controller named by `billet_migration_controller`, the first host of the `control_plane` group by default); `endpoint-migration.yml` in place of the ordinary node restart, running the action whatever the decision said, and on `migrated` confirming the registration on the controller (`rollout registration`, delegated, with the controller's managed binary and its environment file on a PostgreSQL controller) and writing the receipt from the two answers, carried as root-owned temporary files; and `endpoint-receipt.yml` after the node's start, the refresh (a dry run in check mode). Every answer goes through one strict parser per command (`endpoint-answer.yml`, `registration-answer.yml`, `receipt-answer.yml`), which hold each member to the producer's shape and refuse a run the bound ended. The executable every entry asks is the one that answered the preparation (the managed binary, the verified fallback or the candidate), replaced by the managed binary once this converge's transaction committed the candidate; with none to ask (the legacy shape, a pre-R managed binary with no candidate) the role compares the pre-render configuration's endpoint with the rendering's through the collection's endpoint representation (`junioryono.billet.endpoint_change`) and refuses a difference, since such a release cannot migrate it; on a host with no billet it refuses a node found running (nothing can say what it dials) and takes the fresh path otherwise. The bounds are `billet_migration_stop_timeout` (an hour by default; the migration's own stop deadline), `billet_migration_record_wait`, `billet_registration_wait` and `billet_receipt_wait`; the role's outer `timeout` around each call is the sum of every phase the command may take (the judgement's three attempts each waiting the record wait, the stop deadline, the unit's start bound, the record wait under the new invocation) plus the guard's bound as the margin, so it expires after the command's own deadlines and never ends a migration inside a phase it was allowed.

**A guard carries a note.** `billet converge-guard prepare --validate --note "..."` and `hold --note "..."` record up to 200 bytes of printable text in `guard.json` at acquisition, never rewritten by a later validation, a re-binding or a takeover, and `status --json` and the dry run report it; the role passes `billet_converge_guard_note` when it is set. It is what an operator reads first on a held host, as Puppet's `agent --disable "reason"` and balena's update locks each carry one.

**The answers a role consumes are committed fixtures written by the commands.** `ansible_collections/junioryono/billet/tests/fixtures/<command>/*.json` for `node-migrate-endpoint`, `rollout-registration`, `node-receipt`, `rollout-status` and `release-inspect` are produced by the Go tests that run each command over a planted shape, with paths, identifiers, digests and times spelled as the packaged host's; a gate's fake answers from those files and never from a shape written by hand, and the producers' comparison fails when a command's answer no longer matches (`BILLET_UPDATE_FIXTURES=1` rewrites them).

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

## Downgrades

The ledger records the newest release that has served it, and every open of the ledger refuses a binary provably older than that: the control plane's, an operator command's, and the upgrade probe's. The schema check that existed before caught only a pair of releases that differ in a migration, which most do not, so a stale pin converged by hand or an archive restored beside the wrong binary served rows a newer release had written with nothing anywhere saying so. Only a control plane moves the mark forward, and only once it has claimed the deployment and the ledger's binding has agreed the rows are its own, so neither `billet check` run from a laptop carrying a newer binary nor a newer server pointed at the wrong ledger can fence the running server out of its own restart. A development build cannot be ordered against a release and is neither refused nor recorded.

To run an older release on purpose:

```
billet host-upgrade --version v0.4.0 --allow-downgrade        # one host
billet rollout start --version v0.9.2 --allow-downgrade       # the fleet, to v0.9.2 or later
```

The transaction lowers the mark after the ledger snapshot, so a rollback of the downgrade restores the refusal with the rest of the ledger. Without the flag a downgrade is refused before anything drains, naming the flag.

Nodes record the highest release they have ever registered with, and `billet status` marks a host running something older than that as `DOWNGRADED`. That is a note rather than a refusal: a rollout that failed on a host and rolled it back produces exactly that shape, and the coordinator depends on seeing the older registration. `billet rollout status` says whether a rollout did it.

## Guest images

A release is a binary; the runner a job runs in is a guest image billet publishes weekly, and GitHub stops queueing work to a runner about thirty days after a newer one ships. Two things keep the image current without anybody pulling, and they answer different questions. Inside every host upgrade, after the services are stopped and before anything is fenced, the transaction asks the candidate binary whether the images this host's tiers boot are compatible with it (`billet images compatible`) and pulls, boot-verifies and promotes a generation for each that is not (`billet images pull --verify`), so a host is `committed` only with an image it can actually launch, and a host that cannot get one rolls back naming that as the reason. Between upgrades, the package enables `billet-images-refresh.timer`, which daily runs `billet images refresh`: it pulls, verifies and promotes only when the signed image channel names an image built after the newest generation imported, then reaps to three verified generations per guest contract, which is what keeps the baked Actions runner inside GitHub's thirty-day window on a host nobody upgrades for a month. A tart node's refresh pulls every configured image that is absent and nothing more. `automatic: false` stops the refresh too. `billet check` on a firecracker node names each image's newest generation and says when the timer is not enabled. [Guest images](guest-images.md) has the rest.

## Configuration

```yaml
release:
  channel: stable        # or candidate
  automatic: false       # the one sentence that turns automatic updates off
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

`automatic` is on unless you write `false`. This is the one zero value in the file that does not refuse, and it is on because the failure an unattended deployment actually meets is the update that never happens: a runner GitHub stops queueing to, a fix that shipped and never arrived. Everything around it is what makes that safe to default: the drain, the verified candidate and its rollback, and the ledger's refusal to go backwards.

A maintenance window bounds when a rollout may **begin**. It never stops one — a window that could interrupt a rollout would be a clock authorising a teardown, which is the thing this whole area refuses. It is UTC because a fleet spans machines whose local time is not one thing, and because a local window is unstable across a DST transition in exactly the quiet hour you picked.

## What updates itself, and what does not

| | Updated by |
|---|---|
| The control plane, on Linux | `billet-upgrade.timer`, enabled by the package or installed by the Ansible host role |
| The control plane, on a Mac | the `sh.billet.upgrade` agent `billet local up` installs |
| A PostgreSQL controller, single or active-passive | the same timer on every controller host; see [PostgreSQL and active-passive controllers](../deploying/postgres-and-active-passive.md) for what its transaction skips |
| Every node, Linux or Mac | the coordinator's dispatch, through the node's own updater |
| Guest images on a firecracker node | the transaction, and `billet-images-refresh.timer` daily |
| Guest images on a tart node | the transaction and the `sh.billet.images` agent, pulling only what is absent |
| A host installed by `install.sh` with no units | nothing; the units are what an updater stops and starts, and the script installs none |
| An EC2 AMI or a CodeBuild image | nothing; `billet ami build` remains an operator's step |
| The published Actions | the ref you write: `@v0` moves with each release |
| The Terraform modules | nothing; a `?ref=` in your repository is yours to move |

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
