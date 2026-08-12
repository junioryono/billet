---
name: billet-git-flow
description: "Git workflow for billet — branch naming, commit format, the pre-commit gate, and PR conventions. Use this skill whenever creating branches, committing, pushing, or opening a pull request. Also use when the user says 'commit this', 'make a PR', 'push this', or 'create a branch'. Even a simple 'commit and push' should go through this skill so conventions stay consistent."
---

# Git workflow

## Never commit to `main`

Always work in a feature branch. `main` is what people `go install`, and billet is a control plane someone else's CI depends on.

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

Integrate by **merging**. No `git rebase`, no `git push --force`/`--force-with-lease`, no history rewriting. To bring main into a branch:

```bash
git checkout main && git pull
git checkout <branch> && git merge main
```

History stays append-only, which keeps shared branches safe.

## Every commit gets a Codex review, before it is pushed

This is a standing requirement, not a suggestion for large changes. Publishing unreviewed code is the wrong order: a second reader is cheapest before the commit is public, and billet holds credentials that make a quiet mistake expensive.

Run it from the repo root, as a **background task** (~3–6 min), and wait for the completion notification rather than polling:

```bash
codex exec -m gpt-5.6-sol -c model_reasoning_effort="high" \
  --sandbox read-only \
  -o /tmp/codex-billet-<topic>.md \
  -C /path/to/billet \
  "ADVERSARIAL COMMIT REVIEW. HARD RULES: do NOT build, do NOT run tests, do NOT
   run go build / go test / go vet / gofmt / golangci-lint — read code only.
   Review the HEAD commit (git show HEAD) and the full files it touches.
   <state the design intent the change must satisfy>
   Grade P0 (security hole / data loss / will fail in production) through P3,
   each with what breaks, why, and a concrete fix. Cite file:line." < /dev/null
```

Details that each cost a debugging session to learn:

- **The trailing `< /dev/null` is mandatory.** With an open non-TTY stdin — every background shell — `codex exec` prints "Reading additional input from stdin..." and hangs forever. Harmless in the foreground, so always include it.
- **`--sandbox read-only`, and say "do not build or test" in the prompt.** A `go build` inside a long reasoning run recompiles the tree and is the most common way one of these dies mid-flight. You run every gate yourself anyway.
- **Pass the model and reasoning effort explicitly.** Never rely on `~/.codex/config.toml` defaults; they vary per machine.
- **State the design intent in the prompt.** A reviewer that does not know the App key must never be overwritten cannot tell you that it can be.
- **Write output to a deterministic path** keyed to the topic, so a fresh shell can re-derive it after the notification.

**Validate every finding yourself before acting on it.** Codex grades severity and is not always right — across this project's reviews it was correct on `zfs promote` semantics, the BuildKit trust boundary and Tart's license, and overstated an argument about token validation that did not apply. Open the cited `file:line`, decide Confirmed / False positive, and fix what is confirmed. Say in the PR which findings were accepted and which were rejected, with the reason.

## What to put IN the review prompt, and why it is most of the value

A bare "review this diff" wastes the round. Twelve rounds on the node split converged only once the prompt carried four things. Rebuild it from this list; do not rely on a previous session's scratchpad, which does not survive.

1. **SCOPE, narrowly.** "The N most recent commits; run `git log --oneline -N`, then `git diff HEAD~N..HEAD`." A whole-branch review of a large diff runs for an hour and can get killed before it writes anything. One to three commits is the working size. Add: *do not go exploring the rest of the repository; if a finding needs context from an unchanged file, read only the function you need,* and give it a wall-clock budget with "an unfinished review is worth nothing, so write your findings before you run out of time."

2. **The MODEL as it currently stands**, in a paragraph or two of invariants — not the diff restated. What the classes of thing are, what each is allowed to do, and which fact is authoritative. Codex finds the good defects by reasoning about the model, so a stale or vague description produces stale findings.

3. **What is KNOWN AND DELIBERATELY NOT FIXED**, by name, with "do not re-report it; DO report anything else it implies that is fixable." Without this you get the same accepted limitation every round, crowding out new findings. (Currently: the plane cannot tell which of two hosts sharing a name physically holds a container after a restart — that needs durable per-incarnation state, task #14.)

4. **An ATTACK LIST in priority order**, ending with test quality. The test items that have actually caught things: assertions that cannot fail, assertions a panic would satisfy, waits satisfied by something other than the thing under test, ordering that makes an assertion unobservable, wall-clock sleeps standing in for conditions, tests claiming to stage a race without establishing its ordering, goroutines outliving their test, and shared state one test mutates.

Then: **fix, mutation-test each fix, run `make check`, commit, and immediately launch the next round.** Severity falls over rounds but does not reach zero quickly — most rounds find a defect in code written to fix the previous round, so do not treat a small round as the last one.

## The gate runs before the commit, not after the push

```bash
make check
```

Build, vet, gofmt, lint, and `go test -race`. All clean, every time. CI runs all of it, so a red `make check` is a red CI run — that is the point of the gate. CI does more besides (`go mod tidy` diff, coverage upload, `govulncheck`, cross-builds), so green locally is necessary rather than sufficient.

If the linter objects, fix the code. An exception is `//nolint:linter-name // reason` with a real reason; `nolintlint` rejects a bare directive, so "0 issues" means "0 unexplained issues".

## Commit messages

Subject: imperative, no trailing period, under ~70 chars.

Body: **why**, not what — the diff already says what. If the change encodes an invariant, say which failure mode it prevents; that sentence is what a reader needs in a year.

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
- **How it was verified.** `make check` is the floor. Say what else you ran — a mutation test, a manual `billet check`, a crash injection.
- **What is NOT covered.** Be specific. "Untested on a real Mac mini" is useful; silence is not.

Link the issue with `Closes #N`.

## Pre-1.0 has no upgrade path, and the docs must say so

There is no released version, so a change to the on-disk state format is allowed to break an existing database. It is **not** allowed to break it confusingly: detect the incompatible shape and say what to do about it, the way `checkBookkeepingSchema` does. A raw `no such column` gives an operator nothing to act on.

Once there is a tagged release, this stops being true and schema changes become append-only migrations with real upgrade tests.
