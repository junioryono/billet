# Contributing to billet

billet is pre-alpha and the architecture is still moving, so an issue before a large pull request will save you time. Contributions should keep a change at the scope it was asked for, preserve the invariants the skills under `.claude/skills` write down, and include tests that fail when the code is wrong.

## Development setup

Requirements:

- Go 1.26.10 or later, as `go.mod` says. Everything builds with `CGO_ENABLED=0`, because a node is deployed by copying one static file, and a cgo dependency is refused by the linter.
- Python 3.13 for the documentation build and the repository's Python gates.
- bash and a POSIX shell for the gate scripts under `scripts/gates/`, and Docker for the end-to-end suite (it skips without one).

Clone the repository, install the pinned tools, and run the same gate CI runs:

```bash
git clone https://github.com/junioryono/billet && cd billet
scripts/gates/tools.sh     # golangci-lint, goreleaser, sqlc, tflint and trivy at the versions .github/tool-versions.env pins
scripts/gates/check.sh     # the full gate, as CI runs its steps
```

`tools/lint` is a nested Go module holding billet's own analyzers, so `golang.org/x/tools` never becomes a dependency of the binary. `scripts/gates/check.sh` builds and tests it for you.

## Verification commands

| Command | What it does | In `scripts/gates/check.sh`? |
|---|---|---|
| `scripts/gates/check.sh` | `no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources` | it is the gate |
| `scripts/gates/verify.sh` | gofmt, `go build ./...`, then vet, tests without `-race` and golangci-lint on only the packages this checkout changed against origin/main; the check a Claude session runs while working, since CI runs the full gate on every push | no |
| `scripts/gates/test.sh` | `go test -race -count=1 -covermode=atomic ./...`; the coverage counters are part of the gate because they reorder goroutines | yes |
| `scripts/gates/lint.sh` | golangci-lint (pinned) for the host **and** for `GOOS=linux`, because a linter only sees the files it would compile | yes |
| `scripts/gates/lint-custom.sh` | build `tools/lint`, run its own tests, run `billetlint` for darwin/arm64 and linux/amd64 | yes |
| `scripts/gates/cross.sh` | build every target a node runs on; run it before anything touching a build tag | no |
| `scripts/gates/docs.sh` | the Sphinx build with warnings as errors, as Read the Docs and CI run it | no; CI runs it |
| `sqlc generate` / `sqlc diff` | regenerate `internal/state/ledgerdb` after editing a query or adding a migration; prove the committed code is what the pinned sqlc generates | no; CI runs `sqlc diff` |
| `scripts/gates/terraform.sh fmt validate test lint scan` | the terraform gates; run before pushing a `.tf` change | no, they need terraform, tflint and trivy; CI runs them |
| `scripts/gates/scenario.sh NAME` | one of the Ansible collection's scenario tests, by the name of its script or playbook under `ansible_collections/junioryono/billet/tests/` (the billet-infra-terraform-ansible skill says which converges what) | no; CI runs them |
| `scripts/gates/fmt-check.sh`, `scripts/gates/lambda-test.sh`, `scripts/check-module-sources.sh` | gofmt, the spot-router Lambda's tests, and every documented Terraform module source pinnable | yes |
| `scripts/gates/fuzz.sh`, `scripts/gates/bench.sh` | search past every fuzz target's seeds; the allocator's and the node wire's benchmarks | no; CI runs the fuzz search nightly |
| `scripts/gates/dist.sh` | build the release artifacts into `dist/` exactly as a tag would | no |
| `scripts/test-package-lifecycle.sh`, `scripts/test-restore-rehearsal.sh`, `scripts/test-postgres-restore-rehearsal.sh` | the real-package rehearsals, each run after `scripts/gates/dist.sh` | no; CI runs them |
| `scripts/test-systemd-lifecycle.sh`, `scripts/rollout-rehearsal.sh`, `scripts/recover-rehearsal.sh`, `scripts/ca-rotation-rehearsal.sh`, `scripts/promotion-rehearsal.sh`, `scripts/retirement-rehearsal.sh` | the rehearsals that need a real GitHub App, each run by hand (after `scripts/gates/dist.sh`, except the rollout rehearsal, which installs published releases); `docs/reference/records/host-rehearsals.md` says what each needs | no; run by hand |
| `python3 scripts/check-tests-kept.py` | report the `Test` functions HEAD has and the working tree does not, after a scripted edit to `_test.go` files | no; a deleted test can be right |

A green `scripts/gates/check.sh` is necessary rather than sufficient: CI additionally proves `go mod tidy` leaves no diff, runs the suite against a real PostgreSQL, runs `govulncheck`, cross-builds, and runs everything in the table's "no" rows. Before pushing, run `scripts/gates/check.sh` if you want the whole gate on your own machine, or `scripts/gates/verify.sh` for what you changed; either way the pull request needs a green CI run, which runs the full gate and the rest. A Claude session runs `scripts/gates/verify.sh` and reads CI, and the repository's hook refuses it the full gate unless Claude Code was started with `CLAUDE_FULL_CHECKS=1`.

## Making changes

- **Load the skill for the area before you start**, not after you are stuck. `CLAUDE.md` maps areas to skills. Each holds rules that cost a debugging session to learn and are not visible in the code. If your change makes a skill wrong, fix the skill in the same pull request.
- **A test must fail when the code is wrong.** Before submitting a test for an invariant, break the invariant once and confirm the test goes red. Two of the project's original state tests passed against the very bug they were named for; `.claude/skills/billet-testing/SKILL.md` catalogues the shapes that fool a reader.
- **Use `t.Context()` and `t.TempDir()`.** `t.Parallel()` is fine for a test that owns its resources and wrong for one that shares process-global state; the `parallelshared` analyzer reports the latter. Match the file you are adding to.
- **Fix a lint finding at the cause.** A suppression is `//nolint:<linter> // reason` or `//billet:ignore <analyzer> // reason`; both reject a bare directive and report an unused one, so "0 issues" means "0 unexplained issues".
- **A rule about an API billet does not own is pinned to measured behaviour**, with the date, never to a reading of the documentation. Run the probe where the code runs (a Linux container, a real Mac, a real account), not only on the development machine.
- **Never hard-wrap prose.** Every paragraph in a Markdown or text file, an issue, a pull request or a commit body is one line. Go and shell comments wrap at 88 columns.
- Types and functions get a doc comment cut to what the name does not already say; a comment inside a function survives only where it marks something a reader would otherwise get wrong. Do not write the history of the code.
- Keep unrelated refactors out of a feature or bug-fix pull request.

## Pull requests

Work on a feature branch named `<name>-<monthday>-<description>` (`junior-sep03-readthedocs`); never commit to `main`. Integrate with `git merge`, never rebase or force-push, so history stays append-only.

A branch commit's subject is imperative, under about seventy characters, with a body that says **why** rather than what; if the change encodes an invariant, name the failure mode it prevents. Pass the message with `-F`, because backticks in `git commit -m` are command substitution. The pull request title is `Area: What changed`:

```text
State: Enforce a single authoritative control plane
Cache: Fail open to a miss on any proxy error
Docs: Set up the Read the Docs build
```

The body covers what changed and why, how it was verified beyond `scripts/gates/check.sh` (a mutation test, a manual `billet check`, a real host), and what is **not** covered. "Untested on a real Mac" is useful information; silence is not. Link the issue with `Closes #N`. `.claude/skills/billet-git-flow/SKILL.md` has the full conventions, including the adversarial review that runs before a push.

## Signing off your commits

Every commit you author is signed off under the [Developer Certificate of Origin](https://developercertificate.org): by adding `Signed-off-by: Your Name <you@example.com>`, matching the commit's author, you certify that you wrote the change or otherwise have the right to contribute it under the project's license. `git commit -s` adds the line for you. CI's `dco` job checks every commit your pull request adds, merges included, and names any that lacks it. Merge `main` in with `git merge --signoff`: GitHub's "Update branch" button and its web conflict editor make unsigned merges. Repairing commits you have already made rewrites them, which is the one exception to never rebasing or force-pushing: squash your branch into one signed-off commit, which keeps its contents exactly, merges included. A failed `dco` job prints the exact commands for your pull request; they fetch its base branch from billet's repository rather than your fork's `origin`, whose `main` may be behind, push to your pull request's branch by name whatever your push settings, assume nothing is uncommitted or staged, and stop if your branch is not the commit CI checked or has more than one merge base with billet's.

Contributions outside `ee/` are accepted under the Apache License 2.0. `ee/` is under the billet Enterprise License, and takes outside contributions only under a written agreement with its licensor, so open an issue first.

## Releases

Releases are maintainer-operated through the **Cut Release** workflow. It creates or reuses the `release/vX.Y` branch (one per minor, carrying every patch), tags, builds through GoReleaser, publishes a signed release manifest beside the archives and packages, verifies the release's immutability attestation, advances the signed `stable` channel, and moves the `v0` tag. A hotfix is a commit on the release branch and the same button with the patch version, then a merge back to `main`. Never tag by hand.

Press the button twice. The first run cuts nothing: its summary is a report of what the release carries against the previous one, one line per merged pull request grouped by area, with every change to the scheduler, the ledger schema or the node wire flagged, any new migration announced first because it makes the release one-way, and what CI said about the exact commit being cut. Read it, then run the workflow again with `confirm` set to the `<tag>@<commit>` line the report ends with. A confirmation naming any other tag or commit, because `main` or the branch moved since, cuts nothing and fails the run. `.claude/skills/billet-releases-and-upgrades/SKILL.md` covers the pipeline and the fleet rollout.

## Reporting security issues

Do not open a public issue for a vulnerability. billet holds a GitHub App private key, a node-wire certificate authority and, when cache interception is enabled, a TLS interception key; email the maintainer instead. billet is not a sandbox for untrusted code, and its documentation says so: a report amounting to "a job can affect the host it runs on" describes documented behaviour on the Docker backend and on any backend without its separate untrusted network, and the relevant guidance is GitHub's own against self-hosted runners on public repositories.
