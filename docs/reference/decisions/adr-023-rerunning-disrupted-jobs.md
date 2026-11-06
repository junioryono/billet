# ADR-023: billet re-runs a job it ended, for a tier that asks, on GitHub's proof

Accepted, 2026-11-04. Issue #541.

## Context

GitHub does not requeue a job whose runner vanished after it started, so a job billet's infrastructure ended goes red, and a developer re-runs it by hand. billet has recorded that since migration 35: `job_history.disruption` is what its own infrastructure did to a lease, written in the transaction that observed it and never once GitHub reported on the job, and `billet leases failures` shows it beside GitHub's result.

Until now billet re-ran nothing, and said so in that report, for one reason that still holds: a re-run is a side effect on somebody's repository, and a deploy or a migration must not happen twice because a machine went away. That reason decides who may turn this on and what counts as enough evidence. It does not decide that nobody may.

## Decision

**A tier may ask, with `retry.disrupted`, and the default is never.** It is the number of re-runs billet asks for per workflow run, 0 to 5, counted across every attempt of that run. A request is made only while the run has had fewer re-runs from billet than each tier whose job it would run again allows. The bound is billet's own: a run its infrastructure keeps ending is a fleet problem more attempts would hide.

**A tier asks about the jobs that end after it started asking** (`rerun_tiers`). A job that went red before an operator set `retry.disrupted` is not re-run hours later because the config changed, and a tier that stops asking and asks again starts again.

**The closed list of causes is `reclaimed` and `guest-absent`** (`alloc.RerunCauses`). Each says the compute was taken from under the job by something other than the job and other than a person's choice.

**A disruption is not enough. GitHub's own record must show a lost runner:** the job concluded `failure`, a step never finished, and no step concluded `failure` (`github.AttemptJob.RunnerLost`). This is a positive signature, [measured on 2026-11-04](../records/job-reruns.md): a runner killed mid-step leaves it, on Docker and on EC2, and a job that failed on its own never does. Anything else is `unproven`, and the job stays red.

**The request is exact, or there is none.** `rerun-failed-jobs` re-runs every failed job of the run, so it is sent only when every failed job of the latest attempt is proved. One proved job beside failed jobs it must not run again, those that failed on their own and proved ones past the window, is asked for alone (`/actions/jobs/{id}/rerun`). More than one is left for a person and recorded as `mixed`, because each request starts an attempt, and the next job's id is then another attempt's.

**One request per attempt, by the key of `job_reruns`.** It is written `asking` before it leaves, with the jobs it covers, and settled by GitHub's 201. Any other answer leaves it in doubt, because an answer lost on the way back is indistinguishable from a request never received, and the next pass settles it by reading the run: a run going, or on a later attempt, means it landed, and a completed run on the same attempt means it did not, and it is sent again, within the limit as it stands then. Five sends, then `failed`, reported in the log, the ledger, `billet leases failures` and `billet_job_reruns`. A request in doubt is settled whatever became of the jobs that made it, so a job past its window or a tier that stopped asking cannot strand one, and a job a request covers is never expired while it is in doubt. A request is never sent more than a day after it was decided, nor after a tier it covers stopped asking and asked again. A request whose run cannot be read, or read sensibly, on its first read after a day is settled `unconfirmed`, and so is one whose run no target this control plane serves can read, after a day. A refusal or a throttle is the installation's and may pass, so it holds the request to two days, and past two days a request is settled `unconfirmed` by a read that is refused or throttled, or without a read on a pass that cannot read its run: its installation paused or at its reserve, or the run waiting between failed reads. A run that waits only for its installation's share of the pass is read in its turn. A run that can be read past two days is settled from the read, so a request GitHub never took, found after a long outage, is given up `expired` rather than counted. `unconfirmed` is counted against the limit as a request GitHub accepted, since it may have been, which only makes billet ask less, and its jobs are decided at last.

**What GitHub cannot be read to say is never decided about a job.** Two decisions are made without a read, and neither says anything GitHub would have to: `expired`, for a job past the window, and `unconfirmed`, which records that a request covering the job may have landed and only makes billet ask less. Each is final, as every decision is, and the job is not considered again. A first attempt that does not list a job, or a list naming one runner twice, is read again later and ends at the window. A job is `superseded` only when the run's latest attempt is a later one than the job's.

**Never on the scheduling path.** The loop (`server.rerunDisrupted`) is a background task of the control plane's group, so it can stop no tier and no tier waits for it. It runs where no tier asks too: its first pass records that none does, which is what makes one asking again start again, and each pass after reads the requests in doubt, which must be settled when their tier stops asking, and asks GitHub only about those. An installation's few reads a pass go to the runs read longest ago, so every run gets its turn. It shares the App installation's REST budget under the reserve the queue-time sweep already leaves. A run is decided from every one of its jobs the ledger holds, read to the end, never a page of them. A job waits for its whole run, because GitHub refuses a re-run of a run still going and a sibling may yet fail; two minutes after the newest result of the run, and for every failed sibling on this deployment's runners whose result billet has not heard, because results arrive one at a time; and a day at most.

**The outcome is on the job.** `job_history.rerun_decision`, with when and against which attempt, is what `billet jobs show`, `billet leases failures` and #535 read.

### `actions: write`

The request needs the App to hold `actions: write`, and GitHub offers nothing narrower: the same level also covers dispatching a workflow and deleting logs, artifacts and caches. billet uses it for the two re-run requests and nothing else.

It stays on the runner App, opt-in. A target requires exactly `write` when one of its tiers sets `retry.disrupted`, and refuses it otherwise (`app.ActionsAccessOf`, `PermissionMismatches`), so `billet check` holds the claim in both directions as it does every other permission. Unlike `actions: read`, which billet requests by default and so tolerates unneeded, an App holding write where no tier asks is refused. `billet github-app create --actions-write` requests it and says what it also grants; an existing App needs the permission added on its settings page and accepted on its installation.

A separate App, the shape #512 chooses for workspaces, was rejected here. #512 separates `contents: write` and `pull_requests: write` because those are a different kind of power, used by another subsystem, on behalf of a person, and the runner App's two-way check would have to stop meaning what it means. A re-run is the control plane acting on the jobs it scheduled, with the credential it already holds for them. A second App would add a key to create, store, rotate and back up for each target, for one call on a rare event. This decides nothing about #512, which stays as ADR-017 has it.

The cost is real: a stolen App key can now dispatch workflows and delete logs on every repository the installation reaches, which is why it is not the default, and why a target with no such tier refuses it.

## What was excluded, and why

- **`node-forgotten`.** It says this control plane stopped hearing from a host. A host cut off by a partition goes on running the job, and the job can then fail on its own.
- **`held-past-limit`.** `node.max_custody` is a bound an operator set on how long held compute may live. A job that hangs, or simply outlives it, is ended by that choice, leaves a lost runner's record, and would be re-run to hang again, holding capacity for the whole bound each time.
- **An operator's `billet force-destroy`.** It records no token, and a person chose to end that job, so re-running it may undo why.
- **A launch that never registered, the issue's third cause.** It is not a red run: GitHub cancels an assignment nobody ran and reassigns it within about five minutes ([measured](../records/assignment-protocol.md)).
- **A drain, the issue's first cause.** A drain waits for what is running and cuts nothing, so there is nothing to attribute to it.
- **A runner stopped gracefully, whatever stopped it.** On SIGTERM a runner reports the running step `failure`, the same as a job's own failure on every surface billet can read, so it cannot be proved and stays `unproven`. Docker's and Firecracker's teardown are SIGKILL, and EC2's `TerminateInstances` was measured to leave the lost-runner signature, so billet's own destruction is not graceful. A host shut down cleanly may be, and those jobs stay red.
- **A time window as a second proof.** Accepting a failed step that ended after the disruption would cover a graceful stop, on two clocks, and re-run a job that failed on its own in the seconds after a warning. That is circumstance, not proof, and the case it was for turned out not to be billet's.

## Residual

- A job that crashes its own host, on a backend with no real boundary, can reach `guest-absent` with a lost runner's record, and is re-run up to the limit. It is bounded, and it is why a tier asks rather than billet deciding.
- A job re-run this way runs its steps again, deploys included. `retry.disrupted` is for tiers whose jobs can bear that.
- A request GitHub accepted and a person's re-run in the same instant are not told apart: the row settles as accepted, which only makes billet ask less.
- What GitHub's job list says of a run with several jobs is documented rather than measured, as the [record](../records/job-reruns.md) sets out, and so is whether a re-run of one job also runs the jobs that need it. A job re-run through an App holding `actions: write` was not measured, because the shared test App holds `actions: read`.
