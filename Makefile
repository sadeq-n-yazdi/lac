BINARY_DIR   := bin
MODULE       := code.sadeq.uk/lac
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS      := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE)

GO           ?= go
GOFLAGS      ?=

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: deps
deps: ## Install the developer tooling
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: tidy
tidy: ## Sync go.mod and go.sum
	$(GO) mod tidy

.PHONY: build
build: ## Build the lacd daemon and the lac CLI into ./bin
	@mkdir -p $(BINARY_DIR)
	$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY_DIR)/ ./cmd/...

.PHONY: run
run: build ## Build and run the daemon in the foreground
	$(BINARY_DIR)/lacd

.PHONY: test
test: ## Run the test suite with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and open the coverage report
	$(GO) test -race -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out

.PHONY: fmt
fmt: ## Format the code
	$(GO) fmt ./...
	golangci-lint fmt

.PHONY: lint
lint: ## Vet and lint the code (golangci-lint includes gosec)
	$(GO) vet ./...
	golangci-lint run

.PHONY: vuln
vuln: ## Check dependencies for known vulnerabilities
	govulncheck ./...

.PHONY: check
check: lint test vuln ## Everything CI runs

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf $(BINARY_DIR) coverage.out

.PHONY: sync-skill
sync-skill: ## Copy the skill into the package that embeds it
	cp skills/lac/SKILL.md internal/skill/SKILL.md
