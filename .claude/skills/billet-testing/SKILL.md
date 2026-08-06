---
name: billet-testing
description: "Write tests for billet — the state store, config validation, providers, and the cache conformance suite. Use when adding tests, when asked to 'test this', 'add coverage', or 'write a test for X', and when following TDD on billet. Covers the mutation-test discipline, why several original tests were vacuous, SQLite test setup, and what belongs in a conformance suite rather than a unit test."
---

# Testing billet

## The rule: a test must fail when the code is wrong

This is not a slogan here — two of billet's original state tests passed regardless of the code:

- **`TestDurabilityPragmas` read the pragmas through the reader pool.** `journal_mode` is a
  persistent property of the database FILE, so a reader reports `wal` whether or not the writer's DSN
  configured anything. It tested SQLite, not billet.
- **`TestConcurrentWritesSerialize` only used the single-connection writer pool**, which makes
  serialization true by construction. It could not observe the bug it was named for.

Both now assert against `db.w` specifically and against a second `Open`. When you write a test for an
invariant, **break the invariant once and confirm the test fails.**

```bash
# The mutation test that proved the process lock has teeth.
# 1. Comment out the syscall.Flock call in internal/state/lock_unix.go
go test -run TestSecondOpenIsRefused ./internal/state/   # must FAIL
# 2. Restore it
go test -run TestSecondOpenIsRefused ./internal/state/   # must PASS
```

If step 1 passes, the test is decoration. Delete it or fix it.

## Assert the diagnostic, not the shape

Counting error lines passes for the wrong reasons. `TestValidateReportsAllProblemsAtOnce` originally
asserted `strings.Count(err.Error(), "\n") >= 6`, which stays green if the six errors are all the
wrong ones. It now asserts each specific message.

The same applies to messages that carry operator guidance. `installation_id is required; creating an
App does not install it` exists because the failure is otherwise baffling — so the test asserts the
`does not install it` clause, not merely that an error occurred.

## A discarded error is a vacuous assertion waiting to happen

`if n, _ := a.Headroom(ctx, "x"); n != 0 { t.Error(...) }` reads harmlessly and is not. Go returns
the **zero value alongside an error**, so every assertion that a result *is* zero — no headroom, no
open leases, no rows — passes when the call FAILS. The test proves nothing and looks green.

This bit five times in `internal/alloc`: two `Usage` assertions and three `Headroom` ones. **errcheck
cannot catch it**, because `.golangci.yml` excludes errcheck from `_test.go` — deliberately, since
tests are full of `defer db.Close()` where the error genuinely is noise, and a linter firing on forty
of those is one people learn to ignore.

So this is a judgment rule, not a mechanical one. Two ways to satisfy it:

```go
// A checked helper, when the call appears many times.
func headroom(t *testing.T, a *Allocator, tier string) int {
	t.Helper()
	n, err := a.Headroom(t.Context(), tier)
	if err != nil {
		t.Fatalf("Headroom(%s): %v", tier, err)
	}
	return n
}

// Or check inline, when it appears once.
u, err := a.Usage(ctx)
if err != nil {
	t.Fatalf("Usage: %v", err)
}
```

The check to apply: **if this call errored, would my assertion still pass?** If yes, the error has to
be checked. Confirm it the same way as any other invariant — make the call fail and watch the test
fail too.

## Package conventions

- `t.Context()` and `t.TempDir()`, never `context.Background()` or a hand-made temp dir. Enforced by
  `usetesting`.
- `t.Cleanup` for teardown, so it runs on the `t.Fatal` path too.
- No `t.Parallel()`. `paralleltest` is deliberately disabled: these tests open real SQLite files, and
  parallelism buys nothing while inviting flakes.
- Table tests for pure functions (`ParseByteSize`); named functions for anything asserting an
  invariant, so a failure names the invariant.

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

`writeConfig(t, body)` writes a YAML string to a temp file and returns the path. Build cases by
`strings.Replace` on a known-good `validConfig` so the diff between a passing and failing case is
visible in the test.

Cover both directions of a guard. The macOS licence cap has two tests for exactly this reason: one
proving a tier whose label omits `macos` is still capped (`guest_os` is what counts), and one proving
a Linux tier named `builds-macos-artifacts` is **not** capped. A single-direction test would have
passed against the original label-regex implementation, which was the bug.

## What does NOT belong in a unit test

**The Actions Cache v2 protocol.** It is reverse-engineered from an unpublished spec, so a unit test
against our own understanding of the wire format asserts that we are self-consistent, not that we are
correct. That belongs in the conformance suite: real `actions/cache`, `upload-artifact` and
`download-artifact` against live GitHub, gating every runner-image build. A mock cannot catch GitHub
changing the protocol, which is the only failure that matters.

**Anything needing `/dev/kvm` or a hypervisor.** Provider tests assert the lifecycle contract against
a fake; the real backends are validated by the conformance suite on a machine that has one.

## Coverage

```bash
make cover
```

85% project target, 80% patch. Treat a drop as a prompt to look, not a number to satisfy —
`cmd/billet/main.go` is excluded in `.codecov.yml` because unit-testing flag parsing measures nothing
while raising the figure.
