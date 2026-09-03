# Contributing to billet

billet is pre-alpha and the architecture is still moving, so an issue before a large pull request will save you time. Contributions should keep a change at the scope it was asked for, preserve the invariants the skills under `.claude/skills` write down, and include tests that fail when the code is wrong.

## Development setup

Requirements:

- Go 1.26.6 or later, as `go.mod` says. Everything builds with `CGO_ENABLED=0`, because a node is deployed by copying one static file, and a cgo dependency is refused by the linter.
- Python 3.13 for the documentation build and the repository's Python gates.
- GNU Make, a POSIX shell, and Docker for the end-to-end suite (it skips without one).

Clone the repository, install the pinned tools, and run the same gate CI runs:

```bash
git clone https://github.com/junioryono/billet && cd billet
make tools     # golangci-lint, goreleaser, sqlc, tflint and trivy at the pinned versions
make check     # the pre-commit gate
```

`tools/lint` is a nested Go module holding billet's own analyzers, so `golang.org/x/tools` never becomes a dependency of the binary. `make check` builds and tests it for you.

## Verification commands

| Command | What it does | In `make check`? |
|---|---|---|
| `make check` | `no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources` | it is the gate |
| `make test` | `go test -race -count=1 -covermode=atomic ./...`; the coverage counters are part of the gate because they reorder goroutines | yes |
| `make lint` | golangci-lint (pinned) for the host **and** for `GOOS=linux`, because a linter only sees the files it would compile | yes |
| `make lint-custom` | build `tools/lint`, run its own tests, run `billetlint` for darwin/arm64 and linux/amd64 | yes |
| `make cross` | build every target a node runs on; run it before anything touching a build tag | no |
| `make docs` | the Sphinx build with warnings as errors, as Read the Docs and CI run it | no; CI runs it |
| `make sqlc` / `make sqlc-check` | regenerate `internal/state/ledgerdb` after editing a query or adding a migration; prove the committed code is what the pinned sqlc generates | no; CI runs `sqlc diff` |
| `make tf-fmt-check tf-validate tf-test tf-lint tf-scan` | the terraform gates; run before pushing a `.tf` change | no, they need terraform, tflint and trivy; CI runs them |
| `make host-upgrade-order unit-parity …` | the Ansible scenario tests (`make help` lists them) | no; CI runs them |
| `make dist` | build the release artifacts exactly as a tag would | no |
| `make package-lifecycle`, `make restore-rehearsal`, `make postgres-restore-rehearsal`, `make systemd-lifecycle` | the real-package rehearsals | no; CI runs the first three |
| `make acceptance` | an isolated deployment against a real AWS account and a real GitHub App; billable | no; a weekly workflow runs it |

A green `make check` is necessary rather than sufficient: CI additionally proves `go mod tidy` leaves no diff, runs the suite against a real PostgreSQL, runs `govulncheck`, cross-builds, and runs everything in the table's "no" rows. Do not push work that has not passed `make check`, and expect CI to catch the rest.

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

The body covers what changed and why, how it was verified beyond `make check` (a mutation test, a manual `billet check`, a real host), and what is **not** covered. "Untested on a real Mac" is useful information; silence is not. Link the issue with `Closes #N`. `.claude/skills/billet-git-flow/SKILL.md` has the full conventions, including the adversarial review that runs before a push.

## Releases

Releases are maintainer-operated through the **Cut Release** workflow. It creates or reuses the `release/vX.Y` branch (one per minor, carrying every patch), tags, builds through GoReleaser, publishes a signed release manifest beside the archives and packages, verifies the release's immutability attestation, advances the signed `stable` channel, and moves the `v0` tag. A hotfix is a commit on the release branch and the same button with the patch version, then a merge back to `main`. Never tag by hand. `.claude/skills/billet-releases-and-upgrades/SKILL.md` covers the pipeline and the fleet rollout.

## Reporting security issues

Do not open a public issue for a vulnerability. billet holds a GitHub App private key, a node-wire certificate authority and, when cache interception is enabled, a TLS interception key; email the maintainer instead. billet is not a sandbox for untrusted code, and its documentation says so: a report amounting to "a job can affect the host it runs on" describes documented behaviour on the Docker backend and on any backend without its separate untrusted network, and the relevant guidance is GitHub's own against self-hosted runners on public repositories.
