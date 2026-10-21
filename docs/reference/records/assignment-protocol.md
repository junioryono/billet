# What GitHub.com does with an assignment

On 2026-10-21 billet measured, against GitHub.com, the behaviour [ADR-012](../decisions/adr-012-steady-advertisement.md) rests on and GitHub does not document: how long an assignment nobody runs is kept, whether capacity that appears is used at once, what the advertised ceiling counts, whether lowering it takes an assignment back, what a cancellation looks like on the wire, whether a new session's statistics restore a backlog, whether one message carries every assignment it counts, and how many message sessions one installation may hold (#140). It is a record of what GitHub answered on that day, not a promise about what it will answer.

## How it was measured

`TestLiveAssignmentProtocol` (`internal/integration/assignmentprotocol_live_test.go`) drives one measurement per run against a throwaway scale set of its own, through billet's own scale-set client: it opens a message session, polls at a scripted capacity, acknowledges every message, dispatches a workflow whose one job sleeps on the scale set's label, and writes every message, failed poll, statistics block and job status, with its time, to a JSON report. It records rather than asserts, and it fails only when the mechanics a finding rests on did not happen. It starts no runner except in U2, which starts one `ghcr.io/actions/actions-runner` container on a single-use registration for the first job assigned. Every run it dispatched was cancelled at the end, and every scale set it created was removed with `billet teardown`.

The App was a billet App installed on a free organization that exists only for testing billet; the dispatching repository was private; the probes ran on billet's shared Linux test host, three message sessions at a time except in U7. Each measurement ran once.

## What GitHub did

| | Question | Measured 2026-10-21 |
|---|---|---|
| U4 | How long is an assignment nobody runs kept? | One job, capacity 1, no runner, for 90 minutes. The job was assigned 7 s after it was queued, **cancelled 5 minutes later** (`JobCompleted`, result `canceled`) and reassigned 1–2 s after that under a new job id, twice; its **third assignment was kept until the probe ended, 80 minutes later**. Throughout, GitHub's REST API reported the job `queued`, and the message statistics counted it assigned. |
| U2 | What does the advertised ceiling count? | Capacity 2, five jobs queued, one runner started on the first assignment. **Assigned and running work together never exceeded 2**: with one job running, one more was assigned; when it finished, a second unrun job took its place. `TotalAssignedJobs` counted the running job too (2 assigned, 1 running at 1m04s). The unrun assignments cycled as U4's did: each job's first two were cancelled after about five minutes, and its third was still held when the 30-minute window ended. |
| U3 | Does lowering the advertisement take an assignment back? | **No.** Two jobs assigned at capacity 2, then every poll at capacity 0 for 20 minutes: both were cancelled at the ordinary five minutes (5m06s and 5m08s) and **neither was assigned again** while the advertisement stayed at 0. |
| U4-DROP | The same, with one job | Assigned at capacity 1, the advertisement dropped to 0: cancelled at 5m07s and **never reassigned** in the remaining 85 minutes. |
| U6a | Is capacity that appears used at once? | One job queued while advertising 0 for five minutes: **assigned 2 s after the advertisement rose to 1.** |
| U6b | Is the last advertisement honoured while a listener is silent? | **Inconclusive.** The phase-two session opened while phase one's assignment still held the scale set's one slot (it was cancelled only at 11m03s), so the queued job had nowhere to go whatever the answer. It needs a run on a fresh scale set. |
| C1 | Does a cancellation GitHub makes to reassign look different from an operator's? | **No.** Both arrive as `JobCompleted` with result `canceled` and request id 0. After the operator's cancellation GitHub assigned the run's job once more under a new job id and cancelled it in the same message. |
| C2 | Do a new session's statistics restore an acknowledged backlog? | **Yes.** Two jobs assigned and acknowledged, the session closed and a new one opened: its statistics said `TotalAssignedJobs: 2` (and `TotalBusyRunners: 2`, with no runner registered). |
| C3 | Does one message carry every assignment it counts? | **Yes.** Forty jobs queued at capacity 40: one message carried all 40 assignments, and its statistics said 40. |
| U7 | How many message sessions may one installation hold, and what does it say past that? | **Twenty held for an hour with no error.** Past twenty is unmeasured; see [below](#u7-many-sessions-on-one-installation). |

Every assignment, in every measurement, arrived as `JobAssigned` with `runnerRequestId: 0`, with no `JobAvailable` and no acquisition, as billet has measured since 2026-08-19.

## What billet takes from it

- **An assignment waits safely while billet buys room for it, within limits.** A job that billet has been assigned and has not started is cancelled after five minutes and reassigned, twice, and then held: so a job queued behind a full fleet is not lost to the protocol, but the ten minutes of churn before that are messages billet must treat as ordinary (`canceled` with no runner) and not as a failure. ADR-012's open question, the retention after the third reassignment, is answered for at least 80 minutes.
- **Lowering an advertisement does not return work; it only stops new work arriving.** A tier that drops its advertisement keeps what was assigned until GitHub's own five-minute cancellation, and then has nothing reassigned to it until it advertises again: which is why the seal lowers the advertisement and the drain counts assigned work separately (#140).
- **The ceiling is the scale set's total, running work included,** as `steadyAdvertisement` assumes: advertising what is committed while draining is the right number, and `TotalAssignedJobs` less the runners covering it is the count of work waiting.
- **A cancellation carries no cause,** so billet attributes a `canceled` completion by its own records, never by the message.
- **A restart's view of the queue is right:** a new session's statistics carry the acknowledged backlog, which is what `reportOrphanedBacklog` and the first reconciliation rely on.

## U7: many sessions on one installation

From 09:29 to 10:29 UTC on 2026-10-21, with every other user of the installation kept off it, one App installation held **20 message sessions open at once**, each on a scale set of its own, each long-polling at capacity 0 for the hour: 72 polls a session, about 50 seconds a poll, and **no 409, 429, 403 or other error on any open, poll or close**. Twenty sessions for an hour is therefore within what GitHub.com allowed one installation that day; more than twenty, and longer, are unmeasured.

The installation's REST budget was not recorded: the probe asked billet's scale-set client for the rate-limit headers it last saw each minute, and the client exposes none of the message API's, so every sample came back empty. The session limit is what this measured, not the REST budget a deployment's other calls spend.
