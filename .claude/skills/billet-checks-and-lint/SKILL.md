---
name: billet-checks-and-lint
description: "What `make check` runs and why each piece is inside or outside it; golangci-lint (pinned v2.12.2, run for the host and for linux), the depguard layering rules and forbidigo bans with their reasons; billet's own analyzers in tools/lint (parallelshared, rawsql) and the `//billet:ignore` directive; `make cross` and build tags; the mutation-run guards. Load when golangci-lint or billetlint fails, when tempted to suppress a finding, when adding a package or an import that crosses a layer, when touching .golangci.yml, tools/lint or the Makefile, or when a change touches a build tag."
---

# The gate, the linters, and the layering

## What this area is

`make check` is the pre-commit gate and CI runs everything in it plus more. It is `no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources`, in that order. `lint` is golangci-lint at the pinned version in `.golangci.yml`; `lint-custom` is billet's own analyzers in the nested module `tools/lint`; `test` is the race-and-coverage instrumented suite. The layering between packages is enforced by `depguard` rules in `.golangci.yml`, not by convention. A lint failure is fixed at the cause; a suppression needs a reason and is the exception.

## Rules

**A green `make check` is necessary, not sufficient.** CI additionally proves `go mod tidy` leaves no diff, runs the suite against a real PostgreSQL and re-runs `internal/alloc` with `BILLET_TEST_LEDGER=postgres`, runs `govulncheck`, cross-builds every node target, runs `sqlc diff`, the Ansible scenario targets, the package and restore rehearsals, every terraform gate and the Sphinx build. A change touching `.tf` files needs `make tf-fmt-check tf-validate tf-test tf-lint tf-scan` before pushing; those are outside `check` because they need terraform, tflint and trivy installed (`make tools` installs the pins).

**Lint runs twice, and the second pass is the one that speaks for production.** A linter only analyses the files it would compile. billet is developed on darwin and runs on linux, so `make lint` runs golangci-lint for the host and again with `GOOS=linux`, and `make lint-custom` runs `billetlint` for darwin/arm64 and linux/amd64. A `uint64(stat.Rdev)` in the firecracker backend is a redundant conversion on linux and a required one on darwin; it passed locally and failed CI, and three defects on one branch had that shape.

**Coverage instrumentation is part of the gate.** `make test` runs `-race -count=1 -covermode=atomic`. The atomic counters change goroutine timing enough to reorder interleavings a plain `-race` build schedules the same way every time; a launch handed to teardown mid-flight was invisible without the flag and reliable with it. A local gate weaker than CI trains you to trust it.

**A finding is fixed, or suppressed with a reason that names the linter.** `nolintlint` runs with `require-explanation` and `require-specific`, so a bare `//nolint` is itself a finding, and `allow-unused: false` reports a directive that no longer covers anything. Write `//nolint:<linter> // <reason>`. billet's own analyzers use `//billet:ignore <analyzer> // <reason>` with the same three rules: a bare directive is reported, an unused one is reported, and a trailing directive covers only its own line while a directive alone on its line covers the line below. If several exceptions want the same reason, the code is asking for a helper (`readAppliedMigrations` replaced three `rows.Close()` annotations).

**When a bug is a class, encode it, in this order.** A golangci-lint setting (a `forbidigo` ban, a `depguard` rule) is cheapest. A `go/analysis` analyzer in `tools/lint` is next, wired into CI only if it measures about zero current violations, because an analyzer that arrives with a backlog is one people route around (`wsl_v5` measured 5882 findings on this tree and was rejected on that number alone; a linter that wants every test named after the function under test flagged about half the suite, because billet names tests for the invariant they assert). A test over source text is third: `TestNoGoBodyIsWrittenOnOneLine` in `scripts/` refuses one-line bodies, `TestQueryFilesAreASCIIOnly` and `TestNoQueryUsesAWildcardProjection` in `internal/state` police the query files. What needs judgment goes in a skill.

**The depguard rules are the architecture.** Read them in `.golangci.yml`; the ones that carry weight:

- `config` may import no other billet package. It is a leaf, so validation rules that `alloc` also needs are exported from `config` and called from both.
- `provider` and `store` are siblings below the scheduler: neither imports the other, neither imports `server`, `node` or `cmd`.
- `ledgerwriters` (`internal/state`, `internal/alloc`, `internal/rollout`, non-test files) may not import `net/http`, `os/exec`, `provider`, `store`, `github`, `scaleset`, `nodeplane`, `nodeclient`, `node`, `server` or `cmd`. `DB.Tx` begins IMMEDIATE and holds SQLite's single writer slot from BEGIN, so a network call inside a transaction stalls every scheduling write. Measured at zero violations before it went in, which is the bar for any rule in CI.
- `sqlitedriver` confines `modernc.org/sqlite` and `github.com/jackc/pgx` to `internal/state`, which verifies WAL and `synchronous=FULL`, takes the process lock and serialises writers.
- `scalesetclient` confines `github.com/actions/scaleset` (a public preview) to `internal/scaleset`.
- `generatedqueries` lets only `state`, `alloc` and `rollout` import `internal/state/ledgerdb`, for its parameter and row types; the handle comes from `state.ReadQueries`/`state.WriteQueries`, and `forbidigo` bans `ledgerdb.New` everywhere but the direct members of `internal/state`.
- `sqlcgenerated`: generated code imports nothing of billet's.
- `lifeops` is a strict allowlist (`$gostd`, `deploy`, `config`, `lifeops`, `golang.org/x/sys/unix`), every entry `$`-anchored because depguard matches a prefix; `unix.Access` is admitted so a `--dry-run` can ask "can this account write here" without writing.
- Global bans: `github.com/pkg/errors`, `io/ioutil`, logrus/zap/zerolog (billet uses `log/slog`), `math/rand` (use v2 or `crypto/rand`), `github.com/mattn/go-sqlite3` (cgo ends the single static binary).

**The forbidigo bans and why.** `fmt.Print*` (operator output belongs to `cmd/billet`), `panic` (a control plane that panics drops every in-flight lease), `os.Exit`, `http.Get/Post/PostForm/Head`, `context.Background/TODO`, `time.After` (leaks its timer until it fires), `errors.As` (use `errors.AsType[T]`, so a target cannot be used outside the branch that proved it), and `ledgerdb.New`. `analyze-types: true` so an import alias does not evade a ban. `inamedparam` requires named interface parameters because an interface is a contract somebody else implements.

**Exclusions are anchored with `(^|/)`, and that was measured.** golangci-lint's cache is keyed on file content and shared across checkouts; a `^cmd/billet/` anchor stopped matching in a second checkout and 612 previously excluded findings came back as failures against a tree with nothing wrong. Test files relax `gosec`, `contextcheck`, `containedctx`, `nilnil`, `nonamedreturns`, `forbidigo`, `revive`, `dogsled` and `prealloc`, and deliberately not `errcheck`: measured at 19 sites, two of which were real bugs. `.claude/` is excluded because sessions park worktrees there and a pass was linting another branch's half-finished code.

**`tools/lint` is a nested module on purpose.** `golang.org/x/tools/go/analysis` must not become a dependency of the billet binary, which ships with four direct ones. The consequence is that `go test ./...` from the root cannot reach the analyzers' own tests, so `make lint-custom` runs `(cd tools/lint && go test ./...)` before the binary. Without that an analyzer could silently stop detecting anything and still report zero violations, which reads exactly like a clean tree.

**`parallelshared` reports state a parallel subtest writes and its parent test owns.** `paralleltest` and `tparallel` reason about where `t.Parallel()` is placed and neither models what a parallel closure mutates; two parallel subtests writing one map is `fatal error: concurrent map writes`, which aborts the whole binary and blames every unrelated test. Parallelism is propagated through an analysis Fact so a helper added later is covered. It deliberately does not guess at package-level state, anything declared inside a `for`/`range` (Go 1.22 gives each iteration its own copy, so the table-driven pattern shares nothing), writes through a pointer, reads, a merely-declared function literal, a closure reached non-lexically, or which parameter a parallel helper makes parallel (zero such helpers exist, measured).

**`rawsql` reports SQL executed from Go rather than named in a query file.** It decides by signature, not by name: a callee with one of `database/sql`'s statement-executing names and a matching signature (`(context.Context, string, ...any)` returning `(sql.Result, error)`, `(*sql.Rows, error)`, `*sql.Row` or `(*sql.Stmt, error)`, plus the four non-context forms). Signature matching is what covers a hand-rolled wrapper like `state.Querier` or sqlc's `DBTX`, and result-type matching is what keeps `url.URL.Query` quiet. It is default-deny across the module with no scope list and no flag; it skips `_test.go` and generated files. The 21 exemptions are each a `//billet:ignore rawsql` with a row in `internal/state/queries/README.md`, and `TestTheAllowlistTableNamesEveryRawStatement` compares the two in both directions. What escapes it is written down rather than implied: a file falsely marked generated, a dependency reaching `database/sql` under its own name, and `sql.Conn.Raw`.

**`make cross` before anything touching a build tag.** A node is deployed by copying one static file, so a build-tag mistake on a platform nobody develops on is invisible until it reaches that machine. Production tags: `internal/state/lock_unix.go`, `cmd/billet/open_unix.go`/`open_other.go`, `owner_unix.go`/`owner_other.go`, `internal/state/searchdir_{darwin,freebsd,linux,other}.go`, `internal/provider/firecracker/pidfd_linux.go`/`pidfd_other.go`. Tests carry exactly one `//go:build` line (`unix`); everything else is gated by environment variables and `t.Skip`.

**`no-mutants` runs first and `tests-kept` runs outside.** `scripts/check-no-mutants.py` refuses to proceed while a `.bak` sits beside a tracked Go file, because an interrupted mutation run leaves the original holding a mutant that compiles and mostly passes; it is first because it cannot be a false alarm. `scripts/check-tests-kept.py` reports `Test` functions HEAD has and the tree does not; it is deliberately not in `check` because deleting a test is sometimes right, and it exists because a scripted edit once swallowed a neighbouring test with every gate green.

**`sqlc` and `sqlc-check` are outside `check` for the opposite reason to the terraform gates.** The generated query code is committed precisely so an ordinary build never downloads sqlc; CI installs the pin (`SQLC_VERSION` in the Makefile, read by the workflow so the two cannot drift) and runs `sqlc diff` in its own job. Run `make sqlc` after editing `internal/state/queries` or adding a migration, because sqlc reads the migration history as its schema.

**Go comments wrap at 88 columns; Markdown never wraps.** Go source follows the surrounding file. Every `.md` and `.txt` file is one paragraph per line.

## Measured facts

- `make lint` on darwin alone missed a linux-only conversion error that CI caught; hence the second pass.
- `errcheck` on tests: 19 sites, two real bugs, so it is not relaxed for `_test.go`.
- `^`-anchored exclusion paths: 612 findings resurfaced in a second checkout because the cache is content-keyed.
- `wsl_v5`: 5882 findings on this tree. Rejected.
- `ledgerwriters`: zero violations at introduction.
- `parallelshared`: zero helpers on this tree make a `T` parallel, zero take more than one.
- Pins: golangci-lint v2.12.2, goreleaser v2.17.1, sqlc v1.31.1, tflint v0.64.0, trivy v0.74.0 (`GOEXPERIMENT=jsonv2` is required to install trivy, measured against Go 1.26).

## Where the tests are

- `tools/lint/analyzer/parallelshared`, `tools/lint/analyzer/rawsql` and `tools/lint/suppress` carry their own testdata; `TestABareDirectiveIsReported` covers the case testdata cannot express.
- `internal/state/rawsqlallowlist_test.go`: `TestTheAllowlistTableNamesEveryRawStatement`.
- `scripts/onelinerblock_test.go`: `TestNoGoBodyIsWrittenOnOneLine`; `scripts/golangci_exclusions_test.go` pins the exclusion anchoring.
- `internal/state/queryset_test.go`: the query-file rules.

## Related skills

`billet-testing` (the mutation discipline the guards protect), `billet-state` (why the ledger rules exist), `billet-shell-gates` (the same "a gate must be able to fail" rule applied to shell), `billet-git-flow` (when the gate runs).
