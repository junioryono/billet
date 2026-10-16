# Measured facts

Part of the `billet-checks-and-lint` skill: what was measured, dated where it was recorded.

- `make lint` on darwin alone missed a linux-only conversion error that CI caught; hence the second pass.
- golangci-lint on a single FILE type-checks it without the rest of its package: 22 bogus `undefined` errors on one file (2026-10-15). Lint packages, never files.
- `-race` made `internal/alloc`'s tests 21 times slower: 23 s without it, 491 s with it (2026-10-15). Locally tests run without `-race`; CI runs the race suite.
- golangci-lint's lock is machine-wide: a second run on the machine fails at once unless it passes `--allow-serial-runners`, with which it waits for the first (a lint another project ran failed this gate, 2026-10-06; restated 2026-10-15). Never `--allow-parallel-runners`.
- What a targeted check costs on the 32 GiB development Mac (2026-10-15): `go build ./...` warm 4 s and 1.1 GiB; one package's tests 28 s and 0.9 GiB; golangci-lint on one package 10 s and 1.6 GiB, on the module 52 s and 2.6 GiB; `make docs` 14.7 s and 148 MB.
- `errcheck` on tests: 19 sites, two real bugs, so it is not relaxed for `_test.go`.
- `^`-anchored exclusion paths: 612 findings resurfaced in a second checkout because the cache is content-keyed.
- `wsl_v5`: 5882 findings on this tree. Rejected.
- `ledgerwriters`: zero violations at introduction.
- `parallelshared`: zero helpers on this tree make a `T` parallel, zero take more than one.
- Pins: golangci-lint v2.12.2, goreleaser v2.17.1, sqlc v1.31.1, tflint v0.64.0, trivy v0.74.0 (`GOEXPERIMENT=jsonv2` is required to install trivy, measured against Go 1.26).
