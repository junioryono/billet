---
name: billet-git-flow
description: "Git workflow for billet — branch naming, commit format, the pre-commit gate, and PR conventions. Use this skill whenever creating branches, committing, pushing, or opening a pull request. Also use when the user says 'commit this', 'make a PR', 'push this', or 'create a branch'. Even a simple 'commit and push' should go through this skill so conventions stay consistent."
---

# Git workflow

## Never commit to `main`

Always work in a feature branch. `main` is what people `go install`, and billet is a control plane
someone else's CI depends on.

Format: `<name>-<monthday>-<description>`

```
junior-aug06-scaleset-listener
junior-aug14-fix-lease-fence
```

Create from the latest main:

```bash
git checkout main && git pull
git checkout -b junior-aug06-scaleset-listener
```

## Never rebase or force-push

Integrate by **merging**. No `git rebase`, no `git push --force`/`--force-with-lease`, no history
rewriting. To bring main into a branch:

```bash
git checkout main && git pull
git checkout <branch> && git merge main
```

History stays append-only, which keeps shared branches safe.

## The gate runs before the commit, not after the push

```bash
make check
```

Build, vet, gofmt, lint, and `go test -race`. All clean, every time. CI runs the same thing, so a
green local run is a green CI run — that equivalence is the only reason a gate is worth having.

If the linter objects, fix the code. An exception is `//nolint:linter-name // reason` with a real
reason; `nolintlint` rejects a bare directive, so "0 issues" means "0 unexplained issues".

## Commit messages

Subject: imperative, no trailing period, under ~70 chars.

Body: **why**, not what — the diff already says what. If the change encodes an invariant, say which
failure mode it prevents; that sentence is what a reader needs in a year.

```
Take an exclusive lock on the state directory

SQLite's single-writer rule stops two connections writing at once. It does
not stop two billet processes both long-polling GitHub and taking turns
writing conflicting scheduling decisions to one ledger, which double-admits
jobs onto a host that cannot hold them and fails silently.

flock rather than a PID file so a crashed server releases it automatically.
```

Do not mention the tools used to write the change.

## Pull requests

Title: `Subject: Description` — the area, then what changed.

```
State: Enforce a single authoritative control plane
Cache: Fail open to a miss on any proxy error
```

Body covers:

- **What changed and why**, with the failure mode if there is one.
- **How it was verified.** `make check` is the floor. Say what else you ran — a mutation test, a
  manual `billet check`, a crash injection.
- **What is NOT covered.** Be specific. "Untested on a real Mac mini" is useful; silence is not.

Link the issue with `Closes #N`.

## Pre-1.0 has no upgrade path, and the docs must say so

There is no released version, so a change to the on-disk state format is allowed to break an
existing database. It is **not** allowed to break it confusingly: detect the incompatible shape and
say what to do about it, the way `checkBookkeepingSchema` does. A raw `no such column` gives an
operator nothing to act on.

Once there is a tagged release, this stops being true and schema changes become append-only
migrations with real upgrade tests.
