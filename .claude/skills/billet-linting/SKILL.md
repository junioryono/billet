---
name: billet-linting
description: "billet's linter configuration and the rule that a lint failure is fixed rather than suppressed. Use when golangci-lint fails, when tempted to add a nolint directive, or when changing .golangci.yml."
---

# Linting

## Linting

`.golangci.yml` is the source of truth for Go conventions, and the *why* for each non-stdlib rule is a comment next to that rule. The version is **pinned to v2.12.2** and CI uses the same one:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

`nolintlint` rejects a bare `//nolint`. An exception is written `//nolint:linter-name // reason`, and the reason is mandatory — so "0 issues" means "0 unexplained issues".

**When you fix a bug, ask whether it should be a lint rule.** If the bug is a *class* something could reintroduce, and it is mechanically checkable, encode it:

1. **Can golangci-lint express it?** A `forbidigo` identifier ban, a `depguard` layering rule, a linter setting. Cheapest — do this first.
2. **Project-specific but deterministic?** A `go/analysis` analyzer, wired into CI only if it measures ~zero current violations. A noisy analyzer erodes trust and gets ignored.
3. **Needs judgment?** Document it in the Invariants section above.

Rules already carrying real weight here:

- `depguard` bans `mattn/go-sqlite3` (cgo breaks the single-static-binary goal and the cross-build matrix) and confines `modernc.org/sqlite` to `internal/state`, so nothing else can open a connection without the durability pragmas.
- `depguard` also refuses `net/http`, `os/exec` and every upper layer in `internal/state`, `internal/alloc` and `internal/rollout` — the three packages that open a write transaction. `DB.Tx` begins IMMEDIATE, so it holds SQLite's single writer slot from BEGIN, and a remote call inside one stalls every scheduling write in the process and every operator command in the other.
- `depguard` keeps `internal/state/ledgerdb` (sqlc's output) importable from `internal/state`, `internal/alloc` and `internal/rollout` alone, and keeps that package from importing anything of billet's.
- `forbidigo` bans `time.After` (leaks its timer until it fires), `panic` (a control plane that panics drops every in-flight lease), `context.Background` outside `cmd/billet`, and `errors.As` in favour of `errors.AsType[T]` (the value cannot outlive the check that proved it exists).
- `inamedparam` requires named parameters on interface methods, because an interface is a contract somebody else implements and an unnamed list says nothing about what it wants.

## Some rules cannot be a linter, and two are not

`golangci-lint` cannot express everything, and billet has two other places a rule can live. Choose in this order.

**A test**, when the rule is about source text or files. `TestNoGoBodyIsWrittenOnOneLine` in `scripts/` walks the tree and refuses `if err != nil { return err }`; `TestQueryFilesAreASCIIOnly` and `TestNoQueryUsesAWildcardProjection` in `internal/state` police the sqlc query files. A test needs nothing installed and fails on the commit that breaks it.

**A `go/analysis` analyzer in `tools/lint`**, when the rule needs types or control flow. It is a NESTED MODULE so `golang.org/x/tools/go/analysis` never becomes a dependency of the billet binary, and `make lint-custom` — which is in `make check` — builds it, runs it, AND runs its own tests. That last part is not optional: `go test ./...` from the root cannot cross the module boundary, and an analyzer that silently stopped detecting would still report zero violations, which reads exactly like a clean tree.

Two exist. `parallelshared` reports state a parallel subtest writes and its parent test owns, which is what `paralleltest` cannot see and why that linter stays off. `rawsql` reports a call that executes SQL from Go rather than from a named query file — default-deny across the module, deciding from the callee's SIGNATURE rather than its name, because `url.URL.Query` exists and a name-only rule is either noisy or goes stale. It skips `_test.go` files (same exemption `depguard`'s `ledgerwriters` rule makes, same reason) and generated files (`internal/state/ledgerdb` IS the compiled query set).

Suppress a finding with `//billet:ignore <analyzer> // <reason>` on the line or on its own line directly above; a bare directive is itself reported, an unused one is too, and a trailing one covers only its own line. **A bare directive cannot be expressed in an analyzer's own testdata**, because anything trailing it becomes its reason — that half is covered by `TestABareDirectiveIsReported` in `tools/lint/suppress`, and the unused case is expressed by putting the expectation inside the directive.

**Measure before turning any of it on.** billet's bar is ~zero existing violations — `wsl_v5` was measured at 5882 findings on this tree and rejected on that number alone, and `orphantest` at 159 of 324 test files because billet names tests for the invariant they assert. A rule that arrives with a backlog is a rule people learn to route around.

---
