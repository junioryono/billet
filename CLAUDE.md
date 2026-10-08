# billet

Self-hosted GitHub Actions runners on your own hardware, with the cloud as fallback and a cache beside the compute. One Go binary, two roles: `billet server` is the control plane (it long-polls GitHub's Runner Scale Set API, owns the capacity ledger, and tells nodes what to launch); `billet node` is a compute host (it runs one provider and launches instances). A single machine runs both as two processes over loopback; there is no combined mode. Pre-alpha, Apache-2.0, Go 1.26, `CGO_ENABLED=0` everywhere so a node is deployed by copying one static file. The docs site is https://billet.readthedocs.io/en/latest/ and its sources are `docs/`.

## Writing prose: one paragraph, one line

Never hard-wrap prose at a column. Every paragraph in a `.md` or `.txt` file, a skill, an issue, a PR description or a commit body is one line, and the renderer wraps it. A wrapped paragraph reflows on every edit, is unreadable in a GitHub textarea, and turns a one-word change into a rewrite. The only exception is source code: Go and shell comments follow the surrounding file, which wraps at 88 columns.

## Map

| Path | What it is |
|---|---|
| `cmd/billet` | The binary: `main`'s command tree, the `server` and `node` roles, and the two commands that compose several command families (`status`, `acceptance *`); every other command is a family under `internal/ops`. The only package allowed `os.Exit`; its `main` is the one place that names the process's streams and environment, and every command writes to the `cli.Env` it is handed. |
| `internal/cli` | The command line: the command tree and its dispatch, the `Env` a command writes to, the exit status a command answers with, the flag helpers and the signal lifecycle. Commands move here and under `internal/ops` one family at a time (#356 Phase 4). |
| `internal/ops/<family>` | The operator command families, each a package cmd/billet dispatches to and nothing else imports (host alone may import the others): `cache` (`billet cache`), `images` (`billet images`, `billet ami`, `billet runner`, the kernel and probe locks), `fleetops` (`billet leases`, `jobs`, `drain`, `resume`, `force-destroy`, `nodes`, `ca`, `teardown`, `decommission`), `setup` (`billet init`, `github-app`, the App key's publication), `host` (`billet check`, `local *`, `host-upgrade`, `converge-guard *`, `rollout *`, `release *`, `fleet`, `server retire`, `node migrate-endpoint` and `receipt`). A family writes to the `cli.Env` it is handed and opens the ledger through `internal/app`. |
| `internal/config` | `billet.yaml` schema and validation. A leaf: it imports nothing else of billet's. |
| `internal/state` | The ledger: SQLite or PostgreSQL behind one seam, migrations (`migrations/`, `pgmigrations/`), the sqlc query set (`queries/`, `ledgerdb/`), locks, fences, the controller claim; and `ledgertest`, the test-side template a test seeds its throwaway SQLite ledger from. |
| `internal/alloc` | The capacity allocator: escrow, leases and their state machine, placement, floors, the compute barrier, quarantine, force operations, enrollment records. |
| `internal/lease` | The words a lease is described in: its phases, the lease record, and what a node observed its job do (cache outcomes, disruptions, usage). The state machine and every write stay in `alloc`; the node wire names a lease through this package, and it imports nothing of billet's but `config`. |
| `internal/dispatch` | The words the scheduler and a compute host share to turn a lease into compute and back: the `Runner` contract and its completion-aware variants, the `Job`, the `CacheAuthority` a completed job carries, `ErrCustody` and `ErrHolderUnavailable`. The node runtime names these here rather than importing `server`; it imports nothing of billet's but `lease`. |
| `internal/server` | One scale-set listener per tier and the scheduler; the message lifecycle against GitHub. |
| `internal/scaleset`, `internal/github`, `internal/fakeactions` | The only importer of `actions/scaleset`; App onboarding, JWTs, installation and runner-group policy; the scripted stand-in for GitHub used by tests. |
| `internal/nodeapi`, `internal/nodeclient`, `internal/nodeplane`, `internal/node`, `internal/endpoint` | The node wire: the vocabulary and version range, the node's half (and the registration record it publishes after every accepted registration), the plane's half, the node runtime that turns leases into compute and holds custody of what it cannot account for, and the one representation of a control-plane endpoint that the node's request base, its record and the inspector's comparison share. |
| `internal/provider` + `docker`, `firecracker`, `tart`, `ec2`, `codebuild`, `simulated` | The compute contract and the six backends; `simulated` starts no compute, exists for billet's own test harness, and is refused in a config. |
| `internal/store` + `ceph`, `ebss3` | The cache-volume contract and the two site stores. |
| `internal/wirecert`, `internal/wireshare`, `internal/deploymentid` | The node-wire CA and its rotation state machine; carrying an authority between controllers; the deployment identity. |
| `internal/runnerimages`, `internal/imagesource`, `internal/runnerrelease`, `internal/provenance`, `internal/releasesource`, `internal/guestassets` | The vendored runner-image declaration; fetching and verifying signed guest images; the runner-release deadline; which manifest produced the installed binary; what a billet release contains; scripts installed into every guest. |
| `internal/lifeops` (+ `launchd`), `internal/deployarchive`, `internal/archivestore`, `internal/durablefile`, `internal/regularfile` | The local service lifecycle on systemd and macOS; backup, restore and recover; the no-delete S3 hop; the one fsync ordering for installing a file; the one open of a file by pathname that waits on no FIFO and opens no device. |
| `internal/hostauthority` | What a command holds before it touches an identity directory: the exclusion the host's metadata requires (the global lock on a prepared host, the inner lock otherwise), the lock beside a directory a fresh initialisation creates, the lifecycle lock between two lifecycle commands, and the hand-back of what a privileged command created to the service account. |
| `internal/metrics` | The one Prometheus registry and the endpoint that serves it (ADR-016): off unless a role's `metrics` block names an address, loopback unless `allow_remote`, the profiler only on loopback. The only importer of the Prometheus client and `net/http/pprof`. |
| `internal/supervise` | The group a process's long-lived loops run in: an essential loop's error stops the others, a background loop stops nothing and runs until the essential ones have returned, and every loop is joined before the group returns. The standard library only. |
| `internal/rollout`, `internal/hostupgrade` | The durable fleet-upgrade decision and coordinator; the journaled transaction that replaces billet on one host. |
| `internal/awssig`, `awscreds`, `awsjson`, `awspolicy`, `awsquota`, `awss3`, `awsssm`, `awssts` | billet's own SigV4 signer and AWS clients; what S3 said in a refusal; least-privilege IAM generation. |
| `internal/initconfig`, `internal/app`, `internal/version`, `internal/tfclass`, `internal/tfpolicy` | Config generation for `billet init`; the composition root, both roles' assembly with the control plane's startup order held as proof types, which the CLI and the harnesses alike build through (ADR-015); the version; Terraform plan classification and IAM drift. |
| `internal/e2e`, `internal/integration` | The end-to-end suite (real plane, wire and runtime against `fakeactions`) and cross-package boundary tests. |
| `internal/replay`, `internal/importcheck` | The trace replay harness: a workload driven through the real listener, allocator, placer and node over the simulated backend at compressed time, read back from the ledger; and the one walker that proves a test-side package has no production importer. |
| `deploy/` | The systemd units, launchd plists, packaged config template and package scripts. |
| `ansible_collections/junioryono/billet` | The `host` and `development_host` roles and their scenario tests. |
| `terraform/modules` | The AWS infrastructure modules. |
| `actions/` | The published Actions: `stickydisk`, `setup-docker-builder`, `stop-docker-builder`, `build-push-action`, and `converge-fleet`, which runs the collection from its own checkout. |
| `scripts/` | Guest image and kernel builds, release tooling, `install.sh`, rehearsals, repository gates, and the Go tests that execute those scripts. |
| `tools/lint` | billet's own analyzers (`parallelshared`, `rawsql`), a nested module so `go/analysis` never ships in the binary. |
| `docs/` | The Sphinx site: getting started, concepts, deploying, operating, reference (CLI, configuration, decisions, records). |

Layering is enforced by `depguard` in `.golangci.yml`, not by convention: `config` imports nothing; `provider` and `store` are siblings below the scheduler and import neither each other nor `server`, `node` or `cmd`; the ledger writers (`state`, `alloc`, `rollout`) reach no network, no subprocess and no upper layer.

## Commands

```bash
make check       # the pre-commit gate, one run per user at a time across projects (a shared lock): no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources
make build       # ./bin/billet
make test        # go test -race -count=1 -covermode=atomic ./...   (coverage counters are part of the gate; they reorder goroutines; locally -p 4 under nice)
make lint        # golangci-lint for the host AND GOOS=linux (a linter only sees files it would compile)
make lint-custom # tools/lint: build, run its own tests, run billetlint for darwin/arm64 and linux/amd64
make cross       # build linux/amd64, linux/arm64, darwin/arm64 — before anything touching a build tag
make docs        # Sphinx with -W, as CI and Read the Docs run it — after any change under docs/
make sqlc        # regenerate internal/state/ledgerdb after editing queries/ or adding a migration
make sqlc-check  # prove the committed query code is what the pinned sqlc generates
make tf-fmt-check tf-validate tf-test tf-lint tf-scan   # before pushing a .tf change
make tools       # install the pinned golangci-lint, goreleaser, sqlc, tflint, trivy
```

`make check` must be clean before every commit. What is outside it is outside for a stated reason, in the Makefile: the terraform gates need tools installed; `sqlc` is committed so an ordinary build never downloads it; `dist`, `acceptance`, the rehearsals and `systemd-lifecycle` need a package, a real account or a real service manager. CI runs all of that too, so a green `check` is necessary rather than sufficient. A lint failure is fixed at the cause, never suppressed without a reason.

## Skills

Load the skill before starting, not after being stuck. Each holds rules that cost a debugging session to learn and are not visible in the code. If a change makes a skill wrong, fix the skill in the same PR. If you do repeatable multi-step work with no skill for it, say so and offer to write one. `scripts/skills_test.go` holds what the skills said when they were frozen for the #356 rewrite: a sentence that no skill file says any more fails it unless `scripts/testdata/skills-dropped.txt` gives the reason, and it checks the frontmatter, the `.agents/skills` symlinks, one paragraph per line, every `references/` file linked from its `SKILL.md`, a `SKILL.md` of 12 KiB or less and a description of 600 characters or less. A skill is a short `SKILL.md`, one line per rule grouped by the reference file that states it in full with the measurement or incident behind it; read the reference before changing what a rule covers, and put a new rule's long form there too.

| Skill | Load it when |
|---|---|
| `billet-git-flow` | branching, committing, pushing, opening a PR |
| `billet-checks-and-lint` | `make check` or a linter fails; adding an import across a layer; touching `.golangci.yml`, `tools/lint`, the Makefile or a build tag |
| `billet-testing` | writing or changing any test; a test passes suspiciously; a probe on this Mac |
| `billet-shell-gates` | editing any `.sh`, any Go that emits shell, or any gate whose verdict is an exit status |
| `billet-config` | `internal/config`, `internal/initconfig`, `billet.yaml`, `billet init`, tier/node/site validation |
| `billet-state` | migrations, queries, `internal/state`, the controller claim, anything that opens the ledger |
| `billet-assembly` | `internal/app`, `runServer` or `cmdNode`, the proof types and their audit, `OpenNode`, the harnesses' assembly, the role-constructor bans |
| `billet-capacity` | `internal/alloc`, the listener, `nodeplane`, `node`; what a tier advertises; leases, custody, drains, teardown |
| `billet-node-wire` | routes, command kinds, wire types, protocol versions, enrollment, renewal, listeners |
| `billet-identity-and-ca` | deployment identity, `wirecert`, `wireshare`, the App key, `ca *`, `nodes *`, `github-app create` |
| `billet-security` | any key, token or credential; any code path that destroys compute; trust gates; redaction |
| `billet-backup-restore` | `local backup|restore|recover`, `deployarchive`, `archivestore`, `durablefile`, the kernel installer |
| `billet-lifecycle` | `local up|status|down|uninstall`, `drain`, `lifeops`, `deploy/`, package scripts, systemd or launchd facts |
| `billet-providers-local` | `internal/provider`, docker, firecracker, tart, the guest launchers |
| `billet-providers-aws` | ec2, codebuild, every `aws*` package, `billet ami`, `init iam`, `decommission` |
| `billet-storage-and-cache` | sites, `store`, ceph, ebs-s3, the Actions cache, the sticky-disk actions, `billet cache` |
| `billet-guest-images` | `runnerimages`, `imagesource`, `runnerrelease`, `billet images`, `runner check`, the image and kernel builds |
| `billet-releases-and-upgrades` | `.goreleaser.yaml`, the release workflows, `rollout`, `hostupgrade`, `billet rollout`, `host-upgrade`, the converge guard, `release inspect` |
| `billet-controller-retirement` | `billet server retire`, `internal/retirement`, the retirement row, `Inspector.AdmitOperations`, the collection's `retirement*.yml` |
| `billet-github-protocol` | `internal/scaleset`, sessions and messages, `internal/github`, `teardown`, runner groups |
| `billet-infra-terraform-ansible` | any `.tf`, `classification.json`, `tfclass`, `tfpolicy`, the Ansible collection |

A task skill is a checklist for one kind of change; it links to the area skills for the rules rather than repeating them.

| Task skill | Load it when |
|---|---|
| `billet-add-migration` | adding a ledger migration or a query |
| `billet-add-wire-change` | adding a route, a command kind or a wire field, or bumping `nodeapi.Version` |
| `billet-add-provider` | adding a compute backend under `internal/provider` |

`.claude/agents/` holds read-only reviewers for the three questions a diff most often gets wrong: `billet-layering-reviewer` (imports across layers), `billet-test-vacuity-reviewer` (tests that cannot fail) and `billet-wire-compat-reviewer` (the node wire across versions). They are a second reader for a change, not a substitute for the Codex review.

Eleven directories carry a short `CLAUDE.md` of their own (`internal/state`, `internal/alloc`, `internal/server`, `internal/nodeplane`, `internal/node`, `internal/provider`, `cmd/billet`, `internal/ops/host`, `deploy`, the Ansible collection, `terraform`), each with an `AGENTS.md` symlink for Codex: the invariants that bite there, the skill to load and the gates to run. A change that makes one wrong fixes it in the same PR. `ci-scope.sh` counts each of them as documentation by name, except the collection's, which its build packs like any other file there; a new one is added to that list only after checking that nothing reads its directory whole.

## Hooks

`.claude/settings.json` runs `.claude/hooks/guard.py` around Claude Code's tools. Before an edit it refuses `internal/state/ledgerdb/` (generated by sqlc), a migration file that already exists on main (its checksum is frozen in every deployment), and the Codex symlink paths; before a shell command it refuses what the git flow forbids (a force-push, a rebase, a push to main, a commit on main); after an edit to a Go file it runs gofmt. It reads the repository as it is, never as a command line would leave it, so a call that both switches branch and commits or pushes is refused (run them as two calls), and a push must name what it pushes (`git push -u origin <branch>`). A refusal names the rule it holds, and a question it cannot answer (no main to tell whether a migration is published) refuses rather than allows. `scripts/claude_hooks_test.go` executes every rule against throwaway repositories.

## House rules

- **A check answers three ways: yes, no, could not tell.** Could-not-tell never collapses into no. A failed read is not an absent file, a timed-out heartbeat is not a fenced lease, "not found" from an eventually consistent API is not "gone".
- **Permission comes from what is proved, never from what is present or broken.** Capacity is released on proof the compute is gone; a retire, a decommission, an abandon and a fleet handover each derive their permission from a positive proof, and every narrower rule ("is something missing", "is it live") shipped first and was wrong.
- **A rule about an API billet does not own is pinned to measured behaviour, with the date**, never to a reading of the documentation. When in doubt, write a probe and run it where the code runs.
- **Zero values are the safe ones.** `TeardownRequested`, `TrustUnknown`, `AdmissionUnknown`, an unrecorded epoch: each refuses or holds rather than proceeding.
- **A lease before every runner, never one reserved for an idle tier; exactly one party renews a lease; a timer never authorises a teardown.**
- **One of each**: one signer, one SQLite driver import site, one scale-set client, one query directory, one toolset file, one durable-install ordering, one answer to where kernels live. A second copy is one that is wrong.
- **Assemble in tests the way the CLI does** (`internal/app`), and prove a mechanism is used, not only that it works.

## Comments in Go

Types and functions get a doc comment cut to what the name does not already say. A comment inside a function survives only where it marks something a reader would otherwise get wrong: an ordering requirement, a unit, a "this must not move". Do not narrate the next line, and do not write the history of the code; state the rule that holds now and keep the failure it prevents only where that failure is the reason.

## Working style

Deliver what was asked, at the scope asked. Make routine judgment calls yourself and check in only when two readings of the request would produce materially different work. If the request looks mistaken, say so in a sentence and continue rather than quietly narrowing or widening it. Prefer reading a whole file over sampling it; most mistakes in this repository's history came from patching a region without seeing what surrounded it. Delegate to a subagent only for a genuinely independent, wide investigation, never for work a handful of tool calls finishes and never to double-check your own work. Report what happened, not what should have happened: if a test fails, say so and show the output; if you skipped part of a task, say which part.

The architecture program is tracked in #356: one assembly, a use-case layer, a pinned wire, and skills that load fast. A PR that is part of it says `Part of #356` and, in the session it merges, updates that issue's Progress table with its status, its link and what it measured; a finding that changes the plan is written into the issue body, with a dated line under Plan changes, before the PR that acts on it.

## Codex compatibility

This repository also supports OpenAI Codex, which reads the cross-tool standard paths. Claude's files are canonical and the standard paths are committed symlinks: `AGENTS.md -> CLAUDE.md`, and `.agents/skills/<name> -> ../../.claude/skills/<name>` for every skill. Never create a real file at a symlink path and never edit `AGENTS.md`. Add the `.agents/skills` symlink in the same PR that adds a skill, and keep `SKILL.md` frontmatter strict YAML (quote any description containing `:`), because Codex rejects YAML that Claude tolerates. `.codex/config.toml` raises `project_doc_max_bytes`; keep it if you touch Codex config.

## Platform support

Linux (amd64, arm64) and macOS (arm64). A Windows port needs an equivalent of the `flock`-based state lock.
