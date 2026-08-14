# Upstream references

What billet takes from other people's code, what it deliberately does not, and where to look when a
protocol question comes up. Written after reading the sources rather than their READMEs; every claim
here was checked against the code at the version named.

## `github.com/actions/scaleset` v0.4.0 — a dependency, not a reference

MIT, **public preview** — its own README says interfaces may change.

This is the GitHub-authored client for the runner scale-set API, extracted from actions-runner-
controller into a standalone module. Billet depends on it, and so does ARC, **at the same version**.
So the `listener` package inside it is not "some vendor's listener" — it is ARC's, and comparing
billet's behaviour against it is comparing against the thing GitHub ships.

Confined to `internal/scaleset` by a depguard rule (`scalesetclient` in `.golangci.yml`). A preview
dependency reaching into the scheduler is how a third-party release note turns into a rewrite.

Worth reading when a protocol question comes up, because the answer is usually in here and not in any
documentation:

| Question | Where |
|---|---|
| What does the message queue consider acknowledged? | `session_client.go` `getMessage` / `DeleteMessage` |
| Is `lastMessageId` a cursor or a note? | `session_client.go` `getMessage` — the source proves only that it is SENT; the doc comment promises an undeleted message returns again. How the queue filters on it is NOT established by anything readable. |
| What shape is a batched job message? | `client.go` `parseRunnerScaleSetMessageResponse` — the batch is a JSON **string** in `body` |
| Is there a way to decline an acquired job? | `session_client.go` — **there is not**; `AcquireJobs` is one-way |
| What is `maxCapacity`? | `listener/listener.go` — the scale set's TOTAL, sent unchanged as jobs are assigned |
| Which status means "no message"? | `getMessage` — `202`, while `200` carries a batch |
| How long does a long poll actually take? | **Measured, not read**: ~88s against a real org, not the ~50s widely assumed. Against a 90s lease TTL that is two seconds of margin. |

## `actions/actions-runner-controller` — a reference, not a dependency

Apache-2.0. GitHub's official Kubernetes controller for self-hosted runners.

**It is not what billet is rebuilding**, and the overlap is narrower than the surface suggests. Its
scaler is ~260 lines and its entire scaling decision is one expression:

```go
targetRunnerCount := min(w.config.MinRunners+count, w.config.MaxRunners)  // count = TotalAssignedJobs
```

`HandleJobCompleted` sets a dirty flag and returns nil. **ARC does not track individual jobs at all.**

| | ARC | billet |
|---|---|---|
| Scheduling and capacity | Kubernetes | own allocator |
| Capacity scope | `MaxRunners` per scale set, no cross-set budget | one global budget across tiers |
| Over-admission | pods sit `Pending` | escrow-before-advertise |
| Job tracking | none — a count | per-job lease state machine |
| Isolation | pods | Firecracker microVMs |
| Cache, sticky disks, layer cache | **none** | the reason this project exists |
| Deployment | requires Kubernetes | one binary |

**Why billet is not simply ARC-without-Kubernetes.** ARC can be that simple because Kubernetes
absorbs scheduling, queueing and placement. Billet has fixed hardware and must enforce its own global
budget across tiers, and its leases carry placement constraints Kubernetes would otherwise own —
CCD/NUMA locality, the Apple 2-VM licence cap, guest-OS allowlists, provider matching.

**The architectural difference worth understanding.** ARC is LEVEL-TRIGGERED: it recomputes desired
state from GitHub's authoritative `TotalAssignedJobs` on every message, so a missed event self-heals.
Billet is EDGE-TRIGGERED: it mutates per-job state as events arrive, so a missed event leaves state
permanently wrong. That is the origin of the whole "promise nobody follows up on" problem class.

It is tempting to conclude billet should copy the level-triggered model, and there is a real question
there (see task #14). But note the trap: ARC's reconciliation is safe because its action is
**idempotent and reversible** — set a replica count, and a wrong value corrects on the next message.
Billet's attempt at the same shape released leases, which is **destructive and irreversible**. Level-
triggered reconciliation is safe for reversible actions, not for revocations. Two attempts at that
were reverted; see `CLAUDE.md`, "A commitment made to a remote service cannot be revoked by a local
timer."

### What to take from ARC, and when

- **Now, taken:** its test approach. See below.
- **When the node runtime lands:** runner deregistration. `controllers/actions.github.com/`
  `ephemeralrunner_controller.go` — `cleanupRunnerFromService` / `deleteRunnerFromService`, built on
  `GetRunnerByName` + `RemoveRunner`. That is the recipe for reaping orphaned JIT registrations,
  which billet's plan requires and does not yet do. Billet's `internal/scaleset` does not expose
  `RemoveRunner` yet.
- **When observability lands (P6):** `cmd/ghalistener/metrics/metrics.go`, ~560 lines, is a
  ready-made catalogue of what is worth measuring about a listener.
- **Reference only:** JIT config handling (`createRunnerJitConfig` → a Kubernetes Secret). Billet
  injects into a VM instead, but the shape — one config, one runner, ephemeral — is the same and
  billet already implements it.

### What ARC does NOT have

No Actions cache interception, no Docker layer cache, no sticky disks, no microVM isolation, no
non-Kubernetes deployment. Everything that makes this project worth building is absent from it, which
is the real answer to "are we rebuilding ARC".

## Testing against a fake Actions service

**The most valuable thing taken from upstream so far.** Both the client and ARC test against an
`httptest` server rather than a live organization: `client_test.go` `newActionsServer` in the client,
`github/actions/testserver` in ARC.

Billet's version is `internal/scaleset/fakeactions_test.go`. It answers the App handshake —
installation token (**201**, not 200), registration token, and an RS256 admin token the client parses
for expiry — and delegates everything else to a per-test handler.

This matters because it reaches a class of bug nothing else does. Billet shipped `AcquireJobs` called
with ids from `JobAssigned` instead of `JobAvailable`; every unit test passed, because billet's own
types agreed with the mistake. It lived entirely in the relationship between billet and the wire.
`TestAnAvailableJobIsAcquiredByItsOwnRequestID` fails when that bug is reinstated.

Two rules learned building it:

- **Assert on what left the process**, not on billet's state. The acquirejobs body and the
  `X-ScaleSetMaxCapacity` header are the evidence; internal state is the thing that agreed with the
  bug. `maxCapacity` is a header, so it is invisible to any test inspecting billet's types, and it is
  the one number GitHub uses to decide how much work to send.
- **The fake is not a simulator.** It answers the handshake and serves what a test tells it to.
  Nothing it does is evidence of what the real service does — questions about GitHub's actual
  behaviour (task #13) still need a real organization.

## Others

- `tonistiigi/go-actions-cache` (MIT) — the cleanest Go reference for the **reverse-engineered**
  Actions Cache v2 Twirp protocol; `cache_v2.go`. GitHub has never published the `.proto` files
  (`actions/toolkit#1931`, open since Jan 2025). Reference for P4; it can break unilaterally, which
  is why that phase carries a conformance suite.
- `github-aws-runners/terraform-aws-github-runner` (MIT) — the proven shape for webhook-mode,
  instance-per-job runners on AWS. Reference for P12 if `JobSource` webhook mode is ever built.
- **Firecracker (Apache-2.0) — a service billet drives, not code it takes.** The interface is the VMM's own HTTP API over a unix socket plus the `jailer` binary, both installed by an operator, exactly as Ceph and Docker are. There is no Go SDK dependency and there does not need to be: the API is six PUTs and a GET.

  **Four claims this backend's design rests on were checked against upstream after being measured**, because each of them is a rule about somebody else's software and the repo's own rule is that those are pinned to measured behaviour. Three agree with the docs and two of those are worth stating anyway; two DISAGREE, and the disagreement is the interesting part.

  | Claim | Upstream says | Measured on v1.16.1 |
  |---|---|---|
  | There is no way to kill a microVM through the API | `action_type` is exactly `FlushMetrics`, `InstanceStart`, `SendCtrlAltDel` ([swagger](https://github.com/firecracker-microvm/firecracker/blob/main/src/firecracker/swagger/firecracker.yaml)) | agrees — a real guest ignored `SendCtrlAltDel` for twenty seconds, which is what sent billet to signalling the VMM process |
  | MMDS **V1 is the default**, and only V2 requires a session token before a GET returns anything | agrees ([mmds user guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/mmds/mmds-user-guide.md)) | agrees — the guest read the credential only after `PUT /latest/api/token` |
  | Seccomp is on by default and is compiled into the **firecracker binary**, not installed by the jailer | agrees, and notes debug builds and experimental GNU targets carry no filters ([seccomp](https://github.com/firecracker-microvm/firecracker/blob/main/docs/seccomp.md)) | not separately measured; the attribution in billet's own comment was wrong until this check |
  | The jailer builds its chroot at `<base>/<exec-file-name>/<id>/root` | agrees on the shape, and says it **does not resolve symlinks** ([jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)) | **DISAGREES**: passing `/usr/local/bin/firecracker`, a symlink to `firecracker-v1.16.1`, produced `/srv/jailer/firecracker-v1.16.1/<id>/root` |
  | The jailer writes `<exec-file-name>.pid` | says only when `--new-pid-ns` is passed | **DISAGREES**: written either way |
  | A guest's plain GET can retrieve **only** a JSON `string` or an `object` | agrees, in one sentence: *"Retrieving MMDS resources in IMDS format, other than JSON `string` and `object` types, is not supported"* ([mmds user guide](https://github.com/firecracker-microvm/firecracker/blob/main/docs/mmds/mmds-user-guide.md)) | agrees — and cost a day before it was read |

  **The MMDS row is the expensive lesson in this table**, and the only one where upstream was right,
  unread, and the failure it produced pointed somewhere else entirely. billet put a tier's command in
  the metadata as the `[]string` it is. The guest booted, took a DHCP lease, minted its session
  token, read the contract and read the registration — and then its fetch of `command` came back
  **501 Not Implemented**, `Cannot retrieve value. The value has an unsupported type.` (`format_imds`
  in `src/vmm/src/mmds/data_store.rs` returns `UnsupportedValueType`; `respond_to_get_request` in
  `src/vmm/src/mmds/mod.rs` maps it to the 501). The agent stopped, and a microVM that had booted
  perfectly ran nothing.

  Three things kept it hidden. `PUT /mmds` validates **nothing** — it accepts any JSON at all and
  checks only the size, so the host is told the tree is fine. The failure is **per-key**, so every
  other field kept working and exactly one was missing. And in a directory listing an array key is
  printed **without** the trailing slash that marks an object, so it reads as a fetchable leaf right
  up until it 501s.

  billet now sends a command as its JSON encoding in a string, and the guest agent parses it back —
  keeping the property worth having (billet never word-splits somebody's argv) inside what the
  service can actually serve. Having the agent send `Accept: application/json` would also work today
  and was rejected: `imds_compat: true` on `/mmds/config` makes Firecracker ignore that header, so it
  is a fix a later configuration change could silently undo.
  `TestEveryMetadataValueIsSomethingTheServiceCanServe` walks the whole tree and fails on any leaf
  that is not a string, so a field added later cannot fall into the same hole.

  **Neither disagreement is left to be right about.** billet passes the RESOLVED path as `--exec-file`, so the name the jailer derives is the same one billet pinned whether or not it resolves symlinks — the behaviours converge instead of having to be predicted. And it passes `--new-pid-ns`, so the pid file is the documented contract rather than an observed courtesy, while teardown still treats "an API socket with no pid file" as a state it cannot interpret rather than as proof the VMM has stopped. Both are cheap, and both fail in the direction that does not unmap a running guest's disk.

  The general shape is the one this file exists for: **measurement beats documentation about what a tool DOES, and documentation beats measurement about what a tool PROMISES.** Where they differ, build so the answer does not matter.

- **Ceph (LGPL-2.1) — a service billet drives, not code it takes.** The interface is the `rbd`
  COMMAND, deliberately: `go-ceph` is cgo over librados, which would end the single static binary and
  the cross-build matrix in one move — the same reason `mattn/go-sqlite3` is banned. So Ceph is an
  external dependency an operator installs, like Docker and Tart, and `internal/store/ceph` builds an
  argv. What that costs is a process per call, which is why it is for operations measured in tens per
  job and never per block. **Two behaviours are pinned to measurement rather than to the docs**, and
  both are in `docs/adr-003-ceph-rbd.md`: Ceph's own snapshot page still tells you to
  `rbd snap protect`, which on a clone-v2 cluster is unnecessary and on a clone-v1 one makes the
  snapshot undeletable while any clone is live; and the `rbd` man page lists `journaling` among the
  features the kernel client supports, while `rbd device map` refuses an image that has it.
