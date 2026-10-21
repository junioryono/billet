# billet

Self-hosted GitHub Actions runners on your own hardware, with the cloud as fallback and a cache beside the compute. One Go binary, two roles: `billet server` is the control plane (it long-polls GitHub's Runner Scale Set API, owns the capacity ledger, and tells nodes what to launch); `billet node` is a compute host (it runs one provider and launches instances). A single machine runs both as two processes over loopback; there is no combined mode. Pre-alpha, Apache-2.0 outside `ee/` (the billet Enterprise License inside it), Go 1.26, `CGO_ENABLED=0` everywhere so a node is deployed by copying one static file. The docs site is https://billet.readthedocs.io/en/latest/ and its sources are `docs/`.

## Writing prose: one paragraph, one line

Never hard-wrap prose at a column. Every paragraph in a `.md` or `.txt` file, a skill, an issue, a PR description or a commit body is one line, and the renderer wraps it. A wrapped paragraph reflows on every edit, is unreadable in a GitHub textarea, and turns a one-word change into a rewrite. The only exception is source code: Go and shell comments follow the surrounding file, which wraps at 88 columns.

## Map

| Path | What it is |
|---|---|
| `cmd/billet` | The binary: `main`'s command tree, the `server` and `node` roles, and the two commands that compose several command families (`status`, `acceptance *`); every other command is a family under `internal/ops`. The only package allowed `os.Exit`; its `main` is the one place that names the process's streams and environment, and every command writes to the `cli.Env` it is handed. |
| `internal/cli` | The command line: the command tree and its dispatch, the `Env` a command writes to, the exit status a command answers with, the flag helpers and the signal lifecycle. Commands move here and under `internal/ops` one family at a time (#356 Phase 4). |
| `internal/ops/<family>` | The operator command families, each a package cmd/billet dispatches to and nothing else imports (host alone may import the others): `cache` (`billet cache`), `images` (`billet images`, `billet ami`, `billet runner`, the kernel and probe locks), `fleetops` (`billet leases`, `jobs`, `drain`, `resume`, `force-destroy`, `nodes`, `ca`, `teardown`, `decommission`), `setup` (`billet init`, `github-app`, the App key's publication), `host` (`billet check`, `local *`, `host-upgrade`, `converge-guard *`, `rollout *`, `release *`, `fleet`, `server retire`, `node migrate-endpoint`, `node repair-unit` and `receipt`), `guest` (`billet guest monitor` and `flush`, which run inside a job's VM and open no ledger). A family writes to the `cli.Env` it is handed and opens the ledger through `internal/app`. |
| `internal/config` | `billet.yaml` schema and validation. A leaf: it imports nothing else of billet's. |
| `internal/state` | The ledger: SQLite or PostgreSQL behind one seam, migrations (`migrations/`, `pgmigrations/`), the sqlc query set (`queries/`, `ledgerdb/`), locks, fences, the controller claim; and `ledgertest`, the test-side template a test seeds its throwaway SQLite ledger from, and the successor's claim a test stages to replace a controller. |
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
| `internal/guestreport`, `internal/guestagent` | What the in-VM agent reports, the guest's own unverified view that no billet decision reads, and its one codec; the agent itself (`billet guest monitor`): the guest's `/proc`, the runner's step marks and JUnit results, sent to the node's guest-facing service. |
| `internal/metrics` | The one Prometheus registry and the endpoint that serves it (ADR-016): off unless a role's `metrics` block names an address, loopback unless `allow_remote`, the profiler only on loopback. The only importer of the Prometheus client and `net/http/pprof`. |
| `internal/flightrecorder` | The controller's opt-in execution-trace window (`server.flight_recorder`): written under the identity directory when a heartbeat pass overruns or the claim is lost, at most once per reason every ten minutes, the newest four kept. The one place the runtime's flight recorder is started. |
| `internal/supervise` | The group a process's long-lived loops run in: an essential loop's error stops the others, a background loop stops nothing and runs until the essential ones have returned, and every loop is joined before the group returns. The standard library only. |
| `internal/nodeending` | The node's own record of how each of its processes ended (`<node.state_dir>/endings.json`: drained, idle, handed off, left running, superseded, failed, or still `running` because it was killed), and the judgements that read it: a stop is proved by the stopped invocation's ending, never by an exit status, and a start is admitted by the newest one (#127). A leaf: the standard library, `durablefile` and `regularfile`. |
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
scripts/gates/verify.sh         # THE CHECK WHILE WORKING: gofmt, go build ./..., then vet, tests (no -race) and golangci-lint on the changed packages only; says what it ran and why; one run at a time per machine (the shared check lock)
scripts/gates/build.sh          # ./bin/billet
scripts/gates/docs.sh           # Sphinx with -W, as CI and Read the Docs run it (15 s); verify.sh runs it when docs/ changed and sphinx-build is installed
sqlc generate                   # regenerate internal/state/ledgerdb after editing queries/ or adding a migration (the pinned sqlc)
sqlc diff                       # prove the committed query code is what the pinned sqlc generates
scripts/gates/terraform.sh fmt validate test classify lint scan   # the terraform gates, which CI runs on a .tf change; they need scripts/gates/tools.sh
scripts/gates/scenario.sh NAME  # one of the Ansible collection's scenario tests, as CI's host-lifecycle groups run it
scripts/gates/dist.sh           # the release artifacts in dist/, as a tag builds them; the package rehearsals (scripts/test-*.sh, scripts/*-rehearsal.sh) install them
scripts/gates/bench.sh          # the allocator's and the node wire's benchmarks; a change to escrow, placement or renewal shows benchstat before and after
scripts/gates/fuzz.sh           # search past every fuzz target's seeds, FUZZTIME each (default 1m); the seeds already run in the suite
scripts/gates/tools.sh          # install the pinned golangci-lint, goreleaser, sqlc, tflint, trivy (.github/tool-versions.env)

# THE FULL GATE. CI runs it on every push; the hook refuses it to a session unless CLAUDE_FULL_CHECKS=1.
scripts/gates/check.sh          # no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources, one run per user at a time (a shared lock)
scripts/gates/test.sh           # go test -race -count=1 -covermode=atomic ./...   (coverage counters are part of the gate; they reorder goroutines; locally -p 4 under nice)
scripts/gates/lint.sh           # golangci-lint for the host AND GOOS=linux (a linter only sees files it would compile)
scripts/gates/lint-custom.sh    # tools/lint: build, run its own tests, run billetlint for darwin/arm64 and linux/amd64
scripts/gates/cross.sh          # build linux/amd64, linux/arm64, darwin/arm64, which CI's lint job runs for a build-tag change
```

**Verify narrowly here; CI runs the full gate.** While working, run `scripts/gates/verify.sh`: it reads what this checkout changed against its merge base with origin/main (commits, staged, unstaged and untracked files) and runs gofmt on the changed Go files, `go build ./...`, and `go vet`, `go test` (without `-race`, at `-p 2` and `GOMAXPROCS=4`) and golangci-lint for this platform on the changed packages. Run a new or changed test by name, `go test -run '^TestName$' ./internal/<pkg>`; a concurrency test may run under `-race` on its own package. For a build-tag change, `GOOS=linux GOARCH=amd64 go vet ./internal/<pkg>` covers the other platform. Do not run the full gates locally: `scripts/gates/check.sh`, `scripts/gates/test.sh`, `scripts/gates/lint.sh`, `scripts/gates/lint-custom.sh`, `scripts/gates/cross.sh`, `go test ./...`, `-race` across several packages, golangci-lint over the whole module, or golangci-lint on a file (packages only). CI runs all of that and more on every push; read it with `gh pr checks <n> --watch` and `gh run view <id> --log-failed`, and fix what it reports. The reason is measured on a 32 GiB development Mac on 2026-10-15: it kernel-panicked under memory pressure while sessions ran full gates side by side, and a targeted check costs a fraction (one package's tests 28 s and 0.9 GiB, the same under `-race` 491 s, golangci-lint on one package 10 s and 1.6 GiB against 52 s and 2.6 GiB for the module). `.claude/hooks/guard.py` refuses the full gates to a session; a person who wants `scripts/gates/check.sh` here starts Claude Code with `CLAUDE_FULL_CHECKS=1` in its environment, and a prefix on the command does not lift it. Subagents follow the same rule. A directory's `CLAUDE.md` names the gates CI must pass there; locally, `scripts/gates/verify.sh` runs the part of them that covers the change.

What is outside `scripts/gates/check.sh` is outside for a stated reason, in the script that says so: the terraform gates need tools installed; `sqlc` is committed so an ordinary build never downloads it; `dist.sh`, `acceptance`, the rehearsals and the systemd lifecycle need a package, a real account or a real service manager. CI runs all of that too. A lint failure is fixed at the cause, never suppressed without a reason.

## Skills

Load the skill before starting, not after being stuck. Each holds rules that cost a debugging session to learn and are not visible in the code. If a change makes a skill wrong, fix the skill in the same PR. If you do repeatable multi-step work with no skill for it, say so and offer to write one. `scripts/skills_test.go` holds what the skills said when they were frozen for the #356 rewrite: a sentence that no skill file says any more fails it unless `scripts/testdata/skills-dropped.txt` gives the reason, and it checks the frontmatter, the `.agents/skills` symlinks, one paragraph per line, every `references/` file linked from its `SKILL.md`, a `SKILL.md` of 12 KiB or less and a description of 600 characters or less. A skill is a short `SKILL.md`, one line per rule grouped by the reference file that states it in full with the measurement or incident behind it; read the reference before changing what a rule covers, and put a new rule's long form there too.

| Skill | Load it when |
|---|---|
| `billet-git-flow` | branching, committing, pushing, opening a PR |
| `billet-checks-and-lint` | `scripts/gates/verify.sh`, CI or a linter fails; adding an import across a layer; touching `.golangci.yml`, `tools/lint`, `scripts/gates/` or a build tag |
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
| `billet-add-metric` | adding a Prometheus metric: a gauge read at scrape time, or an event counted where it happens |

`.claude/agents/` holds read-only reviewers for the three questions a diff most often gets wrong: `billet-layering-reviewer` (imports across layers), `billet-test-vacuity-reviewer` (tests that cannot fail) and `billet-wire-compat-reviewer` (the node wire across versions). They are a second reader for a change, not a substitute for the Codex review.

Eleven directories carry a short `CLAUDE.md` of their own (`internal/state`, `internal/alloc`, `internal/server`, `internal/nodeplane`, `internal/node`, `internal/provider`, `cmd/billet`, `internal/ops/host`, `deploy`, the Ansible collection, `terraform`), each with an `AGENTS.md` symlink for Codex: the invariants that bite there, the skill to load and the gates to run. A change that makes one wrong fixes it in the same PR. `ci-scope.sh` counts each of them as documentation by name, except the collection's, which its build packs like any other file there; a new one is added to that list only after checking that nothing reads its directory whole.

## Hooks

`.claude/settings.json` runs `.claude/hooks/guard.py` around Claude Code's tools. Before an edit it refuses `internal/state/ledgerdb/` (generated by sqlc), a migration file that already exists on main (its checksum is frozen in every deployment), and the Codex symlink paths; before a shell command it refuses what the git flow forbids (a force-push, a rebase, a push to main, a commit on main), the full gates (`scripts/gates/check.sh` and its steps, `scripts/gates/test.sh`, `scripts/gates/lint.sh`, `scripts/gates/lint-custom.sh`, `scripts/gates/cross.sh`, `go test` over `./...` or with `-race` across several packages, golangci-lint over the whole module or on a file) unless Claude Code was started with `CLAUDE_FULL_CHECKS=1`, and a `git worktree add` outside `.claude/worktrees/` or a Claude scratchpad; after an edit to a Go file it runs gofmt. It reads the repository as it is, never as a command line would leave it, so a call that both switches branch and commits or pushes is refused (run them as two calls), and a push must name what it pushes (`git push -u origin <branch>`). A refusal names the rule it holds, and a question the git rules cannot answer (no main to tell whether a migration is published) refuses rather than allows; the gate and worktree rules are the exception and let through what they cannot read, and a `CI` environment skips them. `scripts/claude_hooks_test.go` executes every rule against throwaway repositories.

## House rules

- **A check answers three ways: yes, no, could not tell.** Could-not-tell never collapses into no. A failed read is not an absent file, a timed-out heartbeat is not a fenced lease, "not found" from an eventually consistent API is not "gone".
- **Permission comes from what is proved, never from what is present or broken.** Capacity is released on proof the compute is gone; a retire, a decommission, an abandon and a fleet handover each derive their permission from a positive proof, and every narrower rule ("is something missing", "is it live") shipped first and was wrong.
- **A rule about an API billet does not own is pinned to measured behaviour, with the date**, never to a reading of the documentation. When in doubt, write a probe and run it where the code runs.
- **Zero values are the safe ones.** `TeardownRequested`, `TrustUnknown`, `AdmissionUnknown`, an unrecorded epoch: each refuses or holds rather than proceeding.
- **Apache-2.0 outside `ee/`, the billet Enterprise License inside it** (`ee/LICENSE`, production use needs a subscription). No package outside `ee/` imports one inside it (depguard's `enterprise` rule), so the Apache build stands alone and an enterprise feature is an addition to it, never a dependency of it. Outside contributions are signed off under the Developer Certificate of Origin (`git commit -s`); CI's `dco` job (`scripts/dco.sh`) refuses an unsigned commit in any pull request the owner or a bot did not open. `ee/` takes no outside contribution without a written agreement with the licensor.
- **A lease before every runner, never one reserved for an idle tier; exactly one party renews a lease; a timer never authorises a teardown.**
- **One of each**: one signer, one SQLite driver import site, one scale-set client, one query directory, one toolset file, one durable-install ordering, one answer to where kernels live. A second copy is one that is wrong.
- **Assemble in tests the way the CLI does** (`internal/app`), and prove a mechanism is used, not only that it works.
- **Learn from other projects' published designs, and never take what is theirs without a license.** Open-source code billet copies into the tree keeps its upstream license and notice beside the copy, and its adoption is recorded with the version or commit it came from in `docs/reference/upstream-references.md` (for example `scripts/runner-images/LICENSE`, `internal/runnerimages/LICENSE`, `scripts/kernel/LICENSE` and `NOTICE`). What billet distributes carries the notices of what it contains: `scripts/mknotices` writes them from the linked modules and `scripts/mknotices/copies.json`, so a new copy gets an entry there, and its tests fail on a license file no entry lists. A short, attributed quotation from another project's documentation is fine where billet's record needs its exact words. Nothing else of another project's (its code, text, images and diagrams) is copied: describe billet's design in billet's own words, cite the source or the prior art it builds on in its decision record or `docs/reference/upstream-references.md`, and name another product only for a factual comparison a reader can check, never in a way that implies an affiliation.

## Comments in Go

Types and functions get a doc comment cut to what the name does not already say. A comment inside a function survives only where it marks something a reader would otherwise get wrong: an ordering requirement, a unit, a "this must not move". Do not narrate the next line, and do not write the history of the code; state the rule that holds now and keep the failure it prevents only where that failure is the reason.

## Working style

Deliver what was asked, at the scope asked. Make routine judgment calls yourself and check in only when two readings of the request would produce materially different work. If the request looks mistaken, say so in a sentence and continue rather than quietly narrowing or widening it. Prefer reading a whole file over sampling it; most mistakes in this repository's history came from patching a region without seeing what surrounded it. Delegate to a subagent only for a genuinely independent, wide investigation, never for work a handful of tool calls finishes and never to double-check your own work. Report what happened, not what should have happened: if a test fails, say so and show the output; if you skipped part of a task, say which part.

**Worktrees live under the main checkout's `.claude/worktrees/`**, never beside the repository in `../` and never in `/tmp`: `git worktree add .claude/worktrees/<name> -b <branch> origin/main`. The directory is ignored by git and excluded from lint, so a parked worktree never reaches another session's diff or lint pass; a Claude scratchpad is the one other place the hook accepts. Work only in your own worktree, and never edit the main checkout another session is using.

**No resident gopls.** `.claude/settings.json` turns off the `gopls-lsp@claude-plugins-official` plugin for this repository, because each session ran a gopls of its own that held 0.3 to 2.6 GB, a cold first index of a fresh worktree peaked at 3.6 GB, the shared-daemon mode (`-remote=auto`) saved 0 to 10% across separate worktrees, and the plugin's LSP tool was used in two sessions ever (measured 2026-10-15). Navigate with grep; when precise navigation is needed, run gopls one-shot, which exits afterwards: `gopls definition <file>:<line>:<col>`, `gopls references <file>:<line>:<col>`, `gopls implementation <file>:<line>:<col>`, `gopls check <file>`.

The architecture program is tracked in #356: one assembly, a use-case layer, a pinned wire, and skills that load fast. A PR that is part of it says `Part of #356` and, in the session it merges, updates that issue's Progress table with its status, its link and what it measured; a finding that changes the plan is written into the issue body, with a dated line under Plan changes, before the PR that acts on it.

## Codex compatibility

This repository also supports OpenAI Codex, which reads the cross-tool standard paths. Claude's files are canonical and the standard paths are committed symlinks: `AGENTS.md -> CLAUDE.md`, and `.agents/skills/<name> -> ../../.claude/skills/<name>` for every skill. Never create a real file at a symlink path and never edit `AGENTS.md`. Add the `.agents/skills` symlink in the same PR that adds a skill, and keep `SKILL.md` frontmatter strict YAML (quote any description containing `:`), because Codex rejects YAML that Claude tolerates. `.codex/config.toml` raises `project_doc_max_bytes`; keep it if you touch Codex config.

## Platform support

Linux (amd64, arm64) and macOS (arm64). A Windows port needs an equivalent of the `flock`-based state lock.
