# Contributing to billet

billet is pre-alpha. The architecture is still moving, so an issue before a large PR will save you
time.

## Setup

```bash
git clone https://github.com/junioryono/billet && cd billet
make tools     # installs the pinned golangci-lint
make check     # build + vet + fmt + lint + race test
```

Go 1.26.5 or newer. No cgo — `CGO_ENABLED=0` everywhere, because a node is deployed by copying one
static file.

## The gate

```bash
make check
```

CI runs all of this, so a red `make check` is a red CI run. It is not the *whole* of CI, which also
verifies `go mod tidy` leaves no diff, uploads coverage, runs `govulncheck`, and cross-builds every
target a node can run on. A green `make check` is therefore necessary rather than sufficient — please
do not push work that has not passed it, and expect CI to catch the rest.

`make cross` additionally builds for `linux/amd64`, `linux/arm64` and `darwin/arm64`. Run it if you
touch build tags or anything platform-specific — the state lock is `//go:build unix`, and a mistake
there is invisible until it reaches a machine nobody develops on.

## Lint exceptions

`nolintlint` rejects a bare `//nolint`. Write:

```go
//nolint:errcheck // Rollback after Commit is a documented no-op; fn's error is the one worth reporting.
```

The reason is mandatory, so "0 issues" means "0 *unexplained* issues" rather than "0 issues someone
silenced".

If you find yourself wanting several exceptions for the same reason, that is usually the code telling
you something — the `readAppliedMigrations` helper exists because three `rows.Close()` calls on error
paths were easier to fix by extracting a function than to annotate.

## Tests

Read `.claude/skills/billet-testing/SKILL.md` first. The short version:

**A test must fail when the code is wrong.** Two of the original state tests did not — one read
per-file state through the wrong connection pool, the other exercised a single-connection pool and
called the result "serialization". Before you submit a test for an invariant, break the invariant
once and confirm the test goes red.

Use `t.Context()` and `t.TempDir()`. Do not add `t.Parallel()`.

## Commits and PRs

Feature branches; never commit to `main`. **Never rebase or force-push** — integrate with `git merge`
so history stays append-only.

Commit subject in the imperative. The body explains **why**, not what: if the change encodes an
invariant, name the failure mode it prevents. That sentence is what a reader needs a year later.

PR title is `Area: What changed`. The body should say what you verified beyond `make check`, and —
importantly — what you did **not** cover. "Untested on real Apple Silicon" is useful information;
silence is not.

Full conventions: `.claude/skills/billet-git-flow/SKILL.md`.

## Security

Do not open a public issue for a vulnerability. billet holds a GitHub App private key and, when cache
interception is enabled, a TLS CA private key. Email the maintainer instead.

Note that billet is **not** a sandbox for untrusted code, and its README says so. Reports amounting
to "a job can affect the host it runs on" are documented behaviour, not vulnerabilities — the
relevant guidance is GitHub's own: do not use self-hosted runners with public repositories.

## Architecture decisions

`CLAUDE.md` records the invariants and why each exists. If you are changing something it describes,
update it in the same PR — an out-of-date invariant is worse than none, because someone will trust
it.
