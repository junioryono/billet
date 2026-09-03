# billetlint — billet's own analyzers

A `go/analysis` multichecker for rules golangci-lint cannot express, in its own Go module (`tools/lint/go.mod`) so `golang.org/x/tools/go/analysis` never becomes a dependency of the billet binary. billet ships as one static binary with four direct dependencies; carrying an analysis framework into it for a CI gate would be the tail wagging the dog.

## Running it

```bash
make lint-custom          # builds the binary and runs it, then runs the analyzers' own tests
```

Or by hand, from the repository root:

```bash
(cd tools/lint && go build -o /tmp/billetlint ./cmd/billetlint)
/tmp/billetlint -parallelshared -rawsql ./... 2>&1
```

Diagnostics go to **stderr** (standard `go/analysis` behaviour), so pipe with `2>&1` when grepping.

**The analyzers' own tests are not reached by `go test ./...` from the root**, because this is a nested module. `make lint-custom` runs them, and so does CI. Without that step an analyzer could silently stop detecting anything and still report zero violations — which reads exactly like a clean tree, and is the failure this whole directory is arranged against.

## Suppression

`billetlint` is its own binary and has no access to golangci-lint's `//nolint` machinery, so it has its own directive, on the flagged line or the line directly above it:

```go
//billet:ignore parallelshared // guarded by mu, which the analyzer cannot see
results[name] = out
```

**A reason is mandatory.** A bare `//billet:ignore parallelshared` is itself reported, exactly as `nolintlint` rejects a bare `//nolint`. So "0 violations" means "0 *unexplained* violations", which is the only version of that sentence worth gating on.

**An unused directive is reported too**, which is the rule `.golangci.yml` already sets for `nolintlint` (`allow-unused: false`). A directive that suppresses nothing today is a standing licence for whatever lands on that line next, carrying a reason written for something else. Deleting a stale one costs nothing; keeping it costs the guarantee.

**A trailing directive covers only its own line.** The "line below" rule applies to a directive alone on its line, where the intent is unambiguous — reaching further would silence a finding nobody wrote a reason for, which is the exact failure the mandatory reason exists to prevent.

## Analyzers

### `parallelshared`

Reports state a **parallel subtest writes** and its **parent test owns**.

`paralleltest` and `tparallel` are both disabled in `.golangci.yml`, and the reason is written there: what decides whether a test is safe is not *whether* it calls `t.Parallel()` but what it **shares** with its neighbours. Neither of those linters models what a parallel closure mutates.

The hazard is not a failed assertion. Two parallel subtests writing one map is `fatal error: concurrent map writes`, which **aborts the test binary** and reports every unrelated test in the package as failed — so the symptom points anywhere but the cause.

Parallelism is detected through an exported analysis **Fact**, so a subtest that inherits it from a harness (`newFixture(t)` calling `t.Parallel()` itself) is covered, and a helper added later is picked up with no change to the analyzer. Nothing at the call site says "parallel", which is exactly why a review rule does not catch this.

Writes include assignment, `++`/`--`, `delete` and `clear` — the last two being writes the runtime performs on the caller's behalf that look nothing like assignments — and are followed through a helper closure the enclosing test declared, because `record := func(k string) { results[k] = … }` writes the parent's map just as surely as an inline assignment.

**Deliberately out of scope**, because including any of it would guarantee false positives:

- **Package-level state.** Sharing it under a mutex is legitimate and lock discipline is not statically decidable.
- **Anything declared inside a `for` or `range`.** Go 1.22 gives each iteration its own copy, so the table-driven pattern shares nothing.
- **Writes through a pointer handed to another function**, and `go func(){}` inside a test body. That is the race detector's job.
- **Reads.** Two parallel subtests reading one map is fine.

### `rawsql`

Reports a call that **executes SQL from Go** rather than from a named query file.

All of billet's ledger SQL lives in `internal/state/queries/*.sql`, is compiled by sqlc into `internal/state/ledgerdb`, and is reached through `state.ReadQueries` / `state.WriteQueries`. That is what puts every statement under the guards on that directory — prepared against a real migrated ledger (the only thing that catches an `ON CONFLICT` whose unique index a migration removed, because sqlc models no indexes), classified read-or-write from its own first keyword, checked for a wildcard projection, checked for a byte that would corrupt sqlc's parameter rewriting. A statement written in Go is under none of it.

**Why types rather than a grep.** The only door to the ledger is `database/sql`, so watching that door is sound — but its method names are ordinary English. `url.URL.Query` exists, this repository calls it in several places, and deciding from a name alone means either false positives (which get a gate deleted) or a hand-maintained exception list (which goes stale silently). So a call counts when the callee is a method with one of `database/sql`'s statement-executing names **and** a signature matching `database/sql`'s: an optional leading `context.Context`, then a `string`, then optionally variadic arguments, returning one of `sql.Result`, `*sql.Rows`, `*sql.Row`, `*sql.Stmt`. Matching the **signature** rather than the receiver is what makes a hand-rolled wrapper interface covered instead of an escape hatch; matching the **results** is what keeps every unrelated `Query` method quiet.

**Default-deny across the module, with no scope list and no flag.** A new package that reaches the ledger is refused rather than silently admitted. It could be written that way because the conversions came first: `internal/alloc` and `internal/rollout` were generated before this was turned on, so it arrives with zero unexplained violations rather than a backlog.

**Deliberately out of scope:**

- `_test.go` files. The same exemption `depguard`'s `ledgerwriters` rule already makes, for the same reason: the invariant is about what a transaction does in a deployment, and a test binary is not that. `internal/state`'s own tests assert things about the ledger's schema that can only be asked directly.
- Generated files, as `go/ast` reports them. `internal/state/ledgerdb` **is** the compiled query set; its statements are the `.sql` files.

Thirteen sites in `internal/state` are exempt by directive, each listed in the allowlist table in `internal/state/queries/README.md` — and `TestTheAllowlistTableNamesEveryRawStatement` compares the table against the directives in both directions, because two records of one fact drift.
