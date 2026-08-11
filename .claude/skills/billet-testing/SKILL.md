---
name: billet-testing
description: "Write tests for billet — the state store, config validation, providers, and the cache conformance suite. Use when adding tests, when asked to 'test this', 'add coverage', or 'write a test for X', and when following TDD on billet. Covers the mutation-test discipline, why several original tests were vacuous, SQLite test setup, and what belongs in a conformance suite rather than a unit test."
---

# Testing billet

## The rule: a test must fail when the code is wrong

This is not a slogan here — two of billet's original state tests passed regardless of the code:

- **`TestDurabilityPragmas` read the pragmas through the reader pool.** `journal_mode` is a persistent property of the database FILE, so a reader reports `wal` whether or not the writer's DSN configured anything. It tested SQLite, not billet.
- **`TestConcurrentWritesSerialize` only used the single-connection writer pool**, which makes serialization true by construction. It could not observe the bug it was named for.

Both now assert against `db.w` specifically and against a second `Open`. When you write a test for an invariant, **break the invariant once and confirm the test fails.**

```bash
# The mutation test that proved the process lock has teeth.
# 1. Comment out the syscall.Flock call in internal/state/lock_unix.go
go test -run TestSecondOpenIsRefused ./internal/state/   # must FAIL
# 2. Restore it
go test -run TestSecondOpenIsRefused ./internal/state/   # must PASS
```

If step 1 passes, the test is decoration. Delete it or fix it.

## Assert the diagnostic, not the shape

Counting error lines passes for the wrong reasons. `TestValidateReportsAllProblemsAtOnce` originally asserted `strings.Count(err.Error(), "\n") >= 6`, which stays green if the six errors are all the wrong ones. It now asserts each specific message.

The same applies to messages that carry operator guidance. `installation_id is required; creating an App does not install it` exists because the failure is otherwise baffling — so the test asserts the `does not install it` clause, not merely that an error occurred.

## A discarded error is a vacuous assertion waiting to happen

`if n, _ := a.Headroom(ctx, "x"); n != 0 { t.Error(...) }` reads harmlessly and is not. These APIs return the **zero value alongside an error**, so an assertion that a result *is* zero — no headroom, no open leases, no rows — passes when the call FAILS. The test proves nothing and looks green.

It bit seven times: five in `internal/alloc` (`Usage` and `Headroom`), and twice in `internal/github`, where `active, _ := hook["active"].(bool)` yielded `false` for an **absent** key — the expected value — so the test never proved the App manifest disables its webhook. Adding `omitempty` to that field, a realistic mistake, kept the suite green.

**errcheck catches this mechanically, and is enabled on tests.** It was excluded, on the assumption that `defer db.Close()` noise would swamp it. Measured, that assumption was wrong: 19 sites, two of them real bugs. Where an error genuinely cannot matter — writing to an `httptest` buffer — use `//nolint:errcheck // reason`.

Prefer a checked helper when the call appears many times, so the shape stops being writable:

```go
func headroom(t *testing.T, a *Allocator, tier string) int {
	t.Helper()
	n, err := a.Headroom(t.Context(), tier)
	if err != nil {
		t.Fatalf("Headroom(%s): %v", tier, err)
	}
	return n
}
```

Note the helper uses `t.Context()`. If a test needs a **cancelled** context to exercise cancellation, call the API directly — the helper would bypass the very context the test is about.

The question to ask, for anything the linter cannot see: **if this call errored, would my assertion still pass?** Confirm it the way you would any invariant — make the call fail and watch the test fail too.

## Comma-ok is the same trap

`v, _ := m["k"].(T)` yields the zero value when the key is absent *or* the wrong type. Asserting the zero value therefore proves nothing about presence. When presence is the point — a manifest field that must be explicitly `false` — assert `ok` separately from the value:

```go
active, ok := hook["active"].(bool)
switch {
case !ok:
	t.Errorf("active must be present and boolean, got %v", hook["active"])
case active:
	t.Error("the webhook must be inactive")
}
```

## Package conventions

- `t.Context()` and `t.TempDir()`, never `context.Background()` or a hand-made temp dir. Enforced by `usetesting`.
- `t.Cleanup` for teardown, so it runs on the `t.Fatal` path too.
- No `t.Parallel()`. `paralleltest` is deliberately disabled: these tests open real SQLite files, and parallelism buys nothing while inviting flakes.
- Table tests for pure functions (`ParseByteSize`); named functions for anything asserting an invariant, so a failure names the invariant.

## Testing the state store

`Open` takes a context and a directory, so a test is one line:

```go
func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
```

Things worth testing here, all of which have caught something:

| Invariant | How to test it |
|---|---|
| Single authoritative process | Second `Open` on one directory returns `ErrLocked` |
| The lock releases | `Open` → `Close` → `Open` succeeds |
| Writer pragmas actually applied | Query `PRAGMA synchronous` on `db.w` — it is per-connection, unlike `journal_mode` |
| Reader cannot write | `QueryRowContext` an `INSERT ... RETURNING` through `Reader()`, expect a read-only error, then assert the row count is still 0 |
| Migrations are append-only | Tamper with a recorded checksum, expect an "append-only" error on reopen |
| Forward compatibility refused | Insert version 9999, expect a "newer version" error |
| Schema CHECK constraints | Insert `phase = 'Done'`, `vcpu = 0` — each must be refused |

## Testing config

`writeConfig(t, body)` writes a YAML string to a temp file and returns the path. Build cases by `strings.Replace` on a known-good `validConfig` so the diff between a passing and failing case is visible in the test.

Cover both directions of a guard. The macOS licence cap has two tests for exactly this reason: one proving a tier whose label omits `macos` is still capped (`guest_os` is what counts), and one proving a Linux tier named `builds-macos-artifacts` is **not** capped. A single-direction test would have passed against the original label-regex implementation, which was the bug.

## What does NOT belong in a unit test

**The Actions Cache v2 protocol.** It is reverse-engineered from an unpublished spec, so a unit test against our own understanding of the wire format asserts that we are self-consistent, not that we are correct. That belongs in the conformance suite: real `actions/cache`, `upload-artifact` and `download-artifact` against live GitHub, gating every runner-image build. A mock cannot catch GitHub changing the protocol, which is the only failure that matters.

**Anything needing `/dev/kvm` or a hypervisor.** Provider tests assert the lifecycle contract against a fake; the real backends will be validated by the conformance suite on a machine that has one.

Neither suite exists yet — nor do the cache and the providers they would cover. Both are described here as the rule to follow when those land, not as something currently running.

## The test written to prove a fix tends to prove the adjacent thing

Four consecutive review rounds on the deployment lock found tests that passed while the check they named did nothing. They are worth listing together, because the shape is the same every time and it is not carelessness — each test was **about** the right subject and **exercised** something else:

| Meant to prove | Actually proved |
|---|---|
| the lock file's gid matches the directory's | `20 == 20` — a `t.TempDir` is owned by the primary group |
| ...then, with a borrowed group | the *kernel* supplied the gid — setgid was set before the file was created, so the comparison was never reached |
| `O_NOFOLLOW` refuses a symlinked lock file | `os.Root` refuses an *escape* — the target was an absolute path outside the directory |
| a second *process* is refused | flock works within one process; a package-local mutex passes identically |

**The check that catches this is mutation, not review.** Delete or neuter the production line the test names and re-run only that test: if it stays green, the test is about something else. Every one of the four above was found that way, three of them after a human-style reading had already called them correct. A test whose mutation survives is not necessarily wrong, but it must be shown to be redundant rather than assumed to be — and the redundancy said out loud.

A related habit that paid off repeatedly: when a platform behaviour matters (`os.Root` and symlinks, umask stripping mode bits, BSD versus Linux gid inheritance, `os.UserCacheDir`'s environment dependence), **write a throwaway probe that prints what actually happens** instead of reasoning from the documentation. Three of the defects above were documented behaviour that read the other way.

**A mutation harness must prove it CHANGED the file.** Verify by hash, not by grepping for a string that may already be absent — a substitution that matches nothing reports SURVIVED, which is indistinguishable from a vacuous test and sends you to fix a test that was fine. This produced three false verdicts in one session, one of them because a route is registered as `root+"/installed"` rather than the literal the pattern looked for.

**A clean `-race` run is evidence, not proof.** It only sees the interleavings that actually occurred. A concurrent slice read in the onboarding fake survived six clean `-race` runs because the racing append only happens while a request from the *previous* visit is still in flight. If two goroutines can reach the same field, fix it because they can, not because the detector complained.

**A catch-all route means a deleted route still answers 200.** The onboarding loopback mux registers `root+"/"`, so removing `/installed` sends the callback to the start page rather than to a 404 — every status-based assertion reads that as success. Where a route matters, assert something only its handler produces.

**A fallback path can make the thing it backs up untestable.** The same fake marked the installation visible as soon as the install page opened, so the authenticated poller completed the flow whether or not a callback was ever issued — deleting the callback, its route, or its address all left the tests green. When a fast path and a fallback both lead to success, the test has to withhold the fallback or it is only ever testing the fallback.

`make test` runs `-race`; `make cover` writes and opens an HTML profile.

- **A test must fail when the code is wrong.** Two of the original state tests did not: one read pragmas through the *reader* pool, where `journal_mode` reports `wal` from persistent file state regardless of the writer's DSN; the other exercised only the single-connection writer, which makes serialization tautological. If a test asserts an invariant, break the invariant once and confirm the test fails.
- **Assert the diagnostic, not the shape.** Counting error lines passes for the wrong reasons; asserting the specific messages does not.
- Tests use `t.Context()` and `t.TempDir()` (enforced by `usetesting`).
- `paralleltest` is deliberately off: these tests open real SQLite files, and `t.Parallel()` everywhere buys nothing and invites flakes.

## Tests that could not have failed

Every one of these passed against the exact bug it existed to catch. Mutation testing is what found them, and it is not optional for anything load-bearing.

- **A fake that ignores `context`** cannot distinguish "cleanup ran on a cancelled context" from "cleanup ran on a fresh one" — which was the entire subject of the test. Fakes honour `ctx.Err()`.
- **Cancelling before the call** made `Bind` fail first, so the launch path was never reached and the test passed because *nothing happened*. The fake cancels from inside `Launch` now, as a real timeout does, and the test asserts the provider was actually called.
- **A race whose window is nanoseconds** is not tested by two goroutines and a `WaitGroup`: the mutation survived five runs under `-race`. The fake can delay inside `Find`, which holds both callers inside the transition long enough to genuinely overlap.
- **Counting containers instead of RUNNING containers** let the headline end-to-end test pass while the "runner" exited instantly — proving container creation and removal, not that a job ran.
- **A fake that pops a message on read** cannot model redelivery, so a test "asserting" acknowledgement passed against a billet that acked before doing any work. The queue holds the head until its exact id is acked.
- **A mutation that does not compile is caught by the compiler, not by a test.** Three of them looked like passes until the failure count read zero. Keep every mutation compiling, and print the failing-test count so a zero is visible.
- **A mutation that never APPLIED looks exactly like a surviving one.** A `perl` substitution with three tabs of indentation against a line that has two matches nothing, reports "SURVIVED", and sends you off to write a test for behaviour that is already covered. Assert the substitution changed the file — `grep -c` the original text and expect zero — before believing the result.

- **A test satisfied by "an error" cannot tell a refusal from a panic.** Deleting the guard that checks a request carries a client certificate makes the code after it dereference a nil `r.TLS` — the handler panics, the client sees an error, and a test asserting `err != nil` passes. The mutation survived because the assertion was too weak, not because the guard was covered. Assert the SPECIFIC refusal: a sentinel error, or a status code.
- **Every test dialling an `httptest` server shares an assumption no production caller makes.** Its `URL` carries a scheme. `billet node` handed the client a bare `host:port` from config and could not construct a single request, and the whole suite was green — the one code path that builds a base URL from configuration had no test at all.

- **A wait that something else already satisfied is not a wait.** A test meant to let the janitor renew once before changing the TTL waited for `heartbeats > 0` — and `Recover` had already heartbeated while adopting, so it returned instantly and the race it was added to remove was still there. Count from a baseline taken before the thing under test exists.
- **A fake that cannot be slow cannot model the bug.** The window where nobody renews a lease only exists while a provider is working, so the fake provider needs to block inside `Launch` and say when it has — a delay plus a channel, never a sleep in the test.

- **A test that manufactures the concurrency it is checking proves only the narrow half.** The starvation test started `retryCleanup` in one goroutine and `heartbeatHeld` in another, then asserted the second could run while the first was stuck. That proves a stuck destroy does not hold `l.mu` — and nothing else. The property it was NAMED for is that the two run on separate clocks, and moving cleanup back onto the heartbeat's tick passed it unchanged. When the property is about scheduling, the test has to use the scheduler under test: drive `Run`, and assert the CONSEQUENCE (a lease the reaper took) rather than the mechanism.
- **A shutdown-time worker must not run on the caller's context.** Renewal was started on a child of `ctx`, so the caller cancelling to shut down stopped it at that instant — before the session close, before the release, before every slow remote destroy the release performs. Stopping it "last" in the deferred teardown was decoration: it had already stopped. Anything that must stay alive DURING shutdown gets `context.WithCancel(context.WithoutCancel(ctx))` and is stopped explicitly by the function that owns the teardown.

  The general form: if a goroutine's job is to protect the teardown, inheriting the cancellation that triggers the teardown is exactly backwards.

  The discriminator, having audited the other two sites — `Server.Run`'s sweeper `KeepAlive` and `nodeclient`'s janitor — is whether the function does meaningful work AFTER its context is cancelled. Both of those simply return, so a child context is right there and nothing needs changing: on process exit their leases are reaped and restart recovery re-adopts. Only the listener keeps working after cancellation, because its teardown destroys compute and releases capacity, and that work is what has to be protected.
- **Cancelling a goroutine is not stopping it.** A cleanup retry blocked in a remote `Destroy` outlived `Run` and came back afterwards to release against a database the caller was entitled to have closed. Cancel AND join, and be explicit about the order when two workers must stop at different times — cleanup before the release that would race it, renewal after.
- **A test whose observation can also be produced by shutdown proves nothing about the loop.** The first version of the cleanup-loop wiring test let the context expire and then asked whether a destroy had happened. `releaseAll` destroys everything still running, so it produced one — the test passed with the loop deleted, and the mutation run reported a kill only because `Run` incidentally returned `DeadlineExceeded`. Observe the effect while the system is still running, and enumerate every other path that could produce it.
- **A mutant that applies but changes no behaviour reports SURVIVED, and that is indistinguishable from a real gap.** Inserting `_ = id` next to a `delete` left the delete in place; the harness verified the file hash changed, so every existing guard passed, and the output said the property was uncovered when it was not. Hash-verification catches an edit that did not apply, not an edit that did nothing. A mutant must remove or invert behaviour — if you cannot say which assertion it should break, it is not a mutant.
- **Run the suite the way CI runs it, instrumented.** `-covermode=atomic` is not a reporting flag; the counters change timing enough to reorder goroutines that a plain `-race` build schedules identically every time. A launch in progress being handed to teardown was invisible under `make check` and reliable under coverage. `make check` now carries the flags, because a local gate weaker than CI trains you to trust it.

## Four ways silence has looked like success, and the guards for each

Every one of these produced a green gate and an untrue conclusion. They are the same failure wearing different clothes, and the pattern is worth recognising before the fifth one: **the thing that would have objected was itself missing.**

| What went missing | What it looked like | Guard |
|---|---|---|
| A scripted substitution matched nothing | Build and tests pass, bug untouched | `assert old in s` before replacing, and verify the file hash changed |
| A mutant applied but changed no behaviour | `SURVIVED` — identical to a real coverage gap | A mutant must remove or invert behaviour; if you cannot name the assertion it should break, it is not a mutant |
| A review prompt file did not exist | `codex exec` exit 0, no findings — identical to a clean round | `run_round.sh` refuses to launch without a non-empty prompt |
| A scripted edit deleted a whole test | Suite green; a deleted test cannot fail | `make tests-kept` — compares Test function names against HEAD |
| A killed mutation run left its mutant in the file | Compiles, mostly passes; an earlier green gate says nothing because the mutation landed after it | `make no-mutants` — runs FIRST in `check`; a stranded `.bak` is the only evidence |

The last one was found only because a mutation run happened to name that test and reported `NO SUCH TEST`. Nothing else in the toolchain noticed, and nothing else would have.

## An edit that did not apply looks exactly like an edit that did

Twice in one session a scripted `replace()` matched nothing and reported success: once because the anchor said HANDOVER where the file said HANDOFF, once because a comment had been reworded a round earlier. The build passed, the tests passed, and the bug the edit was meant to fix was untouched — the only reason it surfaced was a test written against the behaviour rather than the code.

**Assert every substitution.** `assert old in s` before replacing, and check the file hash changed afterwards. This is the same rule already written down for mutation testing, and it applies to every scripted edit for the same reason: the failure mode is silence.

**And use `-F` for every commit message.** Backticks in `git commit -m` are command substitution: three messages this session lost a phrase to it, in a project whose commit messages are the design record. A file cannot misfire.

## Two things Go gets right and reviewers get wrong

- **`url.Parse` accepts `"127.0.0.1:7717"`**, reading the host as a scheme. A validation that only calls it therefore cannot fail on the input it exists to reject. Check the parts you actually need — a scheme you recognise, a non-empty `Host`.
- **Deferred calls run last-in, first-out**, so

  ```go
  defer stopJanitor()   // runs SECOND
  defer janitor.Wait()  // runs FIRST — waits for a goroutine nothing has stopped
  ```

  deadlocks on every exit path the parent context did not cause. Two defers whose order matters belong in one defer, in the order written.

## The end-to-end suite

`internal/e2e` runs the real control plane and a real container runtime against `internal/fakeactions`, a scripted stand-in for GitHub. It exists because every other suite tests one seam, and this project's worst defect — acquiring jobs by the wrong id — lived in the relationship between billet and the wire, where billet's own types agreed with billet's own mistake.

Two things it must keep doing:

- **Assemble billet the way `cmd/billet` does**, through `internal/wiring`. The hand-copied adapter it started with had already drifted — it dereferenced a scale set the client returns as nil — and a test that assembles a different program is testing a different program.
- **Use an image that stays running.** busybox exits immediately, so recovery correctly saw a finished job every time and adoption was untestable.

## Coverage

```bash
make cover
```

Codecov is wired into CI. The project target is **`auto`** — do not regress from the base commit — with an 80% patch target on new code. Not an absolute number, and deliberately so: a hard 85% on a project sitting at 79% fails every PR from day one, and a check that always fails is a check everyone learns to ignore. The overall figure ratchets up as the project grows rather than being declared true in advance.

Coverage is a signal, not a goal: a test that exercises a line without asserting anything raises the number and catches nothing. That is what the mutation discipline at the top of this skill is for.

Treat a drop as a prompt to look, not a number to satisfy — `cmd/billet/main.go` is excluded in `.codecov.yml` because unit-testing flag parsing measures nothing while raising the figure.
