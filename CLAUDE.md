# billet

Self-hosted GitHub Actions runners on your own hardware, with a colocated cache. Go, Apache-2.0, single static binary. Pre-alpha.

## Architecture

One binary, two roles. `billet server` is the control plane: it long-polls GitHub's Runner Scale Set API for assigned jobs, owns the capacity ledger, and tells nodes what to launch. `billet node` is a compute host: it runs a provider and launches instances. A single machine runs BOTH, as two processes reading one config file and talking over loopback. There is no combined mode.

```
cmd/billet/          the binary: server | node roles, plus the whole operator CLI
internal/config/     billet.yaml schema + validation (a leaf package — imports nothing of ours)
internal/state/      SQLite control-plane store: capacity ledger, job history, process lock
internal/github/     App Manifest onboarding, App JWT, installation resolution
internal/alloc/      capacity allocator, placement, lease state machine
internal/server/     scale-set listeners, scheduler
internal/node/       node runtime: provider driver, capacity reporting, custody
internal/nodeplane/  the mTLS node wire: registration, command dispatch, incarnations
internal/provider/   firecracker | tart | ec2 | docker  (docker and ec2 are built)
internal/store/      site storage: ceph/ drives RBD through the rbd command
docs/                reference-hardware.md — the bare-metal host billet is measured against
```

Layering is enforced by `depguard` in `.golangci.yml`, not by convention: `provider` and `store` are siblings that may not import each other or the scheduler, and `config` may not import any other billet package.

## Commands

```bash
make check     # build + vet + fmt-check + lint + race test — the pre-commit gate
make build     # ./bin/billet
make test      # go test -race -count=1 ./...
make cover     # coverage profile + HTML report
make lint-fix  # auto-fix what can be auto-fixed
make cross     # build every target a node can run on — catches build-tag mistakes
make dist      # build the release artifacts locally, exactly as a tag would
```

`make check` is the gate and must be clean before every commit. When it fails, fix the cause; a lint failure is never suppressed with `nolint` (see the `billet-linting` skill).

## Skills

Load the skill before you start, not after you are stuck. Each one holds rules that cost a debugging session to learn and are not visible in the code.

| Skill | Load it when |
|---|---|
| `billet-git-flow` | committing, branching, pushing, opening a PR |
| `billet-capacity` | touching `alloc`, `server/listener.go`, `nodeplane`, `node`; anything about what a tier advertises |
| `billet-security` | credentials, tokens, the node-wire CA, node identity, anything that destroys compute |
| `billet-invariants` | migrations, the state store, tier or node policy validation |
| `billet-testing` | writing or changing any test |
| `billet-linting` | golangci-lint fails |
| `billet-release` | tagging a release, changing the systemd units or `.goreleaser.yml` |

If you do repeatable multi-step work with no skill for it, say so and offer to write one. If a change makes a skill wrong, fix the skill in the same PR.

## Writing prose: no early line breaks

**Never hard-wrap Markdown, GitHub issue bodies, PR descriptions, or commit message bodies at a fixed column.** Write each paragraph as one long line and let the renderer wrap it. A hard-wrapped paragraph is unreadable in a GitHub textarea, produces a diff on every edit where whole paragraphs reflow, and makes a one-word change look like a rewrite. This applies to `CLAUDE.md`, every `SKILL.md`, `README.md`, `docs/**`, issues, PRs, and comment bodies.

Go source comments are the exception: those follow the surrounding file, which wraps at 88 columns.

## Comments in Go

Types and functions get a doc comment; cut it to what the name does not already say. A comment inside a function survives only where it marks something a reader would otherwise get wrong — an ordering requirement, a unit, a "this must not move". Do not narrate what the next line plainly does, and do not write the history of the code ("this used to…", "the first version…"): state the rule that holds now, and keep the failure it prevents only where that failure is the reason.

## Working style

Deliver what was asked, at the scope asked. Make routine judgment calls yourself and check in only when two readings of the request would produce materially different work. If the request looks mistaken, say so in a sentence and continue rather than quietly narrowing or widening it.

Prefer reading a whole file over sampling it. Most of the mistakes in this repo's history came from patching a region without seeing what surrounded it.

Delegate to a subagent only for a genuinely independent, wide investigation. Do not delegate work you can finish in a handful of tool calls, and do not use a subagent to double-check your own work.

Report what happened, not what should have happened: if a test fails, say so and show the output; if you skipped part of a task, say which part.

## Gotchas

- **`config` is a leaf package.** It imports nothing else in billet, and `depguard` enforces it. Validation rules that `alloc` also needs are exported from `config` and called from both, never copied.
- **`alloc.New` cannot assume its catalogue came through `config.Load`.** It is exported, so it re-applies the safety-critical rules itself. A rule enforced in only one of the two has a second entry point that does not enforce it.
- **The example config and the packaged config are both tested.** `billet.example.yaml` and `deploy/billet.yaml` are parsed by the config test suite, so a renamed or moved key breaks the build rather than an operator's install.
- **A loopback `server.listen` serves plain HTTP.** That is the single-machine shape, and setting `node.tls` against it is a config error rather than an upgrade.
- **`make cross` before anything touching build tags.** The `flock` state lock is `//go:build unix`; CI cross-builds linux/amd64, linux/arm64 and darwin/arm64.
- **Never guess a byte size.** Use `config.ByteSize`; a bare int of bytes is how a memory limit ends up 1024x wrong.
- **A node's contribution is DETECTED only for a backend that runs work on that machine.** `ProviderKind.RunsOnHost` is the one reader of that distinction. An `ec2` node is an orchestrator — it calls an API and the compute appears in a region — so `node.max_vcpu`/`node.max_memory` are required rather than defaulted, and the overcommit warning is skipped because there is nothing local to compare against. The ORDER of a tier's `providers:` list is what makes home fill first. Those two numbers are NOT yet a spending limit, and reading them as one is a mistake this repo has already made in writing: the allocator charges a job the size its TIER asked for, while the ec2 backend buys the first declared shape that FITS — so an 8-vCPU shape serving a 2-vCPU tier spends four times what the budget appears to allow (#47). The allowlist in `RunsOnHost` is deliberately keyed on the host backends, so a second remote backend nobody remembers to add is treated as remote.
- **billet signs its own AWS requests rather than taking the SDK, and that is measured.** A program doing nothing but constructing an EC2 client and calling `RunInstances` once builds to **13.2MB against a whole billet of 21.8MB**, and pulls 15 modules against billet's 4 direct ones — carried by every node in a fleet, including the bare-metal hosts the cloud backend exists to fall back FROM. What makes the trade safe is that the signer's output is pinned to **AWS's own signer's output**, not to a reading of the specification: `internal/provider/ec2/sign_test.go` carries vectors generated by `aws-sdk-go-v2` and the snippet that regenerates them. Only env-var and IMDSv2 credentials are supported; anything else (SSO, shared profiles, assume-role) has to be projected into the environment.
- **Storage is a property of the SITE, not of the compute backend.** `node.ceph` is a sibling of `node.firecracker` rather than a field inside it, because two hosts in one place map the same pools — that sharing is the entire reason Ceph replaced ZFS. It is required for `provider: firecracker` and REFUSED for every other backend: a container has nowhere to attach a block device and an ec2 node's compute runs in a region that cannot reach the cluster, so a storage block on either is a deployment that believes it has a cache and does not. `node.ceph.user` defaults to `billet` and `admin` is refused, which matters more than it looks — `admin` is what the `rbd` command picks for itself when nothing names an identity, and an admin key can delete a pool. billet shells out to `rbd` rather than linking librados, because the Go binding is cgo and would end the single static binary. **Every rule about a Ceph name is pinned to what running rbd does, and the first version of them was not** — a pool beginning with `-` is refused because billet builds POSITIONAL `pool/image` specs and `rbd info -weirdpool/x` answers `unrecognised option`, NOT because of `-p`, which consumes whatever token follows it; the identity rules written from the same false premise were removed, keeping only the `client.` prefix, which measurably authenticates as `client.client.billet`. Ceph is far more permissive than it looks — it creates pools named `a/b`, `a@b` and `a b` — so "is this legal" is never the question; "does billet address it correctly" is.
- **Registration and the operator CLI both work against a running control plane.** `Plane.Register` never asks whether a host was declared — it checks the protocol version, a non-empty name, the deployment identity, a non-zero contribution, and that the site is one the deployment declares (`WithSites`, wired from `cfg.SiteNames()`), after which the allocator requires a provider and refuses a provider or site change while leases are outstanding. So `nodes:` is policy about hosts rather than a roster of them. Operator commands reach the ledger through `state.OpenAdmin`, which does **not** take the exclusive state-directory lock when a control plane already holds it — the lock stops two control planes, not two processes, and a one-shot command is not one. It still takes the lock when free (a fresh install runs `ca issue` before any server), and when it cannot, it VERIFIES the schema instead of migrating: a newer CLI must never upgrade a database a running plane is mid-transaction against, so it returns `ErrSchemaBehind` naming which side to restart. Only `runServer` uses `state.Open`. Two rules follow from a second process writing: every write transaction begins IMMEDIATE (`_txlock`), because a DEFERRED one that reads first cannot promote its snapshot once anything else commits and fails with `SQLITE_BUSY_SNAPSHOT` — which `busy_timeout` cannot rescue; and contention is retried rather than returned, because an escrow error stops the listener and destroys every job on the host — the control plane retrying until its context ends, an operator command giving up after `adminBusyLimit` because a person is waiting on it. A read-only operation therefore belongs on `DB.View` (the query-only pool), never `DB.Tx`, or it reserves the single writer slot while it scans. Tiers are read at startup and each becomes one scale set, and `nodes:` is snapshotted into `alloc.New`, so changing either needs a restart.

## Decisions written down

`docs/adr-001-control-plane-hosting.md` — where the control plane runs and what stores its state. A single small EC2 with SQLite on EBS, recovered by EC2 auto-recovery (NOT an ASG — an ASG launches a fresh instance that does not reattach the data volume). DynamoDB was considered and rejected for now: feasible, saves nothing, costs a rewrite of the two most invariant-dense packages.

The fact that resized that decision: **GitHub queues a job for 24 hours when no matching runner is available.** A dead controller delays CI rather than failing it, so the requirement is "recovers in minutes", not HA.

`docs/adr-002-cloud-compute-backend.md` — which AWS service runs a job, and why billet signs its own requests. EC2 instance-per-job, because a GitHub Actions job runs Docker and Fargate cannot (*"privileged containers or access are currently unavailable on Fargate"* — so no Docker-in-Docker, and no block device for the caching plane either). Lambda is capped at 15 minutes against a 6-hour job; CodeBuild would replace billet rather than serve it. The SDK was rejected on a measurement: 13.2MB and 15 modules for one `RunInstances`, against a whole billet of 21.8MB and 4 direct dependencies, carried by every node including the bare-metal ones this exists to fall back from.

**An ec2 node is a serial launch queue.** The node runtime executes one command at a time (`Poll` → `execute` → `Report`), which is unremarkable for a backend where one node is one machine's worth of jobs and very visible for one where a single node can represent 64. The command timeout is 10 minutes, ample at normal latencies and tighter under throttling-plus-retries. Run several ec2 nodes if that binds; each is separately registered with its own budget.

`docs/adr-003-ceph-rbd.md` — why every cache is an RBD image in a Ceph cluster on the nodes' own NVMe, and why the ZFS pool it replaced was destroyed rather than kept beside it. A ZFS clone exists only on the machine that took it, which was the storage half of billet being a one-machine product; the property was verified by mapping a clone from a second kernel client. It also records what building it measured: clone + map + mount is ~350ms for a 10GiB image, sequential reads reach 4.5 GB/s at depth (the ≈2 GB/s the caching plane assumes), and 4k synchronous writes are 14x slower than the same disks locally — the one number that could still sink #26. Two findings there are load-bearing rather than trivia: **clone v2 is a requirement, not a preference** (as `cephadm` leaves a cluster, a snapshot must be protected before it can be cloned, and a protected snapshot with a live clone cannot be deleted — so eviction would be blocked by ordinary traffic), and Ubuntu 26.04's uutils coreutils breaks `cephadm bootstrap` in a way that reads as a Ceph bug.

**The cluster is a precondition billet CHECKS, not one it hopes for.** `billet check` refuses a cluster whose `require-min-compat-client` predates mimic, because on clone v1 a protected snapshot with a live clone can be neither unprotected nor removed — so a cache generation any running job holds would be undeletable and #25's eviction would be blocked by ordinary traffic. Refusing now is a judgement about when the cost lands: nothing clones yet, so the fix is one command on an empty cluster. The rule is written as "not one of the releases OLDER than mimic", because that set can never grow while a list of recent releases goes stale on the next Ceph release and starts refusing correct clusters. Pool replication is reported and not judged — how many copies to keep is the operator's decision, but it is invisible from the config file.

**A real build on RBD costs 2%, and the fio number that said otherwise was measuring a machine that does nothing but IO.** Measured: `go build` with a cold cache 1.02x, fsync-per-file 2.36x, and a COLD read of 40,000 small files **4.07x** — the expensive one, and the opposite of what the synthetic table predicted, because 40,000 small reads are IOPS-bound rather than sequential. That is also the cache-hydrate path, and the baseline that decides whether a sticky disk is worth having is re-fetching over the network, not a local disk a second machine cannot see. Readahead and ext4 stride alignment moved nothing; the defaults are what the numbers were taken on.

`docs/upstream-references.md` records what billet takes from other people's code and what it deliberately does not. Read it before reimplementing anything that touches the scale-set protocol. Two things from it that come up constantly:

- **`github.com/actions/scaleset` is the answer to most protocol questions**, and usually the only answer, because the API is not documented elsewhere.
- **billet is not actions-runner-controller without Kubernetes.** ARC does not track individual jobs; its whole scaling decision is `min(MinRunners+TotalAssignedJobs, MaxRunners)`, and Kubernetes absorbs scheduling, queueing and placement. billet has fixed hardware, a per-machine budget under a deployment ceiling, and placement constraints that need a lease bound to a specific host.

## Codex compatibility

This repo also supports OpenAI Codex, which reads the cross-tool standard paths. **Claude's files are canonical**; the standard paths are committed symlinks. Never create a real file at a symlink path, and never edit `AGENTS.md` — you would be editing the `CLAUDE.md` it points to.

- `AGENTS.md -> CLAUDE.md`
- `.agents/skills/<name> -> ../../.claude/skills/<name>` for every skill. **Add the symlink in the same PR that adds a skill**, and keep `SKILL.md` frontmatter strict YAML: quote any description containing `:`, because Codex rejects YAML that Claude tolerates.
- `.codex/config.toml` raises `project_doc_max_bytes`; keep it if you touch Codex config.

## Platform support

Linux (amd64, arm64) and macOS (arm64). A Windows port needs an equivalent of the `flock`-based state lock.
