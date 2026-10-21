#!/usr/bin/env bash
# tools.sh: install the pinned golangci-lint, goreleaser, sqlc, tflint and trivy
# (.github/tool-versions.env) with `go install`.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_VERSION"
go install "github.com/goreleaser/goreleaser/v2@$GORELEASER_VERSION"
go install "github.com/sqlc-dev/sqlc/cmd/sqlc@$SQLC_VERSION"
go install "github.com/terraform-linters/tflint@$TFLINT_VERSION"
# GOEXPERIMENT=jsonv2 is required, measured: trivy 0.74 imports encoding/json/v2,
# which Go 1.26 gates behind the experiment. The built binary reports
# "Version: dev" (go install stamps no ldflags) but is the pinned tag's source.
GOEXPERIMENT=jsonv2 go install "github.com/aquasecurity/trivy/cmd/trivy@$TRIVY_VERSION"
