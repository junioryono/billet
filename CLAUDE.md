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
internal/provider/   firecracker | tart | ec2 | docker  (only docker is built)
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
- **Adding a machine needs no restart; adding a tier does.** `Plane.Register` never asks whether a host was declared — it checks the protocol version, the deployment identity, a non-zero contribution, and that the site is one the deployment declares (`WithSites`, wired from `cfg.SiteNames()`). So `nodes:` is policy about hosts rather than a roster of them. Tiers are read at startup and each becomes one scale set, and `nodes:` reaches the allocator through `Limits.Nodes` when it is built, so changing either needs a restart.

## Decisions written down

`docs/adr-001-control-plane-hosting.md` — where the control plane runs and what stores its state. A single small EC2 with SQLite on EBS, recovered by EC2 auto-recovery (NOT an ASG — an ASG launches a fresh instance that does not reattach the data volume). DynamoDB was considered and rejected for now: feasible, saves nothing, costs a rewrite of the two most invariant-dense packages.

The fact that resized that decision: **GitHub queues a job for 24 hours when no matching runner is available.** A dead controller delays CI rather than failing it, so the requirement is "recovers in minutes", not HA.

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
