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
