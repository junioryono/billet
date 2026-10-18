# Draining and stopping

Nothing billet does on its own ever destroys a running job. A drain seals admission and waits, for as long as the work takes; a stop leaves running jobs running; the one operation that ends work is `billet force-destroy`, and it names every job before it acts.

## Drain

```bash
billet drain --reason "kernel update"        # seal admission; returns at once
billet drain --wait --timeout 2h             # seal, then wait for the deployment to hold nothing
billet resume                                # open admission again
```

A seal stops new work being admitted. It takes effect at each listener's next poll and says nothing about what is already running, so `--wait` is what asks the real question, in two stages. First the ledger: no lease is holding anything. Then the machines: the control plane asks every node what it is actually running, because the ledger cannot see compute whose lease has already gone, and an empty answer counts only after five minutes of continuous emptiness with no launch dispatched in between. `--wait` exits 2 while still draining or when interrupted, and the seal remains; treat that as *not drained*, because reading it as permission to continue is what destroys running jobs. `--without-compute-proof` skips the second stage and prints a different conclusion.

A host that is off, or negotiated a protocol too old to be asked, never answers and blocks the proof; `billet nodes decommission` is the escape ([Adding and removing nodes](nodes.md)).

## Stop

A SIGTERM to `billet node` is a drain. A SIGTERM to `billet server` is a **handoff** while the deployment admits work, and a drain once it is sealed:

- `billet server` with admission open treats the stop as a restart (an upgrade, a converge, a `systemctl restart`) and hands over to the next control plane at once. It finishes the destroys it already owes for jobs GitHub has concluded, then exits without waiting for running jobs, without closing its message sessions (one a session recovery was already replacing is not reopened) and without handing back capacity. The jobs keep running on their hosts and the next control plane re-adopts their leases. GitHub keeps seeing the last advertisement and redelivers what it assigns meanwhile to the next session; held escrow comes back once its leases stop being renewed, exactly as after a crash. The next control plane waits out GitHub's hold on the abandoned session, measured at 60 to 92 seconds, before its tiers poll again. A drain used to do this instead, and it cost twice: on 2026-10-04 it held the reference deployment's controller for ninety minutes while one long job ran, so a converge timed out and nothing launched; and because a draining server lowers its advertisement, 28 jobs GitHub re-offered in that window were never offered again and stayed queued until they were re-run (`gh run cancel` and `gh run rerun --failed` recover such a job). A stop that cannot read admission drains rather than hand over. To shut the whole deployment down, seal first (`billet drain`, or `billet local down`, which seals for you): a plain `systemctl stop` on an open deployment hands over to a control plane that is not coming, and GitHub keeps assigning into its session until that session expires. A seal is deployment-wide, so to retire one controller of an active-passive pair while the other keeps serving, do not seal; follow the [controller-retirement procedure](upgrades.md#when-a-controller-retires), where handing over to the survivor is what you want. Removing the package stops the services without sealing, for the same reason.
- `billet node` with `node.stop: handoff` (accepted for `node.provider: firecracker` only) hands over instead: it withdraws from placement and exits at once, leaving the compute it holds running, and the next node process adopts it when it registers (Firecracker's VMMs live in cgroups of their own, so stopping the service does not stop them). The node's guest cache listener stops accepting at once and gives requests already in flight up to fifteen seconds to finish (a sixth of the lease TTL, because nothing renews the handed-over leases until the next process registers); a guest's Git, Bazel and Go caches reach the node through a relay inside the guest that waits for the next process to listen, so a fetch that starts during the gap waits instead of failing. The next process mounts the guests' cache volumes again in its own mount namespace before it serves them, and serves nothing until it has registered and recovered; a volume it cannot mount again is a cache miss for the rest of that job. An Actions cache upload or download in flight at the stop is cut off, because its tunnel is not one the listener can wait for; the Actions cache fails open, so the step warns and the job goes on. A job's usage is measured across a handover: the stopping process keeps what it measured of each job under the node's `state_dir`, the next process continues each job's series from it and reports it when the job ends, and the time between the two is an interval marked `handover` that no step's or window's usage counts; hardware counters, package energy and traffic by destination are not measured for a handed-over job, and a job that ends before the next process has found its counters reports no usage. An operation that must not leave guests behind asks for a drain for as long as it runs: while `/var/run/billet-node-drain` exists, every node process that stops drains, whatever `node.stop` says. A stop that asks for one before the process has recovered what its host runs recovers first. A handoff whose withdrawal does not land drains instead, because the next process takes over only from a process the control plane recorded as withdrawn. `billet local down` writes it before stopping a Linux node and `billet local up` removes it. The Ansible role writes it before a converge changes guest networking, before the node's next stop whenever its config changes, by an ordinary restart, an endpoint migration or an upgrade's candidate config (a changed config can move what the next process adopts from), before stopping a node that will not run on the host again, and before recovering an interrupted upgrade's ledger, and removes it once the node is where the converge leaves it. Removing the package writes it and leaves it, and refuses to go on if it cannot. To take the host out of service by hand, `sudo touch /var/run/billet-node-drain` before `systemctl stop billet-node`, and remove it when the node is meant to hand over again. Use it on a host that is restarted to be upgraded or converged, after watching one handoff work there; a host that is leaving is drained first or decommissioned.
- `billet node` with the default `node.stop: drain` stops taking new work, waits for the compute already running here, destroys it when each job completes, tells the control plane it is leaving, and exits. It notices that it holds nothing within about two seconds of the last compute going, whatever the sweep interval. The first launch it refuses while draining takes the host out of placement until it registers again, so the control plane stops sending it work it will only refuse.
- `billet server` with admission sealed (`billet drain`, `billet local down`) stops admitting work and waits for the jobs it is tracking, finishing the teardown it owes (destroys a completion already asked for, the session close, the idle escrow). A pool runner started from GitHub's assigned count is not waited for while it has no job. If the ledger records no job started on any runner still held, and still records none after one more poll, the server stops and logs `only pool runners that never started a job remain`. The runners stay registered and their capacity stays charged. Nothing is destroyed, and the next control plane adopts them. Without this rule the drain could wait indefinitely: one was observed idle for a whole sixteen-minute drain with jobs queued for its tier.

Whichever way a node stops, a launch it has already begun finishes first and is reported, so the control plane hears that the guest started and the guest is drained or handed over like any other, rather than cut off half made with its capacity held in quarantine until the next process accounts for it. A launch is seconds of work on Firecracker; the node logs that it is waiting for it, and a second signal cancels it, after which it unwinds as any failed launch does. A launch the control plane sends after the stop has begun is refused, as before.

Neither has a deadline. `drain_timeout` decides only when billet starts saying the drain is running long, and it repeats every fifteen minutes. The packaged units and plists give the service manager 88200 seconds before it kills a drain, so a reboot mid-drain loses billet's bookkeeping and not the jobs: the guests keep running, their leases stay charged, and the next process re-adopts them.

**A stop that lands while `billet server` is still starting is a stop too.** Whether it arrives as the ledger opens, while a standby waits for or takes the controller claim, as the node wire reads its authority, or while the tiers' scale sets are reconciled with GitHub, the process prints `billet server: stopped` and exits 0. Under `systemctl stop` the unit then ends `inactive`, not `failed`, even though it never sent `READY=1` (measured under systemd 255 on 2026-10-16); a SIGTERM sent some other way, with no stop job, to a server that has not yet sent `READY=1` leaves its start job unfinished, so systemd counts the start as failed and `Restart=on-failure` starts it again. Three endings exit non-zero instead, because each is something an operator has to know. A server a successor took the deployment from exits with `this process is no longer this deployment's controller`; unless systemd was stopping it, `Restart=on-failure` brings it back to take the deployment again or to be refused naming the holder. A fault that arrived with the stop is reported as the fault. And a stop that lands after the process migrated the ledger and before its claim recorded the release exits naming the two schema versions: `migrated the deployment's ledger from schema N to schema M before it failed`, after which a billet that expects the older schema is refused by that ledger and the schema does not move back ([Downgrades](upgrades.md#downgrades)), or `could not tell whether` that migration committed, in which case it may have. Once the claim has recorded the release, a stop is a clean one again. Each of these three endings under `systemctl stop` leaves the unit `failed` on purpose, and `billet server retire` refuses a `failed` server, so read the message, then run `systemctl reset-failed billet-server` before retiring that host.

A second SIGTERM ends the **wait**, not the work:

```bash
sudo systemctl kill --kill-whom=main --signal=SIGTERM billet-server
```

`--kill-whom=main` matters because without it systemd signals every process in the service, including a container CLI billet has in flight. The jobs still running are left running, their capacity stays charged until a host proves the compute gone, and the next control plane re-adopts their leases.

## Proving a node stop

**systemd's exit says nothing about how a node stopped.** The node exits 0 after a drain, after a handoff, after a drain a second signal cut short, and when superseded. Measured on systemd 255:
- a stop killed at `TimeoutStopSec` still has `systemctl stop` exit 0, and leaves the unit `failed` with `Result=timeout`;
- `systemctl reset-failed` then turns that into exactly what a clean stop leaves.

So the node keeps its own record, `<node.state_dir>/endings.json` (root's, 0600). It holds the newest sixteen processes, each written `running` when it starts, under systemd's invocation id, and replaced when it stops by one of:
- `drained`: it held nothing and knew what its host runs;
- `idle`: it never registered;
- `handed-off`;
- `left-running`: a second signal, or a stop that never learned what its host runs;
- `superseded`;
- `failed`.

An entry still `running` names a process that was killed or crashed. `billet node ending show --config /etc/billet/billet.yaml --json` prints it.

**A stop billet performs is proved by the stopped invocation's own entry.** `billet host-upgrade` and `billet local down` read the node's invocation before the stop, since a clean stop clears systemd's, and judge its ending after it:
- `local down` requires `drained` before it stops the server;
- so does an upgrade while a drain is requested;
- otherwise `handed-off` passes too.

An `idle` process never registered, so it never adopted what the process before it left: its stop is as good as that one's. The invocation must also be the newest process the record holds; one that a restart followed proves nothing about the process the stop ended. A node that was already stopped is judged by what its last process left, so running `local down` or an upgrade again does not pass a stop an earlier attempt refused. An unproved stop leaves the node stopped and goes no further. An upgrade rolls back with nothing installed.

**A node of a release before the record keeps none.** Its stop falls back to systemd's `inactive`/`dead`/`success`, labelled `proof: systemd`, and only when the processes the record holds before it left nothing a start may not run over. That is the weaker floor, which a handoff or a cut-short drain also passes. systemd's answer is recorded either way, clean or not, so the start after it reads the same thing after `reset-failed`. An updater older than this release stops its node with the old check one last time.

**A node is started only over a proved ending.** `billet local up`, and an upgrade's finish, rollback or resumed journal, each read what the node's processes left first. A start passes over a drain, a handoff, an acknowledgement, or an ending from an earlier boot on a host-run provider (the reboot ended its compute). An `idle` ending, and systemd's clean stop of a process that kept no record, pass the question to the process before them. With no entry at all, systemd's account of the unit decides. A node that is already running is not started again, so there is nothing to admit.

**When a stop is unproved:**
1. Find out what it left: `billet node ending show`, `billet leases`, and the provider's own view (`pgrep -a firecracker`, `docker ps`, `tart list`).
2. Acknowledge it with the arguments the refusal named (its `acknowledge` field in JSON): `sudo billet node ending acknowledge --config /etc/billet/billet.yaml --invocation <id> --json`. That is the newest node entry, or `--unrecorded` where none has an invocation.
3. Start the node again. Its recovery adopts what is still running.

An acknowledgement takes the node's own deployment lock, so it refuses while a node process holds the identity. It permits a start only, never an operation that needs a drain.

**A record that cannot be read** (corrupt, from a newer release, or writable by another user) is never overwritten, and every judgement reads it as could-not-tell. Once what the host runs is known, `sudo billet node ending acknowledge --config /etc/billet/billet.yaml --reset-unreadable --json` keeps it beside itself (`endings.json.unreadable-<time>`, a hard link) and replaces it in one rename with a new record that holds only the acknowledgement, so the host never has no record at all; a directory at that name cannot be linked, so it is moved aside after a marker (`endings.resetting`) is written, and a missing record with the marker present reads as could-not-tell until the reset is run again.

These checks are Linux's. The record is written on a Mac too, but a launch agent's stop is not judged by it yet. The Ansible role's own node restarts read the same record once the collection adopts it.

## `billet local down` and `uninstall`

`billet local down` is a drain that then stops, in that order: seal, wait, stop the node, prove it drained from its own ending record, stop the server, disable both. It has no default time limit (`--timeout` bounds the wait; `--reason` is recorded). It re-reads the clearance immediately before the first stop, refuses to stop a service that is still deactivating, and on a node-only host says that it cannot fence anything and names `billet drain` against the control plane. Its seal is cleared by the next successful `billet local up`; an operator's seal from `billet drain` is left alone. `billet local uninstall` is `down` plus forgetting the services, leaving the config, App key, ledger, identity and CA where they are.

On a Mac the same commands drive the launch agents, with one difference: launchd cannot start a disabled service, so `up` enables before it proves, and what protects the host is the unwinding ([Run jobs on a Mac](../deploying/mac-tart.md)).

## Retiring a controller and keeping its node

Use the [controller-retirement procedure](upgrades.md#when-a-controller-retires) to remove one controller of a PostgreSQL active-passive pair while retaining its node. Plan the endpoint move first: a node still dialling the retiring controller cannot be requested, so an ordinary converge must move it to the survivor and establish matching receipt, registration and installed configuration before retirement. The node runs through controller shutdown, archival and configuration replacement; retirement's own restart phase then stops and starts it before completion. Retention is not uninterrupted node uptime.

A later converge may admit a settled retained host whose node is quiet `inactive` or `failed`, provided the entry proof finds no main or control process, no cgroup processes or queued job, persistent enablement and an agreeing supported loaded and installed unit. This permits the shared ordinary node lifecycle to bring it back. It requires no runtime registration record at entry and proves neither successful drain nor compute absence; strict publication, settlement and the ordinary pass's closing proof still require an active node with its current registration.

The retained ordinary pass records the same node lifecycle operations as a node-only pass over the same effective configuration, including network changes, endpoint migration and restart after interruption. The recorded-operation parity witnesses do not establish successful drains on real hosts. The all-host successful-drain guarantee belongs to issue #127 and is not established by this retirement work; use the [deployment drain](#drain) when a compute-clearance proof is needed.

## Force

```bash
billet force-destroy --tier billet-8vcpu --reason "host being retired" --yes
```

The one operation that fails somebody's build on purpose. It refuses unless admission is already sealed by `billet drain`, because it enumerates a set, shows it to a person and acts on their answer, and a job admitted in between would be destroyed without ever appearing in the list. The target set is fixed when the request is taken; a standing flag would destroy every job launched after it. Every affected job, lease and host is printed before confirmation, forced leases are archived as failed, and it warns that GitHub does not requeue a job whose runner vanished after starting. A lease a node has taken custody of is not touched; `billet leases release --force` is the operation for that, because it goes through the holder rather than underneath it. Exit 2 means refused and nothing destroyed.

## Removing GitHub and AWS resources

`billet teardown --tier <name>` (a tier's `runs_on`, or its label; `--target` scopes it to one target) or `--all` deletes the scale sets billet created on GitHub; there is no delete control for them in GitHub's UI. `billet decommission --yes` deletes the EC2 instances and the EBS+S3 cache billet made outside Terraform, scoped by the deployment identity; run it before `terraform destroy`, and pass `--terminate-instances` only if failing any job still running is acceptable. The CodeBuild Terraform module refuses a destroy while a build is running; drain first.

## What stays charged

A stop deliberately leaves capacity charged for compute that is still running. Freeing a slot whose container is live is the overcommit every ordering here exists to prevent; capacity reclaimed late is recoverable, and a failed build is not. [Status and leases](status-and-leases.md) shows what is held and why.
