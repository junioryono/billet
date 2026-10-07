# cmd/billet

The binary: the `server` and `node` roles and the whole operator CLI. It is the only package of the binary allowed stdout and `os.Exit` (tests and the standalone tools under `scripts/` have their own exemptions in `.golangci.yml`). #356 Phase 4 moves its logic into `internal/cli` and `internal/ops`; until then, load the skill for the area a command belongs to (CLAUDE.md's table).

- The order a command acts in is often its whole safety content, and structural tests (`upgradefence_test.go`, `standbystop_test.go` and others) assert it by parsing this package's source. Move a call and its test together.
- An ordinary command opens the ledger through `ledger.go`'s helpers, and the control plane's own modes are `internal/app`'s `OpenLedger`; both apply the release-watermark rules (`ledgerrelease_test.go` and `internal/app/ledger_test.go` read them). Retirement opens its own restricted handles (admin, inspection, completion) in `serverretire*.go`; never replace one with an ordinary helper, and load `billet-controller-retirement` before touching them.
- Coverage ignores `main.go` (`.codecov.yml`), so new logic goes in another file. The server role's assembly is `internal/app`'s (`OpenControlPlane`, `BecomeController`, the `Controller`'s steps), and `runServer` only invokes it; the node role follows (#356 Phase 3c).
- This package's tests are CI's critical path (1,364s in the last green run), so a new test here should be cheap.

Gates: `make check`; `make cross` before anything touching a build tag.
