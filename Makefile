# billet — development commands.
#
# `make check` is the pre-commit gate. CI runs all of it, so a red check is a red
# CI run — that is what makes the gate worth having. CI does MORE besides:
# `go mod tidy` diff, coverage upload, govulncheck, and cross-builds. So a green
# check is necessary rather than sufficient.

GOLANGCI_VERSION := v2.12.2
BIN              := bin/billet
COVERPROFILE     := coverage.out

.DEFAULT_GOAL := check

.PHONY: check
check: build vet fmt-check lint test ## The pre-commit gate (CI runs this and more)

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

.PHONY: lint
lint: ## golangci-lint (pinned version)
	golangci-lint run --timeout=5m

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
tools: ## Install the pinned linter
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

.PHONY: clean
clean:
	rm -rf bin $(COVERPROFILE)

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
