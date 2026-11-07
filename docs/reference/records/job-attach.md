# Attaching to a job, and holding a failed job's VM

What a real microVM did when a terminal was opened in it and when a failed job's VM was held, for the facts [ADR-032](../decisions/adr-032-job-attach.md) relies on.

Both runs used an EC2 instance with nested virtualization (m8i.2xlarge, Ubuntu 24.04, kernel 7.0.0-1014-aws), Firecracker and the jailer v1.16.1, and the published guest image `guest-20261102-060005` (runner 2.337.0) and its kernel, with the branch's `billet`, agent, job-started hook and `billet-guest-attach` unit put into the image. The instance was terminated afterwards.

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

Measured on 2026-11-07, from main at `9bbfb7fa`. The end-to-end suite's stack ran over the wire: the real control plane, ledger, listener and cleanup loop, the node wire, and the node runtime over the real Firecracker provider. One tier, `trusted`, with `debug.attach: true` and `debug.hold_on_failure: 2m`. Two things stood in for the real thing:

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

**One limit across a restart that this run could not show.** A restarted listener restores its pending completions, and so its holds, once its message session opens. The scripted service opens a session at once. GitHub refuses a successor's session while the one a stopped control plane left is still active: on eight runs on 2026-09-04 it was still refused at 60 s and opened at 91 or 92 s (`openSession` in `internal/server/server.go`). So a hold whose bound falls while a restarted control plane waits for its session ends when the listener runs, and the cleanup loop destroys its VM after that, which is the same wait every finished job's destroy has after a restart. The bound itself does not move. #783 is the change that would remove the wait.

## What was not run

- A hold on a job a real runner ran for a real GitHub; the job here was a scripted one, in a real VM.
- `billet drain` waiting for a hold, and `--cut-holds` ending one, against a real VM; both are exercised by the suite's tests on the simulated backend.
- A hold on a real Ceph-backed node, and on a PostgreSQL ledger.
