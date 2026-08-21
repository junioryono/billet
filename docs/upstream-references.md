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
| Can `runnerRequestId` be zero? | **Yes.** A real GitHub.com run on 2026-08-19 delivered `JobAssigned` directly with `runnerRequestId: 0`, then repeated zero in `JobCompleted`; [actions/scaleset#107](https://github.com/actions/scaleset/issues/107) independently reports the same direct-assignment shape. `jobId` remains stable, so billet durably maps it into a collision-free negative namespace while positive request ids remain untouched. |
| What is `maxCapacity`? | `listener/listener.go` — the scale set's TOTAL, sent unchanged as jobs are assigned |
| Which status means "no message"? | `getMessage` — `202`, while `200` carries a batch |
| How long does a long poll actually take? | **Measured, not read**: ~88s against a real org, not the ~50s widely assumed. Against a 90s lease TTL that is two seconds of margin. |
| What if one completion can never be identified? | `DeleteMessage` acknowledges the whole batch, so billet first processes every valid completion, offer and assignment beside it idempotently. A completion with no resolvable local lease or job identity after three deliveries is logged at error level, discarded and acknowledged so it cannot keep every tier down in a systemd restart loop. Cross-tier identities, acquisition contradictions and ledger failures remain fatal because they can represent known or unknown capacity commitments rather than poisoned input. |

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
- **The fake is not a simulator.** It answers the handshake and serves what a test tells it to. Nothing it does is evidence of what the real service does. Billet's EC2 acceptance now proves the ordinary JIT registration and job path against a real organization, but a protocol behavior not exercised there still needs its own live observation.

The direct-assignment path is the concrete example. Every fake used positive request ids until a real job reached `JobAssigned` without a preceding offer and carried zero through completion. Zero is a wire value, not an identity: using it as a map key aliases concurrent jobs, and rejecting it stops the whole control plane after the first real completion. Billet now maps the message's stable `jobId` transactionally to a unique negative integer before assignment or completion handling. The sign keeps GitHub's positive namespace disjoint, the table makes redelivery and restart recover the same value, and an offered job still sends GitHub's original wire id to `AcquireJobs`. Multiple distinct offers sharing one wire id are refused because an acquisition response containing that id cannot say which promise GitHub accepted.

## `actions/runner` v2.336.0 — a pinned service and one fail-closed result contract

The supported `ACTIONS_RUNNER_HOOK_JOB_COMPLETED` hook is not an authoritative job-result channel: the runner registers it as an unconditional post-job step, and the documented hook environment exposes ordinary default variables but no conclusion. Billet instead opts its one-job runner into the same result path GitHub's hosted runners use by setting `ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true`. At the pinned v2.336.0 source, `src/Runner.Listener/Runner.cs` makes that setting force one-job mode, awaits `RunOnceJobCompleted`, and returns `TaskResultUtil.TranslateToReturnCode(jobResult)`; `src/Runner.Common/Util/TaskResultUtil.cs` defines the translation as `100 + TaskResult`, while `src/Sdk/DTWebApi/WebApi/TaskResult.cs` assigns `Succeeded = 0`, `SucceededWithIssues = 1`, `Failed = 2`, `Canceled = 3`, `Skipped = 4`, and `Abandoned = 5`. The stock `run-helper.sh.template` maps those hosted codes back to zero, so billet's service wrapper invokes `Runner.Listener` directly and preserves 100–105 for the guest's changed-store classification. The ordinary workflow commit route cannot directly publish slot zero, and an early readiness call is rejected until the node opens settlement. Live scale-set completions delivered `JobCompleted.result` as lowercase `succeeded`, matching the protocol fixture; billet accepts only that exact success token and treats every other or malformed result as non-success. Only that authoritative success opens the bounded settlement window; after `Runner.Listener` returns, the helper marks its unmounted, checked and changed store ready. That call is coordination, not a secret capability boundary: a workflow has passwordless sudo and Docker-root equivalence and can read any guest metadata or process environment. Publication authority therefore remains outside the guest in the node's trust classification plus GitHub's independent result. The control plane persists that result, its fenced lease identity and its bound holder before acknowledging the scale-set message. Settlement durably retires the message identity, while physical deletion waits until the source acknowledgement is also durable; a stale or retired delivery is rejected before teardown and cannot relaunch an assignment from the same batch. Restart recovery invalidates prior inventory as soon as a replacement registration begins, then orders the persisted holder's exact-incarnation inventory, ownership adoption and outcome-aware single-lease quarantine settlement under one node-plane serialization boundary; a generic known-empty fleet snapshot is never enough. The allocator preserves the authoritative completion outcome even when ordinary inventory reconciliation reached the same lease first. Thus a failed job cannot publish its image store by calling the cache API itself, while 101–105 plus every unknown code are discarded. These are upstream implementation contracts rather than documented self-hosted-runner APIs, so every runner bump must re-read those files and the live guest-image acceptance must prove both a successful publisher and a failed discarder before the new image is published.

## Others

- `docker/docker` Engine 29 storage contract — Docker's official storage documentation records that fresh Engine 29 installations default to the containerd image store and keep image content and snapshots in `/var/lib/containerd`, outside Docker's `/var/lib/docker` data root. Billet's image-store cache is one independently fenced block device mounted at `/var/lib/docker`, so both guest image builders explicitly set `features.containerd-snapshotter=false` and `storage-driver=overlay2`; the Firecracker image inspection gate verifies both before publication, while the EC2 provision-script test keeps the same contract in its AMIs. This deliberately trades the containerd store's multi-platform image and attestation support for a complete atomic image-store generation rather than claiming to cache a directory that no longer holds the images. Source: [Docker containerd image store](https://docs.docker.com/engine/storage/containerd/) and [Docker daemon data directory](https://docs.docker.com/engine/daemon/#daemon-data-directory).
- `docker/compose` v5.3.1 (Apache-2.0) — the pinned Compose CLI plugin for Amazon Linux guest images, where the distribution still does not package `docker-compose-plugin`. Billet downloads Docker's architecture-specific release asset into the standard CLI plugin directory and checks the published SHA-256 before an AMI can be created: `f9ebc6ebdb19d769b793c245a736caaeb198c62587f13b25c660c13b4987f959` for x86_64 and `aa611e811d0ea25897839c404bfb5bf93ce706dc51c500a4457890f5d0606a86` for aarch64. Ubuntu Firecracker images use the signed `docker-compose-v2` distribution package instead. Both image builders execute or inspect the plugin before publication because Docker being present does not imply that `docker compose` works.
- `actions/toolkit` at `193fa46c20fde8b0ed54194bc08b841c78c0776d` (MIT) — the primary wire reference for transparent Actions caching. `packages/cache/src/generated/results/api/v1/cache.twirp-client.ts` establishes the exact JSON Twirp service and three method names; `cache.ts` establishes request/response field use and fail-soft action behavior; `internal/uploadUtils.ts` and `internal/downloadUtils.ts` establish the Azure Block Blob upload, property lookup and range-download shapes; and `internal/shared/user-agent.ts` establishes the `@actions/cache-<package-version>` client identity. Billet uses that identity only to keep other clients on GitHub, not as authentication: the opaque runtime token remains undecoded, the VM cache capability authenticates the proxy, and server-supplied job identity scopes storage. Billet does not implement ArtifactService. Every client or path outside that exact local surface is upstream by default.
- `actions/runner` at `258d6c857db3519913f7deb6004b60172f8043ae` (MIT) — the primary container-propagation reference for #29. `RunnerWebProxy` reads the runner process's proxy environment; `ContainerInfo.UpdateWebProxyEnv` carries it into job, service and action containers; `ContainerActionHandler` and `ContainerStepHost` translate mounted host paths in action environments; and `JobHookProvider` processes `GITHUB_ENV` from `ACTIONS_RUNNER_HOOK_JOB_STARTED` before container initialization. Those measured source contracts are why the guest copies the combined trust bundle into `RUNNER_TEMP` and publishes its path through the supported hook rather than assuming inherited process environment appears inside a container. Interception itself reaches those containers by a DNS remap of the results origin — a guest resolver the Docker daemon is pointed at — not by a proxy variable, because a blanket proxy funnels every request the runner and its containers make through one guest relay and bulk transfers stall through it.
- `buildkite/buildkite-gha` at `027892a251938f3dc561a1ccf9339af736741fe7` (MIT) — a P4 cache-conformance reference, not an execution model. It replaces GitHub's runner with a Buildkite workflow compiler and compatibility runtime, so billet takes none of its parser, expression evaluator, action downloader, native checkout/artifact adapters or credential architecture. The useful overlap is narrower: its cache integration catalog identifies `actions/cache` plus the cache clients embedded in `actions/setup-node`, `actions/setup-java`, `actions/setup-python`, `actions/setup-go` and `actions/setup-dotnet`; its tests use synthetic actions to cover Node 20 and Node 24 metadata, fresh per-lifecycle credentials, environment isolation and every catalogued setup-action route, while a separate allowlist pins admitted `actions/cache` commits without executing those upstream clients. Billet keeps the official runner; its interceptor authenticates by VM cache session, never decodes GitHub's runtime token, and passes every unknown method upstream, so the action allowlist, subprocess isolation and per-lifecycle credential model do not transfer. The checked-in conformance workflow applies the transferable surface: real direct and embedded cache clients, a pinned-known-good lane beside a moving-major lane, and hostile `ACTIONS_*`, Node, CA, proxy, loader, tar and path variables followed by byte-checked artifact passthrough.
- `tonistiigi/go-actions-cache` at `664d1898071f863eec2164f90c5c4d6e4ac2a572` and `moby/buildkit` at `51c46f67b678820d5d491ee1371c448d80268432` (both MIT) — secondary Go references for the reverse-engineered Actions Cache v2 Twirp protocol and the accepted BuildKit boundary. The generic Go client defaults to `go-actions-cache/1.0`, while BuildKit's GHA exporter and importer explicitly replace it with `buildkit/<version>`; both use the same three v2 methods. Billet therefore selects only the official toolkit's `@actions/cache-` user agent for local handling and leaves BuildKit `type=gha` on GitHub as promised. GitHub has never published the `.proto` files (`actions/toolkit#1931`, open since January 2025), so the official toolkit source above remains authoritative and the live conformance workflow guards unilateral wire drift.
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

  **Clone chains need an explicit bound.** Ceph's layering documentation says each clone retains a reference to its parent and that severing the reference requires copying the shared blocks; Ceph CSI consequently documents a soft depth of four and a hard depth of eight. Measured on the reference cluster, `rbd cp source@snapshot destination` completed as an independent format-2 image whose JSON carried no parent, and the source stayed intact until the copy was ready. Billet uses that non-mutating copy at depth eight: unlike flattening a candidate in place, a crash cannot alter the published generation, and old ancestors become reclaimable while the new current generation remains active.

## Prior art for transparent Actions cache interception, checked and not taken

Checked when the live conformance suite exposed the results host's WebSocket traffic, because "surely somebody established has solved this" deserved an answer rather than an assumption. **Nobody publishes what billet's interceptor does.** [Ubicloud](https://www.ubicloud.com/blog/github-actions-transparent-cache) (the closest open-source neighbour, and the only one running transparent caching in production at scale) does it by **forking the runner**: their own write-up says they reviewed the runner source and forked it to point the cache-URL environment at a local proxy, so their proxy only ever sees cache requests and the forked runner keeps talking to GitHub directly for logs and step updates — they never face the shared-host problem at all. Blacksmith runs transparent caching on unmodified runners but publishes no code. Everything else ([falcondev's cache server](https://github.com/falcondev-oss/github-actions-cache-server), act/Gitea/runner.server's cache services, the Squid gist) requires overriding `ACTIONS_*` environment or replacing the runner, which is the path [toolkit#1051](https://github.com/actions/toolkit/issues/1051) has kept closed upstream since 2022. billet's constraint — the official, unpatched runner — is exactly the part none of them publish, and it is why billet carries the interception burden: the results host serves cache twirp, artifact metadata, step updates, log-archive uploads AND the live-log WebSocket ([actions/runner's own connectivity doc](https://github.com/actions/runner/blob/main/docs/checks/actions.md) requires wss to results-receiver), so everything billet does not handle locally is a raw byte splice, never a re-framed round trip. The splice technique itself is ordinary CONNECT-proxy machinery (mitmproxy, elazarl/goproxy); billet takes the technique and not a dependency, for the same reason it signs AWS requests instead of taking the SDK.
