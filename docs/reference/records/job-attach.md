# Attaching to a job, and holding a failed job's VM

What a real microVM did when a terminal was opened in it and when a failed job's VM was held, for the facts [ADR-032](../decisions/adr-032-job-attach.md) relies on.

The three runs below used an EC2 instance with nested virtualization (m8i.2xlarge, Ubuntu 24.04, kernel 7.0.0-1014-aws), Firecracker and the jailer v1.16.1, and the published guest image `guest-20261102-060005` (runner 2.337.0) and its kernel, with the branch's `billet`, agent, job-started hook and `billet-guest-attach` unit put into the image. The instance was terminated afterwards.

## A terminal in a running job, and after its runner exited

Measured on 2026-11-05: a real job from `billet-testbed/jobs` ran in the VM, and terminals were opened through the node's `handshakeVsock` and the relay's frames from a test binary on the host, not yet through a control plane.

Both during the job and after the runner had exited, which is the case a hold relies on:

- The guest's `billet-guest-attach` unit started from the metadata, the vsock device took `CONNECT 5151`, and a shell opened as `runner` (uid 1001, groups `runner` and `docker`) in `/home/runner/runner/_work`.
- The pts belonged to `runner`, the operator's window size reached it (33 by 111), and the shell's `exit 7` came back as the exit frame.
- The session's environment held neither the node's bearer nor the runner's registration.
- A socket answered by the wrong uid was refused, and a port with nothing listening came back as nothing listening in under a millisecond.
- A session dropped while its shell ignored the hangup still gave its slot back: six more opened in turn.
- The job-started hook printed `billet: to open a terminal in this job's VM, run on the control plane: billet jobs attach <lease>` and the hold policy into the job's log, with the lease taken from `RUNNER_NAME`.

## A hold through the whole stack

Measured on 2026-11-07, from main at `9bbfb7fa` with #782's test-only commit on top, and, for the run whose numbers are below, three temporary log lines in `restoreCompletions`, `holdCompletion` and the cleanup attempt put in on the probe host to trace the restart; none of them changed what ran. The end-to-end suite's stack ran over the wire: the real control plane, ledger, listener and cleanup loop, the node wire, and the node runtime over the real Firecracker provider. One tier, `trusted`, with `debug.attach: true` and `debug.hold_on_failure: 2m`. Two things stood in for the real thing:

- **GitHub was the scripted Actions service** the suite uses, so each job's offer, assignment, start and completion were messages the test sent. The runner in each VM never reached a real GitHub; what was held, attached to and destroyed was the real VM.
- **The root disk was a device-mapper snapshot** of a read-only loop device over the image, not an RBD clone. A Firecracker node's config requires Ceph, and the provider takes its disk through the `RootDisk` interface, so that interface was the one seam replaced.

The terminal went through `billet jobs attach`'s own path, `app.AttachJob`, over the controller's operator socket; release and status were the commands' own calls.

| Job | What happened | Measured |
|---|---|---|
| A, failed | held at its completion with its bound written once, two minutes out; still running 20 s later, its jail on the host; `billet status` said `a failed job's VM, 1m40s left of its hold`; a terminal opened in it as `runner`, took a 33 by 111 window, and `exit 7` came back as status 7; the shell's end ended the hold | destroyed 9.8 s after the shell exited, the hold ended `session` |
| B, failed | held, then `billet jobs release`'s call ended it | destroyed 19.5 s after the release, the hold ended `operator` |
| D, succeeded | no hold row was written | destroyed 0.25 s after its completion |
| C, failed | held; the control plane and the node both stopped while the VM ran on, and a new control plane and node started over the same state directory; the node adopted the VM, the hold row came back with the same bound, and the listener restored the pending completion; still running 10 s before the bound | destroyed 15.4 s after its bound, the hold ended `bound` |

The ends after a release and after a shell exit are the listener's cleanup tick, not a wait in the hold: the hold ends at once, and the next pass destroys the VM. `ss -ltn` taken before the first job and after the last differed only by the test process's own control-plane listeners, and nothing was left on the host at the end: no VM, no jail and no clone.

Every assertion of the probe passed, and the test binary still reported `FAIL`: the scripted Actions service errors on the `GET .../actions/runs/<id>/jobs` reads that main's job statistics (#770) make, which it does not serve, and the suite marks an unexpected call as a failure. None of those reads is on the hold's path.

**What a restart costs a hold, which this run could not show.** The scripted service opens a session at once. GitHub refuses a successor's session while the one a stopped control plane left is still active, and a restarted listener restored its pending completions, and so its holds, only once its session opened. The real-GitHub runs below measured the refusal and tested #794's branch, whose cleanup no longer waits for the session.

## A hold through the real control plane and real GitHub

Measured on 2026-11-07, from the branch of #794 at `ba6a1da1` (main at `803f1f06` with #794's first four commits). The server first started on the commit before, `a8b613ef`, and was restarted onto `ba6a1da1` at 16:36, before the node registered and before any job; every job ran on `ba6a1da1`. Nothing scripted stood between GitHub and the VM:

- **The control plane was the `billet server` binary**, run on the shared test host with the `billet-testbed` App, a runner group `b1t19-hold` restricted to one workflow in `billet-testbed/jobs`, and one tier, `trusted`, with `debug.attach: true` and `debug.hold_on_failure` of 3 minutes on SQLite and 2 on PostgreSQL. `billet check` verified the App, its permissions and the runner group.
- **The jobs were real**: `workflow_dispatch` runs of that workflow, whose job wrote a marker file into its work directory, slept, and then failed or passed. The runner in each VM registered with GitHub through the JIT configuration billet minted, ran the job and reported it.
- **The node was the suite's node process** (`startNodeLoop`: `app.NewNodeClient`, `app.NewNodeRunner`, `app.RunNodeLoop`) over the real Firecracker provider under the jailer, on an EC2 instance with nested virtualization, dialing the control plane on loopback through SSH tunnels, since the App's key stays on the shared host. It ran the same runtime a `billet node` runs; its root disk was the device-mapper snapshot above, which is why it was not the `billet node` binary, whose config requires Ceph.
- **The guest image was `guest-20261102-060005`** with the branch's `billet`, agent, hook and attach unit put in, since no published release carries the attach server yet.

Every operator step was the command an operator runs, on the controller's host: `billet status`, `billet jobs attach` with a script on its standard input, `billet jobs release`, `billet drain --wait` with and without `--cut-holds`, `billet resume` and `billet teardown`. Times are UTC.

| Ledger | Run | What happened |
|---|---|---|
| SQLite | 42392255184, failed | the VM was held at 16:38:42 until 16:41:42; `billet status` said `a failed job's VM, 2m50s left`; `billet jobs attach` opened a shell as `runner` in `/home/runner/runner/_work`, where the job's marker file still was, and its `exit 7` was the command's exit status; the hold ended `session` at 16:39:01 and the VM was destroyed at 16:39:08 |
| SQLite | 42392389501, failed | held until 16:42:55; `billet drain --wait --timeout 40s` printed `1 failed jobs' VMs are held, until 2026-11-07T16:42:55Z at the latest. A drain waits for them; billet drain --cut-holds or billet jobs release ends them now.` and exited 2 at its timeout with the deployment sealed; `billet drain --wait --cut-holds` ended the hold `drain` at 16:40:51, and the VM was destroyed at 16:41:08, about 1m47s before its bound |
| SQLite | 42394193056, passed | no hold was opened; `billet status` afterwards showed nothing held and no open lease |
| SQLite | 42393812124, failed | held at 16:57:07 until 17:00:07; the server's stop began at 16:59:52 and finished at 16:59:57 (a handover), and the same build started again at once; GitHub refused its session on six attempts; the hold ended `bound` at 17:00:30 and the VM was destroyed at 17:00:31; the session opened at 17:00:52 |
| PostgreSQL 18 | 42394482798, failed | held at 17:05:07 until 17:07:07; `billet jobs attach` ran a shell whose `exit 9` was the command's exit status; the hold ended `session` at 17:05:16, and the VM was destroyed at 17:05:36 |
| PostgreSQL 18 | 42394596165, failed | held at 17:06:25 until 17:08:25; the server's stop began at 17:08:10 and finished at 17:08:15, and the same build started again at once; GitHub refused its session on seven attempts; the hold ended `bound` at 17:08:48 and the VM was destroyed at 17:08:49; the session opened at 17:09:20 |
| PostgreSQL 18 | 42394923271, failed | held at 17:10:11; `billet jobs release` at 17:10:16 printed `Released.` and ended the hold `operator`; the VM was destroyed at 17:10:49 |

**A restart inside a hold.** Both restarts were timed so the bound fell while GitHub still refused the new control plane's session, and both destroyed the VM about 24 s after its bound (23.6 and 24.1 s), consistent with the cleanup pass every 30 s, a third of the 90 s lease, and 21 and 31 s before the session opened. That is #794: the listener restores its completions and runs its cleanup before the session opens. Before it, the VM stayed until the session opened.

**How long GitHub held a stopped control plane's session.** Three graceful stops, each a handover that closes no session: the successor's session opened 64 s after the first stop (6 attempts, 52 s of waiting), 60 s after the second (6 attempts, 52 s) and 69 s after the PostgreSQL one (7 attempts, 62 s). The 2026-09-04 measurement in `openSession`'s comment, refused at 60 s and open at 91 or 92 s, was of abandoned sessions; these are a bracket of the same kind, not an expiry.

**A job queued in the moment after `billet resume`** (run 42392947357, dispatched seconds after the resume at 16:46:39) had not been offered by 16:56, while the tier by then advertised four runners; it was cancelled, and a job queued at 16:56 was held at 16:57. One observation, not measured further.

**Other observations.** The lease of a running job, held or not, reads `launching` in the ledger, and a drain names it so: nothing advances a lease past `launching`, which #795 records. On the probe host, `ss -ltn` at the end showed only its resolver, sshd and the tunnel's own listener, and nothing was left: no VM, no jail, no snapshot device. The instance, the runner group, the workflow, both scale sets and the PostgreSQL container were removed afterwards.

## What was not run

- The `billet node` binary. The node was the same runtime assembled by the suite, because the binary's config requires Ceph for a Firecracker node; what that leaves out is the node's config path, `billet images pull` into a Ceph pool and the RBD clone, which were proven on the reference host before #533.
- A guest image a release published with the attach server in it; the server was put into the published image by hand.
