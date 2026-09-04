# billet — development commands.
#
# `make check` is the pre-commit gate. CI runs all of it, so a red check is a red
# CI run — that is what makes the gate worth having. CI does MORE besides:
# `go mod tidy` diff, coverage upload, govulncheck, cross-builds, the Ansible
# role tests, and the terraform gates (tf-fmt-check/tf-validate/tf-test/tf-lint/
# tf-scan — run those before pushing a .tf change). So a green check is
# necessary rather than sufficient.

GOLANGCI_VERSION := v2.12.2
GORELEASER_VERSION := v2.17.1
SQLC_VERSION     := v1.31.1
TFLINT_VERSION   := v0.64.0
TRIVY_VERSION    := v0.74.0
# EVERY top-level module, DISCOVERED rather than listed. The gates used to name
# TF_MODULE alone, and the comment below explained that validate and test load its
# CHILDREN as called modules -- true, and it left every SIBLING invisible. Two new
# modules were added, went green through the whole terraform suite, and had never
# been validated, linted or scanned by any of it. A list would have had the same
# failure one module later, so this is a wildcard.
TF_MODULES       := $(patsubst %/,%,$(sort $(dir $(shell find terraform/modules -name '*.tf' -not -path '*/.terraform/*' -exec dirname {} \; | sed 's|\(terraform/modules/[^/]*\).*|\1/|' | sort -u))))
BIN              := bin/billet
COVERPROFILE     := coverage.out

.DEFAULT_GOAL := check

.PHONY: check
check: no-mutants build vet fmt-check lint lint-custom test lambda-test module-sources ## The pre-commit gate (CI runs this and more)

.PHONY: build
build: ## Build ./bin/billet
	@mkdir -p bin
	CGO_ENABLED=0 go build -o $(BIN) ./cmd/billet

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: ## Race-enabled test run, instrumented exactly as CI runs it
	# COVERAGE INSTRUMENTATION IS PART OF THE GATE, not an extra. CI runs the
	# suite with -covermode=atomic, and that is not the same test run: the
	# counters change timing enough to reorder goroutines the plain -race build
	# happens to schedule one way every time.
	#
	# This is not hypothetical. A launch still in progress was being handed to
	# teardown — it carries no outcome, so the release failed with `invalid phase
	# transition: "" is not terminal` — and the bug was invisible here while being
	# reliable under coverage. A local gate that is weaker than CI trains you to
	# trust it and then be surprised.
	go test -race -count=1 -covermode=atomic -coverprofile=$(COVERPROFILE) ./...

.PHONY: cover
cover: ## Coverage profile + HTML report
	go test -race -count=1 -coverprofile=$(COVERPROFILE) -covermode=atomic ./...
	go tool cover -func=$(COVERPROFILE) | tail -1
	go tool cover -html=$(COVERPROFILE)

.PHONY: no-mutants
no-mutants: ## Refuse to proceed while a killed mutation run has left a mutant on disk
	@# FIRST in `check`, unlike tests-kept, because this one cannot be a false
	@# alarm: a `.bak` beside a tracked file means an interrupted mutation run
	@# left the ORIGINAL holding a mutant. It compiles and mostly passes, so
	@# every other gate is happy to tell you so.
	python3 scripts/check-no-mutants.py

.PHONY: tests-kept
tests-kept: ## Report Test functions that HEAD has and the working tree does not
	@# NOT part of `check`, because deleting a test is sometimes right and this
	@# cannot tell. It is for scripted edits to _test.go files, where a replaced
	@# range can swallow a neighbouring test and every gate stays green — a
	@# deleted test cannot fail. That has happened once; see the script's header.
	python3 scripts/check-tests-kept.py

.PHONY: module-sources
module-sources: ## Prove every documented Terraform module source names a version that carries it
	@# IN `check` RATHER THAN WITH THE tf-* TARGETS, which are outside it because
	@# they need terraform, tflint and trivy installed. This one is git and shell,
	@# so it can fail on the commit that breaks it instead of on the release.
	EXPECTED_REF=main scripts/check-module-sources.sh

.PHONY: lambda-test
lambda-test: ## Unit-test the Terraform spot-router Lambda (stdlib only, boto3 stubbed)
	@# The router's error CLASSIFICATION (drop vs re-raise) decides whether a real
	@# two-minute Spot warning is lost, and a Terraform plan test cannot reach it.
	python3 -m unittest discover -s terraform/modules/billet/modules/fleet-ec2/lambda -p '*_test.py'

.PHONY: docs
docs: ## Build the Sphinx documentation with warnings as errors, as Read the Docs and CI do
	@build_dir=$$(mktemp -d); \
		trap 'rm -rf "$$build_dir"' EXIT; \
		$(MAKE) -C docs html BUILDDIR="$$build_dir" SPHINXOPTS="-W --keep-going"

.PHONY: lint
lint: ## golangci-lint (pinned version), for this platform AND linux
	golangci-lint run --timeout=5m
	@# AND AGAIN FOR LINUX, because a linter only analyses the files it would
	@# compile. billet is developed on darwin and RUNS on linux, so every linux-only
	@# file, and every branch of a platform-dependent type, is unexamined by the pass
	@# above. Not theoretical: `uint64(stat.Rdev)` in the firecracker backend is a
	@# redundant conversion on linux and a required one on darwin, so it passed here
	@# and failed CI. Three defects on one branch had exactly this shape.
	@#
	@# It costs one more pass, and it is the only way this gate speaks for the
	@# platform the thing actually runs on.
	GOOS=linux golangci-lint run --timeout=5m

.PHONY: lint-custom
lint-custom: ## billet's own analyzers, and the tests that prove they still detect
	@# IN `check` because it needs nothing installed: tools/lint is a nested module
	@# built with the Go toolchain that is already here. It is nested so
	@# golang.org/x/tools/go/analysis never becomes a dependency of the billet
	@# binary, which ships with four direct ones.
	(cd tools/lint && go build -o /tmp/billetlint ./cmd/billetlint)
	@# The analyzers' OWN tests, which `go test ./...` from here cannot reach
	@# across the module boundary. Without this an analyzer could silently stop
	@# detecting anything and still report zero violations -- which reads exactly
	@# like a clean tree, and is the failure this whole directory is arranged
	@# against.
	(cd tools/lint && go test ./...)
	@# ONCE PER PLATFORM, for the reason `lint` above does two passes: an analyzer
	@# only sees the files it would COMPILE, so a single pass on darwin never
	@# examines a linux-only test and a single pass on linux never examines a
	@# darwin-only one. billet is developed on darwin and runs on linux, so one
	@# pass is guaranteed to miss whichever half the machine is not.
	@#
	@# Diagnostics go to stderr, and the binary exits non-zero on any finding.
	@set -e; for target in darwin/arm64 linux/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  billetlint $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch /tmp/billetlint -parallelshared -rawsql ./...; \
	done

.PHONY: lint-fix
lint-fix:
	golangci-lint run --fix --timeout=5m

.PHONY: fmt
fmt:
	gofmt -s -w .

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt -s:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: sqlc
sqlc: ## Regenerate internal/state/ledgerdb from the query files
	@# NOT part of `check`: the generated code is committed precisely so an
	@# ordinary build never downloads sqlc. Run this after editing anything in
	@# internal/state/queries -- or after adding a MIGRATION, which is an input to
	@# code generation here, because sqlc reads the migration history rather than a
	@# flattened schema.
	sqlc generate

.PHONY: sqlc-check
sqlc-check: ## Prove the committed query code is what sqlc generates
	@# `sqlc diff` writes nothing and exits 1 on drift (measured). It cannot see a
	@# generated file ORPHANED by a deleted query, because sqlc never removes
	@# output -- TestEveryGeneratedQueryIsNamedInASourceFile covers that direction.
	sqlc diff

# A node is deployed by copying one file, so a build-tag mistake on a platform
# nobody develops on is invisible until it reaches that machine.
.PHONY: cross
cross: ## Build every target a node can run on
	@for target in linux/amd64 linux/arm64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o /dev/null ./cmd/billet || exit 1; \
	done

.PHONY: tools
.PHONY: dist
dist: ## Build the release artifacts locally, exactly as a tag would
	@# NOT part of `check`: it needs goreleaser installed and takes twenty seconds
	@# to cross-compile three targets, neither of which belongs in a gate that runs
	@# before every commit. Run it when touching .goreleaser.yaml.
	goreleaser release --snapshot --clean --skip=publish

# The terraform targets are NOT in `check`: they need terraform/tflint/trivy
# installed, which the Go gate must not depend on. CI runs them on every PR.
# tf-fmt-check sweeps all of terraform/; the rest iterate TF_MODULES, which is
# every top-level module found under terraform/modules. validate and test also
# reach each module's CHILDREN, because a root loads them as called modules --
# that was once the whole story, and it is why a sibling module could be added
# and never gated.

.PHONY: tf-modules-check
tf-modules-check: ## Refuse an empty module list, which would make every gate vacuously pass
	@# EVERY TOP-LEVEL DIRECTORY MUST BE ACCOUNTED FOR, not merely "the list is
	@# non-empty". A module whose config is .tf.json -- which terraform accepts and
	@# `find -name '*.tf'` does not -- would otherwise be skipped in silence while
	@# every gate reported success, which is this same failure one turn further on.
	@set -e; missing=""; \
	for d in $$(find terraform/modules -mindepth 1 -maxdepth 1 -type d); do \
		case " $(TF_MODULES) " in *" $$d "*) ;; *) missing="$$missing $$d";; esac; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "terraform module directories with no discovered .tf:$$missing" >&2; \
		echo "every gate would pass without examining them" >&2; \
		exit 1; \
	fi
	@test -n "$(strip $(TF_MODULES))" || { \
		echo "no terraform modules discovered under terraform/modules; every terraform gate would pass without examining anything" >&2; \
		exit 1; \
	}
	@echo "terraform modules: $(TF_MODULES)"

.PHONY: tf-fmt-check
tf-fmt-check: ## terraform fmt, recursively, as a gate
	terraform fmt -check -recursive terraform

.PHONY: tf-classify
tf-classify: ## Prove the plan classification describes every module resource, and nothing else
	@# WHAT A CHANGE COSTS A RUNNING DEPLOYMENT is committed beside the module as
	@# classification.json, because `terraform plan` cannot say it: ADR-004 keeps
	@# live billet nodes outside Terraform, so a plan does not know these hosts are
	@# running somebody's build and "1 to change" reads the same for a tag and for
	@# the instance holding the ledger.
	@#
	@# THIS IS THE VACUITY GATE, not the classifier. A resource with no entry is
	@# one `tfclassify` refuses on — safe at apply time, and far too late, because
	@# the first person to meet it is an operator mid-change rather than whoever
	@# added the resource. It needs no terraform, no provider plugins and no
	@# network, so it runs in the Go job as well as the terraform one.
	@#
	@# Classifying an actual plan is the operator's invocation, and it is separate
	@# because it needs a plan:
	@#   terraform -chdir=terraform/modules/billet plan -out=tfplan
	@#   terraform -chdir=terraform/modules/billet show -json tfplan > plan.json
	@#   go run ./scripts/tfclassify -plan plan.json
	go test -count=1 ./internal/tfclass/

.PHONY: tf-validate
tf-validate: tf-modules-check ## Validate every Terraform module (init downloads providers, no credentials)
	@set -e; for m in $(TF_MODULES); do \
		echo "validate $$m"; \
		terraform -chdir=$$m init -input=false -backend=false > /dev/null; \
		terraform -chdir=$$m validate; \
	done

.PHONY: tf-test
tf-test: tf-modules-check ## Plan-test every Terraform module that has tests, against mocked providers
	@set -e; for m in $(TF_MODULES); do \
		if [ -d "$$m/tests" ]; then \
			echo "test $$m"; \
			terraform -chdir=$$m init -input=false > /dev/null; \
			terraform -chdir=$$m test; \
		else \
			echo "test $$m: no tests/ directory, skipped"; \
		fi; \
	done

.PHONY: tf-lint
tf-lint: tf-modules-check ## tflint with the aws ruleset pinned in .tflint.hcl, over every module
	@# --config TAKES AN ABSOLUTE PATH. tflint resolves a relative config against
	@# --chdir, so a shared file named relatively would silently not be found and
	@# the module would be linted with bundled rules only.
	@set -e; cfg="$(CURDIR)/terraform/.tflint.hcl"; \
	for m in $(TF_MODULES); do \
		echo "tflint $$m"; \
		tflint --chdir=$$m --config="$$cfg" --init; \
		tflint --chdir=$$m --config="$$cfg" --recursive; \
	done

.PHONY: tf-scan
tf-scan: tf-modules-check ## trivy config scan; every ignore in .trivyignore carries its justification
	@# --skip-check-update pins the rego bundle to the one embedded in the pinned
	@# binary — without it trivy fetches new checks daily and an unrelated PR
	@# goes red, the exact failure mode every other pin here exists to prevent.
	@# The second pass scans with every feature flag on: trivy expands count, and
	@# spot/KMS default off, so the default pass alone never sees that surface.
	@# Each module carries its OWN ignorefile when it needs one, because an ignore
	@# is a justification about that module's resources and borrowing another's
	@# silences a finding nobody argued for.
	@set -e; for m in $(TF_MODULES); do \
		echo "trivy $$m"; \
		ignore=""; [ -f "$$m/.trivyignore" ] && ignore="--ignorefile $$m/.trivyignore"; \
		trivy config $$m $$ignore --skip-check-update --exit-code 1; \
		if [ -f "$$m/tests/scan.tfvars" ]; then \
			trivy config $$m $$ignore --skip-check-update --tf-vars $$m/tests/scan.tfvars --exit-code 1; \
		fi; \
	done

# CI reads the pinned scanner versions from here, the way it reads the
# goreleaser pin, so the workflow and this file cannot drift apart.
.PHONY: print-tflint-version
print-tflint-version:
	@echo $(TFLINT_VERSION)

.PHONY: print-trivy-version
print-trivy-version:
	@echo $(TRIVY_VERSION)

.PHONY: alert-lifecycle
alert-lifecycle: ## Exercise alert ownership migration and teardown through the Ansible role
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/alert-lifecycle.yml

.PHONY: development-check-mode
development-check-mode: ## Exercise a first Linux development-host dry run before tools exist
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook --check ansible_collections/junioryono/billet/tests/development-check-mode.yml

.PHONY: host-upgrade-order
host-upgrade-order: ## Guard the host role's drain, migration, image, restart, and rollback ordering
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/host-upgrade-order.yml

.PHONY: ledger-mount-render
ledger-mount-render: ## Pin the fail-closed ledger mount units the host role renders
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/ledger-mount-render.yml

.PHONY: postgres-profile
postgres-profile: ## Prove the host role converges a deployment whose ledger is in PostgreSQL
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/postgres-profile.yml

.PHONY: unit-parity
unit-parity: ## Prove the packaged units and the role's templates agree on what matters
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/unit-parity.yml

.PHONY: emitted-block-check
emitted-block-check: ## Converge the host role with a block `billet init --emit ansible` generated
	ansible_collections/junioryono/billet/tests/emitted-block-check.sh

.PHONY: example-check
example-check: ## Converge examples/single-host-docker with a generated block
	ansible_collections/junioryono/billet/tests/example-check.sh

.PHONY: key-policy-check
key-policy-check: ## Prove the role's GitHub App key policy, owned path and foreign
	ansible_collections/junioryono/billet/tests/key-policy-check.sh

.PHONY: converge-guard-check
converge-guard-check: ## Prove a converge driven from a billet-managed runner is refused
	ansible_collections/junioryono/billet/tests/converge-guard-check.sh

.PHONY: release-fetch-check
release-fetch-check: ## Prove the billet_version fetch path and the URL it builds
	ansible_collections/junioryono/billet/tests/release-fetch-check.sh

.PHONY: firecracker-example-check
firecracker-example-check: ## Prove the role accepts a firecracker emission, up to the hardware
	ansible_collections/junioryono/billet/tests/firecracker-example-check.sh

.PHONY: fetch-retry-check
fetch-retry-check: ## Prove every network fetch in both roles is bounded and retried
	ansible_collections/junioryono/billet/tests/fetch-retry-check.sh

.PHONY: acceptance
acceptance: ## Run an ISOLATED acceptance deployment against a real account, and destroy exactly what it makes
	@# NOT IN `check`, and not in CI's ordinary jobs: it launches billable compute
	@# in a real AWS account and needs a real GitHub App. .github/workflows/
	@# acceptance.yml is what runs it on a schedule, against the dedicated account
	@# docs/reference/records/aws-acceptance.md names.
	@#
	@# BILLET_ACCEPTANCE_CONFIG is the config to DERIVE FROM; the run never writes
	@# to it, and every path, port, tier label and deployment identity it uses is
	@# its own.
	@test -n "$(BILLET_ACCEPTANCE_CONFIG)" || { \
		echo "set BILLET_ACCEPTANCE_CONFIG to the billet.yaml to derive an isolated run from" >&2; \
		echo "  make acceptance BILLET_ACCEPTANCE_CONFIG=/etc/billet/billet.yaml BILLET_ACCEPTANCE_ACCOUNT=..." >&2; \
		exit 2; \
	}
	scripts/acceptance.sh --config $(BILLET_ACCEPTANCE_CONFIG) \
		$(if $(BILLET_ACCEPTANCE_ACCOUNT),--account $(BILLET_ACCEPTANCE_ACCOUNT),) \
		$(if $(BILLET_ACCEPTANCE_REGION),--region $(BILLET_ACCEPTANCE_REGION),) \
		$(if $(BILLET_ACCEPTANCE_JOBS),--jobs $(BILLET_ACCEPTANCE_JOBS),)

.PHONY: host-fresh-check
host-fresh-check: ## Check-mode converge of the host role against a FRESH machine (the collection README's documented first step)
	ansible_collections/junioryono/billet/tests/host-fresh-check.sh

.PHONY: package-lifecycle
package-lifecycle: dist ## Install and remove both Linux packages without losing operator state
	scripts/test-package-lifecycle.sh

.PHONY: restore-rehearsal
restore-rehearsal: dist ## Back a deployment up and restore it onto a packaged Linux host that has never seen it
	@# AN UNTESTED BACKUP IS NOT A BACKUP (ADR-001), and until this existed the
	@# claim "this deployment can be restored" rested entirely on reading. The Go
	@# half — a restored control plane SERVING the fleet that trusted the old one
	@# — is internal/e2e's TestARestoredDeploymentServesTheFleetThatTrustedTheOldOne
	@# and runs with the ordinary suite. This is the half that needs a real
	@# package: the service account, the state directory's ownership, and the real
	@# binary's `local backup` and `local restore`.
	scripts/test-restore-rehearsal.sh

.PHONY: postgres-restore-rehearsal
postgres-restore-rehearsal: dist ## Rehearse a restore of a deployment whose ledger is in PostgreSQL
	@# ITS OWN TARGET, NOT A FLAG ON THE ONE ABOVE. On this profile billet archives
	@# HALF a deployment on purpose — the ledger is pg_dump's to copy — so the
	@# archive is a different schema, the entry set is one short, and two of the
	@# restore's refusals exist only here. It is also the profile where the archive
	@# matters most: control-plane-postgres has no ledger volume by design, so that
	@# archive is the only copy of the deployment's identity there is.
	scripts/test-postgres-restore-rehearsal.sh

.PHONY: systemd-lifecycle
systemd-lifecycle: dist ## Drive `billet local up`/`down` against real systemd and the real package
	@# THE ONLY TEST OF THE LOCAL LIFECYCLE THAT USES A REAL SERVICE MANAGER.
	@# Every other one supplies a fake, so what systemd does with the units
	@# billet ships — and whether `local down` really leaves them alone when a
	@# host is running compute the ledger cannot see — was unexercised.
	@#
	@# Outside `check` because it needs privileged docker and a config with a
	@# working GitHub App; it SKIPS rather than fails without one, since a fake
	@# credential would exercise a path no deployment takes.
	scripts/test-systemd-lifecycle.sh

tools: ## Install the pinned linter, goreleaser, sqlc, tflint, and trivy
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	go install github.com/terraform-linters/tflint@$(TFLINT_VERSION)
	@# GOEXPERIMENT=jsonv2 is required, measured: trivy 0.74 imports
	@# encoding/json/v2, which Go 1.26 gates behind the experiment. The built
	@# binary reports "Version: dev" (go install stamps no ldflags) but is the
	@# pinned tag's source.
	GOEXPERIMENT=jsonv2 go install github.com/aquasecurity/trivy/cmd/trivy@$(TRIVY_VERSION)

# CI installs goreleaser itself and reads the pinned version from here, so the
# gate and `make tools` cannot drift to two different goreleasers.
.PHONY: print-goreleaser-version
print-goreleaser-version:
	@echo $(GORELEASER_VERSION)

# CI reads the pinned sqlc from here for the same reason: the generation gate
# compares committed output against what a specific sqlc produces, so `@latest`
# would fail an unrelated PR the day sqlc changes its codegen.
.PHONY: print-sqlc-version
print-sqlc-version:
	@echo $(SQLC_VERSION)

.PHONY: clean
clean:
	rm -rf bin $(COVERPROFILE)

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
