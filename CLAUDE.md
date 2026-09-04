# billet

Self-hosted GitHub Actions runners on your own hardware, with the cloud as fallback and a cache beside the compute. One Go binary, two roles: `billet server` is the control plane (it long-polls GitHub's Runner Scale Set API, owns the capacity ledger, and tells nodes what to launch); `billet node` is a compute host (it runs one provider and launches instances). A single machine runs both as two processes over loopback; there is no combined mode. Pre-alpha, Apache-2.0, Go 1.26, `CGO_ENABLED=0` everywhere so a node is deployed by copying one static file. The docs site is https://billet.readthedocs.io/en/latest/ and its sources are `docs/`.

## Writing prose: one paragraph, one line

Never hard-wrap prose at a column. Every paragraph in a `.md` or `.txt` file, a skill, an issue, a PR description or a commit body is one line, and the renderer wraps it. A wrapped paragraph reflows on every edit, is unreadable in a GitHub textarea, and turns a one-word change into a rewrite. The only exception is source code: Go and shell comments follow the surrounding file, which wraps at 88 columns.

## Map

| Path | What it is |
|---|---|
| `cmd/billet` | The binary: the `server` and `node` roles and the whole operator CLI (`init`, `github-app`, `check`, `local *`, `ca *`, `nodes *`, `leases *`, `drain`, `images *`, `ami *`, `rollout *`, `host-upgrade`, `cache *`, `acceptance *`, `teardown`, `decommission`). The only package allowed stdout and `os.Exit`. |
| `internal/config` | `billet.yaml` schema and validation. A leaf: it imports nothing else of billet's. |
| `internal/state` | The ledger: SQLite or PostgreSQL behind one seam, migrations (`migrations/`, `pgmigrations/`), the sqlc query set (`queries/`, `ledgerdb/`), locks, fences, the controller claim. |
| `internal/alloc` | The capacity allocator: escrow, leases and their state machine, placement, floors, the compute barrier, quarantine, force operations, enrollment records. |
| `internal/server` | One scale-set listener per tier and the scheduler; the message lifecycle against GitHub. |
| `internal/scaleset`, `internal/github`, `internal/fakeactions` | The only importer of `actions/scaleset`; App onboarding, JWTs, installation and runner-group policy; the scripted stand-in for GitHub used by tests. |
| `internal/nodeapi`, `internal/nodeclient`, `internal/nodeplane`, `internal/node` | The node wire: the vocabulary and version range, the node's half, the plane's half, and the node runtime that turns leases into compute and holds custody of what it cannot account for. |
| `internal/provider` + `docker`, `firecracker`, `tart`, `ec2`, `codebuild` | The compute contract and the five backends. |
| `internal/store` + `ceph`, `ebss3` | The cache-volume contract and the two site stores. |
| `internal/wirecert`, `internal/wireshare`, `internal/deploymentid` | The node-wire CA and its rotation state machine; carrying an authority between controllers; the deployment identity. |
| `internal/runnerimages`, `internal/imagesource`, `internal/runnerrelease`, `internal/provenance`, `internal/releasesource`, `internal/guestassets` | The vendored runner-image declaration; fetching and verifying signed guest images; the runner-release deadline; which manifest produced the installed binary; what a billet release contains; scripts installed into every guest. |
| `internal/lifeops` (+ `launchd`), `internal/deployarchive`, `internal/archivestore`, `internal/durablefile` | The local service lifecycle on systemd and macOS; backup, restore and recover; the no-delete S3 hop; the one fsync ordering for installing a file. |
| `internal/rollout`, `internal/hostupgrade` | The durable fleet-upgrade decision and coordinator; the journaled transaction that replaces billet on one host. |
| `internal/awssig`, `awscreds`, `awsjson`, `awspolicy`, `awsquota`, `awss3`, `awsssm`, `awssts` | billet's own SigV4 signer and AWS clients; what S3 said in a refusal; least-privilege IAM generation. |
| `internal/initconfig`, `internal/wiring`, `internal/version`, `internal/tfclass`, `internal/tfpolicy` | Config generation for `billet init`; assembling the pieces the way the CLI does; the version; Terraform plan classification and IAM drift. |
| `internal/e2e`, `internal/integration` | The end-to-end suite (real plane, wire and runtime against `fakeactions`) and cross-package boundary tests. |
| `deploy/` | The systemd units, launchd plists, packaged config template and package scripts. |
| `ansible_collections/junioryono/billet` | The `host` and `development_host` roles and their scenario tests. |
| `terraform/modules` | The AWS infrastructure modules. |
| `actions/` | The published Actions: `stickydisk`, `setup-docker-builder`, `stop-docker-builder`, `build-push-action`. |
| `scripts/` | Guest image and kernel builds, release tooling, `install.sh`, rehearsals, repository gates, and the Go tests that execute those scripts. |
| `tools/lint` | billet's own analyzers (`parallelshared`, `rawsql`), a nested module so `go/analysis` never ships in the binary. |
| `docs/` | The Sphinx site: getting started, concepts, deploying, operating, reference (CLI, configuration, decisions, records). |

Layering is enforced by `depguard` in `.golangci.yml`, not by convention: `config` imports nothing; `provider` and `store` are siblings below the scheduler and import neither each other nor `server`, `node` or `cmd`; the ledger writers (`state`, `alloc`, `rollout`) reach no network, no subprocess and no upper layer.

## Commands

```bash
make check       # the pre-commit gate: no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources
make build       # ./bin/billet
make test        # go test -race -count=1 -covermode=atomic ./...   (coverage counters are part of the gate; they reorder goroutines)
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

Load the skill before starting, not after being stuck. Each holds rules that cost a debugging session to learn and are not visible in the code. If a change makes a skill wrong, fix the skill in the same PR. If you do repeatable multi-step work with no skill for it, say so and offer to write one.

| Skill | Load it when |
|---|---|
| `billet-git-flow` | branching, committing, pushing, opening a PR |
| `billet-checks-and-lint` | `make check` or a linter fails; adding an import across a layer; touching `.golangci.yml`, `tools/lint`, the Makefile or a build tag |
| `billet-testing` | writing or changing any test; a test passes suspiciously; a probe on this Mac |
| `billet-shell-gates` | editing any `.sh`, any Go that emits shell, or any gate whose verdict is an exit status |
| `billet-config` | `internal/config`, `internal/initconfig`, `billet.yaml`, `billet init`, tier/node/site validation |
| `billet-state` | migrations, queries, `internal/state`, the controller claim, anything that opens the ledger |
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
| `billet-releases-and-upgrades` | `.goreleaser.yaml`, the release workflows, `rollout`, `hostupgrade`, `billet rollout`, `host-upgrade` |
| `billet-github-protocol` | `internal/scaleset`, sessions and messages, `internal/github`, `teardown`, runner groups |
| `billet-infra-terraform-ansible` | any `.tf`, `classification.json`, `tfclass`, `tfpolicy`, the Ansible collection |

## House rules

- **A check answers three ways: yes, no, could not tell.** Could-not-tell never collapses into no. A failed read is not an absent file, a timed-out heartbeat is not a fenced lease, "not found" from an eventually consistent API is not "gone".
- **Permission comes from what is proved, never from what is present or broken.** Capacity is released on proof the compute is gone; a retire, a decommission, an abandon and a fleet handover each derive their permission from a positive proof, and every narrower rule ("is something missing", "is it live") shipped first and was wrong.
- **A rule about an API billet does not own is pinned to measured behaviour, with the date**, never to a reading of the documentation. When in doubt, write a probe and run it where the code runs.
- **Zero values are the safe ones.** `TeardownRequested`, `TrustUnknown`, `AdmissionUnknown`, an unrecorded epoch: each refuses or holds rather than proceeding.
- **Escrow before advertise; exactly one party renews a lease; a timer never authorises a teardown.**
- **One of each**: one signer, one SQLite driver import site, one scale-set client, one query directory, one toolset file, one durable-install ordering, one answer to where kernels live. A second copy is one that is wrong.
- **Assemble in tests the way the CLI does** (`internal/wiring`), and prove a mechanism is used, not only that it works.

## Comments in Go

Types and functions get a doc comment cut to what the name does not already say. A comment inside a function survives only where it marks something a reader would otherwise get wrong: an ordering requirement, a unit, a "this must not move". Do not narrate the next line, and do not write the history of the code; state the rule that holds now and keep the failure it prevents only where that failure is the reason.

## Working style

Deliver what was asked, at the scope asked. Make routine judgment calls yourself and check in only when two readings of the request would produce materially different work. If the request looks mistaken, say so in a sentence and continue rather than quietly narrowing or widening it. Prefer reading a whole file over sampling it; most mistakes in this repository's history came from patching a region without seeing what surrounded it. Delegate to a subagent only for a genuinely independent, wide investigation, never for work a handful of tool calls finishes and never to double-check your own work. Report what happened, not what should have happened: if a test fails, say so and show the output; if you skipped part of a task, say which part.

## Codex compatibility

This repository also supports OpenAI Codex, which reads the cross-tool standard paths. Claude's files are canonical and the standard paths are committed symlinks: `AGENTS.md -> CLAUDE.md`, and `.agents/skills/<name> -> ../../.claude/skills/<name>` for every skill. Never create a real file at a symlink path and never edit `AGENTS.md`. Add the `.agents/skills` symlink in the same PR that adds a skill, and keep `SKILL.md` frontmatter strict YAML (quote any description containing `:`), because Codex rejects YAML that Claude tolerates. `.codex/config.toml` raises `project_doc_max_bytes`; keep it if you touch Codex config.

## Platform support

Linux (amd64, arm64) and macOS (arm64). A Windows port needs an equivalent of the `flock`-based state lock.
