# Measured facts

Part of the `billet-checks-and-lint` skill: what was measured, dated where it was recorded.

- `make lint` on darwin alone missed a linux-only conversion error that CI caught; hence the second pass.
- `errcheck` on tests: 19 sites, two real bugs, so it is not relaxed for `_test.go`.
- `^`-anchored exclusion paths: 612 findings resurfaced in a second checkout because the cache is content-keyed.
- `wsl_v5`: 5882 findings on this tree. Rejected.
- `ledgerwriters`: zero violations at introduction.
- `parallelshared`: zero helpers on this tree make a `T` parallel, zero take more than one.
- Pins: golangci-lint v2.12.2, goreleaser v2.17.1, sqlc v1.31.1, tflint v0.64.0, trivy v0.74.0 (`GOEXPERIMENT=jsonv2` is required to install trivy, measured against Go 1.26).
