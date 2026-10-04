# cmd/billet

The binary: the `server` and `node` roles and the whole operator CLI. It is the only package of the binary allowed stdout and `os.Exit` (tests and the standalone tools under `scripts/` have their own exemptions in `.golangci.yml`). #356 Phase 4 moves its logic into `internal/cli` and `internal/ops`; until then, load the skill for the area a command belongs to (CLAUDE.md's table).

- The order a command acts in is often its whole safety content, and structural tests (`promotionorder_test.go`, `upgradefence_test.go` and others) assert it by parsing this package's source. Move a call and its test together.
- An ordinary command opens the ledger through `ledger.go`'s helpers, which apply the release-watermark rules (`ledgerrelease_test.go` reads that file). Retirement opens its own restricted handles (admin, inspection, completion) in `serverretire*.go`; never replace one with an ordinary helper, and load `billet-controller-retirement` before touching them.
- Coverage ignores `main.go` (`.codecov.yml`), so new logic goes in another file; #356 Phase 3 moves the role assemblies out of it.
- This package's tests are CI's critical path (1,364s in the last green run), so a new test here should be cheap.

Gates: `make check`; `make cross` before anything touching a build tag.
