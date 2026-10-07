---
name: billet-checks-and-lint
description: "Load when `make check`, golangci-lint or billetlint fails; when tempted to suppress a finding; when adding a package or an import that crosses a layer; when touching .golangci.yml, tools/lint, the Makefile, scripts/ci-scope.sh or ci.yml; or when a change touches a build tag."
---

# The gate, the linters, and the layering

## What this area is

`make check` is the pre-commit gate, and CI runs everything in it plus more on every push to main. On a pull request `scripts/ci-scope.sh` selects JOB FAMILIES from the merge commit's diff (`HEAD^1..HEAD`, `--no-renames`, both sides of every record): ten `<family>=true|false` outputs (`host_lifecycle`, `postgres`, `postgres_command`, `postgres_retirement`, `package_lifecycle`, `terraform`, `terraform_consumer_floor`, `terraform_lint`, `replay`, `lint`) plus `schema=2`, written straight to `$GITHUB_OUTPUT`. `test`, `release-config` and `docs` always run (`KEPT`); every other job belongs to exactly one family (`FAMILY` in `verify-all-checks`) and carries exactly `if: needs.changes.outputs.<family> == 'true'`. Each rule is a SAFE SUPERSET of what the family reads, derived from its steps, the Makefile targets they run and the Go closures they build, not from names: `cmd/`, `internal/` and `deploy/` select every family that builds or drives billet; non-test Go also selects `postgres` (its raw-SQL walk reads every Go file); the replay harness's own package set selects `replay`; `ansible_collections/` selects the host and command/retirement PostgreSQL families; Terraform, packaging and lint inputs select theirs. `.github/`, the `Makefile`, any `go.mod`/`go.sum`, `sqlc.yaml` and the classifier select everything, and so do a push, a merge commit it cannot read, a mode change, a symlink, a submodule and ANY PATH NO RULE NAMES: an unknown path never selects nothing. Documentation is file kinds, not directories (the Sphinx build's own kinds under `docs/`, `.md` under `.claude/`, `CLAUDE.md`, each a regular non-executable file; never `LICENSE` or the root `README.md`, which are package inputs). `verify-all-checks` judges each job by its own family: a KEPT job must succeed, a selected job must succeed, an unselected one may succeed or be skipped, a push must select every family, and the FAMILY map must cover every job it needs; a selected job skipped because what it needs failed is a failure. `scripts/ci_scope_test.go` holds the classifier's cases, the per-path map, the verdict and the structure. A job added to `ci.yml` must be in `verify-all-checks`' needs and either KEPT or in one family with that family's gate; a new input to a family must be added to its rule in the same PR, or the classifier skips it. It is `no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources`, in that order. `lint` is golangci-lint at the pinned version in `.golangci.yml`; `lint-custom` is billet's own analyzers in the nested module `tools/lint`; `test` is the race-and-coverage instrumented suite. The layering between packages is enforced by `depguard` rules in `.golangci.yml`, not by convention. A lint failure is fixed at the cause; a suppression needs a reason and is the exception.

Each invariant below is one line here and stated in full, with the incident or measurement behind it, in the reference file its group names. Read the reference before changing anything the invariant covers.

## Invariants

### The gate and CI: [references/gate.md](references/gate.md)

- **A green `make check` is necessary, not sufficient.**
- **Lint runs twice, and the second pass is the one that speaks for production.**
- **Coverage instrumentation is part of the gate.**
- **A local gate shares the machine: one gate per user at a time across projects, `go test -p 4`, under `nice`.**
- **A finding is fixed, or suppressed with a reason that names the linter.**
- **When a bug is a class, encode it, in this order.**
- **`make cross` before anything touching a build tag.**
- **`no-mutants` runs first and `tests-kept` runs outside.**
- **`sqlc` and `sqlc-check` are outside `check` for the opposite reason to the terraform gates.**
- **Go comments wrap at 88 columns; Markdown never wraps.**
- **Superseded CI runs are force-cancelled.**

### The layering and the bans: [references/layering-and-bans.md](references/layering-and-bans.md)

- **The depguard rules are the architecture.**
- **The forbidigo bans and why.**
- **Exclusions are anchored with `(^|/)`, and that was measured.**
- **One gRPC server, in one package.**

### billet's own analyzers: [references/analyzers.md](references/analyzers.md)

- **`tools/lint` is a nested module on purpose.**
- **`parallelshared` reports state a parallel subtest writes and its parent test owns.**
- **`rawsql` reports SQL executed from Go rather than named in a query file.**

Measured facts for this area, dated: [references/measured-facts.md](references/measured-facts.md).

## Checklists

**A finding.** Fix the cause. A suppression is `//nolint:<linter> // <reason>` (or `//billet:ignore <analyzer> // <reason>`); a bare or unused directive is itself reported.

**A new rule.** Encode a class of bug in the cheapest place that can hold it: a golangci-lint setting, then an analyzer in `tools/lint`, then a test over source text. Wire it into CI only once it measures about zero violations on the tree ([references/gate.md](references/gate.md)).

**A new CI job or input.** Add the job to `verify-all-checks`' needs and to KEPT or one family, and a new input to its family's rule in `scripts/ci-scope.sh`, in the same PR; see What this area is.

## Where the tests are

- `tools/lint/analyzer/parallelshared`, `tools/lint/analyzer/rawsql` and `tools/lint/suppress` carry their own testdata; `TestABareDirectiveIsReported` covers the case testdata cannot express.
- `internal/state/rawsqlallowlist_test.go`: `TestTheAllowlistTableNamesEveryRawStatement`.
- `scripts/onelinerblock_test.go`: `TestNoGoBodyIsWrittenOnOneLine`; `scripts/golangci_exclusions_test.go` pins the exclusion anchoring.
- `internal/state/queryset_test.go`: the query-file rules.

## Related skills

`billet-testing` (the mutation discipline the guards protect), `billet-state` (why the ledger rules exist), `billet-shell-gates` (the same "a gate must be able to fail" rule applied to shell), `billet-git-flow` (when the gate runs).
