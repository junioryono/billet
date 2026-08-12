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
- `forbidigo` bans `time.After` (leaks its timer until it fires), `panic` (a control plane that panics drops every in-flight lease), and `context.Background` outside `cmd/billet`.

---
