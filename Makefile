# billet — development commands.
#
# `make check` is the pre-commit gate. CI runs all of it, so a red check is a red
# CI run — that is what makes the gate worth having. CI does MORE besides:
# `go mod tidy` diff, coverage upload, govulncheck, and cross-builds. So a green
# check is necessary rather than sufficient.

GOLANGCI_VERSION := v2.12.2
GORELEASER_VERSION := v2.17.1
BIN              := bin/billet
COVERPROFILE     := coverage.out

.DEFAULT_GOAL := check

.PHONY: check
check: no-mutants build vet fmt-check lint test ## The pre-commit gate (CI runs this and more)

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

.PHONY: alert-lifecycle
alert-lifecycle: ## Exercise alert ownership migration and teardown through the Ansible role
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/alert-lifecycle.yml

.PHONY: development-check-mode
development-check-mode: ## Exercise a first Linux development-host dry run before tools exist
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook --check ansible_collections/junioryono/billet/tests/development-check-mode.yml

.PHONY: host-upgrade-order
host-upgrade-order: ## Guard the host role's drain, migration, image, restart, and rollback ordering
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/ansible_collections ansible-playbook ansible_collections/junioryono/billet/tests/host-upgrade-order.yml

.PHONY: package-lifecycle
package-lifecycle: dist ## Install and remove both Linux packages without losing operator state
	scripts/test-package-lifecycle.sh

tools: ## Install the pinned linter and goreleaser
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

# CI installs goreleaser itself and reads the pinned version from here, so the
# gate and `make tools` cannot drift to two different goreleasers.
.PHONY: print-goreleaser-version
print-goreleaser-version:
	@echo $(GORELEASER_VERSION)

.PHONY: clean
clean:
	rm -rf bin $(COVERPROFILE)

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
