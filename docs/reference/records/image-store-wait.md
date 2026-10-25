# How long a guest waits for its Docker image store

A guest's agent starts the runner only after its node answers for the Docker image store, because service containers are pulled before the first step and the store has to be mounted before Docker starts. Until #320 the guest waited up to 780 seconds for that answer and then up to 100 seconds for the device, so a slow store could keep a registered runner offline at GitHub for about fifteen minutes. This record is what the reference deployment measured on 2026-10-24, and the bound billet now keeps.

## What was measured

**Where:** the reference deployment's control-plane ledger, read with one read-only SQLite query on the controller (opened `mode=ro`, nothing written, nothing copied off the host), at 16:00 UTC on 2026-10-24. Its node is the Firecracker host described in [CPU placement](cpu-placement.md), on billet v0.25.0, where the node already bounded the clone or fresh volume at three minutes (#334).

**What:** every `job_history` row with a `launched_at`. That column has been recorded only since migration 65 reached the deployment, so the window is 2026-10-23 09:10Z to 2026-10-24 15:56Z: 31 hours and 3,344 leases. For each lease whose job was already queued at GitHub when billet launched the guest (`github_queued_at` at or before `launched_at`), the time from `launched_at` (billet's clock) to `github_runner_assigned_at` (GitHub's) is how long the guest took to boot, attach its store, start its runner and be handed the job. A lease launched before its job was queued measures how long the pool waited for work instead, so it is left out. `image_cache` is what the node observed the store do.

| Image store | Leases | Measured | p50 | p90 | p99 | Max |
|---|---|---|---|---|---|---|
| warm (a clone of a published generation) | 3,221 | 3,154 | 28 s | 69 s | 135 s | 199 s |
| cold (no generation, a fresh volume) | 6 | 6 | 16 s | 20 s | 47 s | 47 s |
| unavailable (the node gave up; the job pulled cold) | 26 | 12 | 171 s | 237 s | 239 s | 239 s |

Two clocks are compared, so a value carries their offset; the controller and GitHub both keep NTP time, and the smallest cold values (16 s for a boot, an agent and a runner connection) bound how large it can be.

## What it shows

- **No guest came near 780 seconds.** The slowest runner was handed its job 239 seconds after its launch, and every slow one was a store the node gave up on at its three-minute acquisition bound, after which the guest started Docker on its root disk.
- **A slow store costs the runner its job.** Of the 26 runners whose store was unavailable, 13 were never handed a job; of the 3,221 whose store was warm, 29 were not. A pool runner that comes up three minutes late finds its job taken by a runner that came up sooner, and is retired once it has sat idle (#319): the capacity is spent and nothing ran on it.
- **The unavailable stores came together.** All 26 fell between 2026-10-23 20:27Z and 2026-10-24 13:50Z, while the same node logged repeated failures to publish cache generations (ext4 verification and `rbd: error opening image`). The bound kept those hours to cold jobs; it did not make the store faster.

## The bound billet keeps

- **The node bounds the whole request at four minutes** (`dockerStoreRequestLimit` in `internal/node/cache.go`): the session lock, the kill switch, the acquisition (still three minutes of it, inside the four) and the attach, which on EC2 waits for AWS to report the volume attached. Every cleanup after a failure runs on the longer cache bound, so the expiry that ended an attach never cancels the detach and discard it requires.
- **The guest waits twenty seconds past that** (260 seconds in `internal/guestassets/docker-cache.sh`), then pulls cold. `TestTheGuestWaitsJustPastTheNodesDockerStoreBound` executes the guest's script and holds the two numbers together.
- **So a runner starts no later than its boot, plus 260 seconds, plus the wait for the device** (up to 100 seconds, which is EC2's udev link; a Firecracker slot's device exists from boot). That is about six minutes in the worst case, against fifteen before.
- **Each attach is logged once with how long it took**: `attached a Docker image store … cache=warm took=…`, and the warnings for a store that was unavailable carry `took` too. The next measurement is a `journalctl` query on the node rather than a ledger query.

An image built before this change still waits 780 seconds, and is answered within the node's four minutes. A node older than this change can still be working when a newer image gives up; it then attaches a volume nobody mounts, and the session's teardown discards it, as it does any store a guest did not use.

## What this does not establish

- How long the attach itself takes. The table measures launch to assignment, which includes the boot and the runner's connection; the node's `took` field measures the attach alone from this release on.
- Whether three minutes is the right acquisition bound. Lowering it would turn some warm stores cold (the slowest warm runner was handed its job at 199 seconds), and the attach timings above are what to choose it from.
- Starting the runner beside the attach, so GitHub sees it online before the store is mounted. That needs a job-started hook that waits for Docker before service containers start, and it is not built.
