# trace8 Makefile - thin wrappers over plain go commands, stdlib only.

GO ?= go
GOLANGCI ?= golangci-lint
BINARY ?= bin/trace8
CMD := ./cmd/trace8
ADDR ?= :8080
# Test parallelism caps: -p limits package builds, -parallel limits
# test binaries. Raise both only with RAM to spare.
TEST_P ?= 2
TEST_PARALLEL ?= 2
GO_TEST_FLAGS ?=

.PHONY: all help build test vet lint fmt fmt-check run server clean check

all: check ## Default: vet, test, build

help: ## Show this help
	@grep -E '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'

build: ## Compile the binary into bin/
	mkdir -p bin
	$(GO) build -o $(BINARY) $(CMD)

test: ## Run the full test suite (capped parallelism)
	$(GO) test -p $(TEST_P) -parallel $(TEST_PARALLEL) $(GO_TEST_FLAGS) ./...

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint over the tree
	$(GOLANGCI) run ./...

fmt: ## Format Go files in place
	gofmt -w ./cmd ./internal

fmt-check: ## Fail if Go files need formatting
	test -z "$$(gofmt -l ./cmd ./internal)"

run: ## Run the CLI (example: ARGS="--version" make run)
	$(GO) run $(CMD) $(ARGS)

server: ## Run the local HTTP server (example: ADDR=":8080" make server)
	$(GO) run $(CMD) --server --addr $(ADDR)

clean: ## Remove build output
	rm -rf bin

check: vet test build ## Run vet, tests, then build
