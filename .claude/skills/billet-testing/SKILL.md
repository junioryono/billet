---
name: billet-testing
description: "Load when adding or changing any test, when asked to add coverage or follow TDD, when a test passes suspiciously, before believing a probe run on the development Mac, or when touching the e2e suite, the replay harness, a real*_test.go, a live test, the rehearsals or any environment variable that gates them."
---

# Testing billet

## What this area is

billet's suite is 450-odd `_test.go` files across unit tests, structural tests that parse Go source, cross-package integration tests in `internal/integration`, the end-to-end suite in `internal/e2e` (a real control plane, real node wire and real container runtime against `internal/fakeactions`), `real*_test.go` files that assert billet's model against a real tart, docker, firecracker, CodeBuild, S3 or launchd, and opt-in live tests that spend money. `make test` runs the ordinary suite with `-race -count=1 -covermode=atomic`; CI adds a real PostgreSQL and a second `internal/alloc` run with `BILLET_TEST_LEDGER=postgres`.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### A test must be able to fail: [references/discipline.md](references/discipline.md)

- **A test must fail when the code is wrong, and the way to know is to break the code once.**
- **A mutation harness must prove it changed the file, and a mutant must change behaviour.**
- **Assert the diagnostic, not the shape.**
- **A discarded error is a vacuous assertion waiting to happen.**
- **Prove the mechanism is used, not only that it works.**
- **The test written to prove a fix tends to prove the adjacent thing.**
- **When a platform behaviour matters, write a throwaway probe and run it where the code runs.**
- **Silence looks like success, and each silence has a guard.**

### Conventions, parallelism and concurrency: [references/conventions-and-concurrency.md](references/conventions-and-concurrency.md)

- **Conventions.**
- **`t.Parallel()` turns on the sharing, not the disk.**
- **A parallel test that writes an executable and then execs it can exec something else.**
- **Concurrency tests use the scheduler under test.**
- **A test about time runs in a `testing/synctest` bubble, and nothing in the bubble waits on a real socket.**
- **A stand-in must stay alive and be observable.**
- **Structural tests assert order and call graphs where a unit test cannot reach the call site.**

### The suites: [references/suites.md](references/suites.md)

- **The end-to-end suite assembles billet the way `cmd/billet` does.**
- **The replay harness drives a workload through the real scheduler at compressed time, and steers exactly two things.**
- **Generated shell is executed, never pattern-matched.**
- **Real and live tests skip without their resource and never spend money by accident.**
- **PostgreSQL is tested against a real server or not at all.**
- **Coverage is a signal, not a goal.**
- **A benchmark measures the ledger the run exercises, and a change to the hot paths shows its numbers before and after.**
- **The host rehearsals run the real commands on packaged hosts under real systemd, and each needs a real App.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**Before submitting a test.** Break the production line it names once and watch it go red, then restore it ([references/discipline.md](references/discipline.md)). Assert the sentinel, the status or the diagnostic clause, never only that an error came back. Discard no error an assertion depends on. Drive the caller, not only the helper, or assert the call site structurally. Use `t.Context()`, `t.TempDir()` and `t.Cleanup`, and make a test parallel only if it shares no process-global state.

**A test that waits.** Steer the clock under test rather than sleeping past a wall-clock bound, and count a wait from a baseline taken before the thing under test exists ([references/conventions-and-concurrency.md](references/conventions-and-concurrency.md)). A loop on a ticker is tested in a `synctest` bubble, through the ticker it really runs on.

## Where the tests are

- `internal/state/*_test.go`: single writer, pragmas on the writer connection, reader cannot write, migration checksums, forward compatibility, schema CHECK constraints.
- `internal/config/*_test.go`: build cases by `strings.Replace` on a known-good config; cover both directions of every guard.
- `internal/integration`: config boundary, generated tier over the wire, release boundary, live session replacement, real Ceph into firecracker capacity.
- `internal/e2e`: lifecycle, wire, enroll, revocation, restart, restore, compute barrier, holder-gone, multiday, codebuild, ec2, init launch.

## Related skills

`billet-checks-and-lint` (the gate and the analyzers), `billet-shell-gates` (probes and executed shell), `billet-capacity` and `billet-lifecycle` (the areas whose vacuous tests are catalogued there).
