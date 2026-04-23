SHELL := /bin/bash

BINARY      := x-beacon
CMD_PATH    := ./cmd/gateway
BIN_DIR     := bin
COVERAGE    := coverage.txt

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
  -X main.version=$(VERSION) \
  -X main.commit=$(COMMIT) \
  -X main.buildTime=$(BUILD_TIME)

GO          ?= go
GOFLAGS     ?=
PKGS        := ./...

.DEFAULT_GOAL := help

.PHONY: help
help:
	@awk 'BEGIN{FS=":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the gateway binary into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD_PATH)

.PHONY: dev
dev: configs/config.yaml ## Run the gateway locally (reads configs/config.yaml)
	$(GO) run $(CMD_PATH) --config configs/config.yaml

.PHONY: run
run: dev ## Alias for `dev`

configs/config.yaml:
	@cp configs/config.example.yaml $@ && echo "created $@ from example — edit before real use"

.PHONY: test
test: ## Unit tests
	$(GO) test $(GOFLAGS) -count=1 $(PKGS)

.PHONY: test-race
test-race: ## Unit tests with -race detector
	$(GO) test $(GOFLAGS) -count=1 -race $(PKGS)

.PHONY: cover
cover: ## Test coverage report (coverage.txt + HTML)
	$(GO) test $(GOFLAGS) -count=1 -covermode=atomic -coverprofile=$(COVERAGE) $(PKGS)
	$(GO) tool cover -html=$(COVERAGE) -o coverage.html
	@echo "Coverage report: coverage.html"

.PHONY: bench
bench: ## Run benchmarks
	$(GO) test $(GOFLAGS) -run=^$$ -bench=. -benchmem $(PKGS)

.PHONY: fmt
fmt: ## gofmt + goimports
	gofmt -s -w .
	@if command -v goimports >/dev/null 2>&1; then goimports -w .; else echo "goimports not installed; run: go install golang.org/x/tools/cmd/goimports@latest"; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet $(PKGS)

.PHONY: lint
lint: ## golangci-lint
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; see https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	fi

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: check
check: fmt vet lint test-race ## Full pre-commit check

.PHONY: mocks
mocks: ## Regenerate mocks (stub until mockery is wired in)
	@echo "mocks: not yet configured (will be wired in Week 1 alongside provider interface)"

# --- Docker / local dependencies ----------------------------------------

.PHONY: docker-up
docker-up: ## Start postgres + redis via docker-compose
	docker-compose up -d postgres redis

.PHONY: docker-down
docker-down: ## Stop docker-compose services
	docker-compose down

.PHONY: docker-logs
docker-logs: ## Tail docker-compose logs
	docker-compose logs -f --tail=100

# --- Database migrations ------------------------------------------------

MIGRATE_DSN ?= postgres://xbeacon:xbeacon@localhost:5432/xbeacon?sslmode=disable

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations (requires golang-migrate)
	@if command -v migrate >/dev/null 2>&1; then \
		migrate -database "$(MIGRATE_DSN)" -path internal/storage/migrations up; \
	else \
		echo "migrate not installed; run: brew install golang-migrate"; \
		exit 1; \
	fi

.PHONY: migrate-down
migrate-down: ## Rollback last migration
	migrate -database "$(MIGRATE_DSN)" -path internal/storage/migrations down 1

# --- Cleanup ------------------------------------------------------------

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(COVERAGE) coverage.html
