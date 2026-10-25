# How long a guest waits for its Docker image store

A guest's agent starts the runner only after its node answers for the Docker image store, because service containers are pulled before the first step and the store has to be mounted before Docker starts. Until #320 the guest waited up to 780 seconds for that answer, so a slow store could keep a registered runner offline at GitHub for well over ten minutes. This record is what the reference deployment measured on 2026-10-24 and 2026-10-25, and the bound billet now keeps.

## What was measured

**Where:** the reference deployment, on billet v0.25.0, where the node already bounded the clone or fresh volume at three minutes (#334). Two read-only sources:

- **The control plane's ledger,** read with read-only SQLite queries on the controller (opened `mode=ro`; nothing written, nothing copied off the host).
- **The Firecracker node's journal** (the host described in [CPU placement](cpu-placement.md)), for why each store that failed did so.

**What:** every `job_history` row whose `launched_at` falls between 2026-10-23 09:00Z and 2026-10-24 15:57Z. That column has been recorded only since migration 65 reached the deployment, so this is the first 31 hours of it: 3,344 leases.

- **Which leases are measured:** only those whose job was already queued at GitHub when billet launched the guest (`github_queued_at` at or before `launched_at`), and which GitHub then handed to that runner.
- **What is measured:** the time from `launched_at` (billet's clock) to `github_runner_assigned_at` (GitHub's) is how long the guest took to boot, ask for its store, start its runner and be handed the job.
- **What is left out:** a lease launched before its job was queued measures how long the pool waited for work instead.
- **`image_cache`:** what the node observed the store do.

| Image store | Leases | Handed a job | Measured | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|---|
| warm (a clone of a published generation) | 3,240 | 3,211 | 3,173 | 28 s | 70 s | 135 s | 199 s |
| cold (no generation, so a fresh volume) | 6 | 6 | 6 | 16 s | 20 s | 47 s | 47 s |
| unavailable (no store; the job pulled cold) | 26 | 13 | 12 | 171 s | 237 s | 239 s | 239 s |
| unused (the guest never asked) | 11 | 0 | 0 | | | | |
| none recorded | 1 | 0 | 0 | | | | |
| Tart guests (no image store) | 60 | 60 | 59 | 50 s | 76 s | 81 s | 83 s |

Two clocks are compared, so each value carries their offset. Both keep NTP time, and the offset was not measured.

**Why the 26 stores were unavailable,** from each one's warning in the node's journal:

| Cause | Stores | Handed a job |
|---|---|---|
| The acquisition ran into its deadline inside Ceph: the wait for the cache-index lock, or an `rbd` call that did not answer while taking, releasing or writing under it | 14 | 13 (all succeeded) |
| The microVM was already gone when the node came to attach the store | 11 | 0 |
| The VMM refused the drive (`PATCH /drives/cache0`) | 1 | 0 |

All 26 fell between 2026-10-23 20:27Z and 2026-10-24 13:50Z. In those same hours the node also logged repeated failures to publish cache generations (ext4 verification, and `rbd: error opening image`).

## What it shows

- **No runner that was handed a job waited anywhere near 780 seconds.** The slowest was handed its job 239 seconds after its launch.
- **The slow runners were the stores that ran into Ceph's trouble.** Those ran into the three-minute acquisition bound, after which the guest started Docker on its root disk. Thirteen of those fourteen were still handed their job and succeeded, about three minutes later than a warm one would have.
- **The runners never handed a job do not measure the wait.** Eleven had lost their microVM before the attach, and their compute was already gone. What a runner never handed a job waited for is not in this table.
- **The bound kept those hours to late, cold jobs; it did not make the store faster.**

## The bound billet now keeps

**The node bounds the steps toward the answer that take a context at four minutes** (`dockerStoreRequestLimit` in `internal/node/cache.go`). The session's own writes and the report to the control plane (at most two seconds) are outside it. The steps inside it are:
- the session lock;
- the kill switch;
- the acquisition, which keeps its three-minute bound but gets no more than what is left of the four;
- the attach, which on EC2 waits for AWS to report the volume attached.

**A failed attach keeps its volume.** This includes an attach the four minutes cut short. Nothing proves such an attach left the volume free: EC2 can complete an attachment it accepted after billet stopped waiting, its own timeouts and describe calls can fail after it did, and its describes are eventually consistent. So the node answers at once and does not race the attach with a detach. The volume stays in the session's custody, marked as never mounted, so no settlement waits for a readiness proof the cold guest will never send, and the guest's own discard of it is refused. The session's cleanup discards it once the compute is proved gone.

**A volume is recorded as never mounted from the moment it is in custody**, and only a successful attach, recorded durably, makes it the guest's. A restart in between, or a write that fails after the attach, leaves a record that still says never mounted. If that write fails, the guest is sent cold even though the attach succeeded, and the never-mounted state is written again. A write can fail after its rename, at the directory sync, and leave "mounted" installed. So a node that also crashed before the second write could reach the disk would find a store its settlement waits two minutes on.

**A clone the session could not record** is discarded before the answer, on a context the guest hanging up cannot cancel. Before this change the cleanup inherited the guest's connection, which the guest now closes twenty seconds after the node's bound.

**The guest waits 260 seconds for the answer** (`internal/guestassets/docker-cache.sh`), twenty seconds past the node's bound, and then pulls cold. `TestTheGuestWaitsJustPastTheNodesDockerStoreBound` executes the guest's script to hold the two numbers together.

**So the waits the agent can spend before Docker starts are:**
- that request, at most 260 seconds;
- then the wait for the device, at most 100 seconds, which is EC2's udev link (a Firecracker slot's device exists from boot);
- and, only on a path where the store arrived but cannot be used, one discard call of at most 120 seconds.

Those explicit waits add up to eight minutes at most, against about seventeen before (780, 100 and 120 seconds). On top of them come the filesystem check, the mount and Docker's own start, none of which has a deadline of its own. The common case is the table's first row.

**Each attach is logged with how long the whole request took:** `attached a Docker image store … cache=warm took=…`. The warnings for a store that was unavailable carry `took` too. The next measurement is therefore a `journalctl` query on the node, not a ledger query.

**Older and newer pieces together:**
- **An image built before this change** still waits 780 seconds. A newer node answers it once the steps the four minutes bound have run. Outside those minutes are the session's own writes, the report to the control plane (at most two seconds) and, when the session could not record the clone at all, the clone's discard, all before the answer.
- **Rolling a node back** past this change loses the never-mounted mark: an older binary reads a session's record without it, so a store still in custody reads as mounted, its settlement waits, and the guest's discard of it is allowed again. Drain a node before rolling it back, as a host upgrade already does.
- **A node older than this change** keeps its twelve-minute handler. A newer image hangs up on it at 260 seconds, which cancels that node's request mid-way. Whatever its session had recorded, the session's teardown discards.

## What this does not establish

- **How long the attach itself takes.** The table measures launch to assignment, which includes the boot and the runner's connection. The node's `took` measures the whole request from this release on, not the attach alone.
- **Whether three minutes is the right acquisition bound.** Lowering it would turn some warm stores cold: the slowest warm runner was handed its job at 199 seconds. The node's `took` values are what to choose it from.
- **The time a request spends before the node's handler starts.** That is the wait for a node that is not yet serving, the listener's connection limit, and on EC2 the TLS handshake. It counts against the guest's 260 seconds and not against the node's 240. If it took most of the twenty-second margin, the node could finish an attach the guest has already given up on. The node would then record `warm` for a job that pulled cold, and the store would be discarded at teardown.
- **EC2's retry of an attach.** Retrying an attach EC2 refused while the instance was still `pending` now has only what is left of the four minutes. So an acquisition longer than about two minutes can cut that retry short and turn a producer job cold.
- **Starting the runner beside the attach,** so GitHub sees it online before the store is mounted. That needs a job-started hook that waits for Docker before service containers start, and it is not built.
