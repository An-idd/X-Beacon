SHELL := /bin/bash

BINARY      := x-beacon
CTL_BINARY  := xbctl
CMD_PATH    := ./cmd/gateway
CTL_PATH    := ./cmd/xbctl
BIN_DIR     := bin
COVERAGE    := coverage.txt

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG := github.com/An-idd/x-beacon/pkg/version
LDFLAGS     := -s -w \
  -X $(VERSION_PKG).Version=$(VERSION) \
  -X $(VERSION_PKG).Commit=$(COMMIT) \
  -X $(VERSION_PKG).BuildTime=$(BUILD_TIME)

GO          ?= go
GOFLAGS     ?=
PKGS        := ./...

.DEFAULT_GOAL := help

.PHONY: help
help:
	@awk 'BEGIN{FS=":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: build-gateway build-ctl ## Build both the gateway and xbctl binaries

.PHONY: build-gateway
build-gateway: ## Build the gateway binary into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD_PATH)

.PHONY: build-ctl
build-ctl: ## Build the xbctl ops CLI into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(CTL_BINARY) $(CTL_PATH)

.PHONY: dev
dev: configs/config.yaml ## Run the gateway locally (reads configs/config.yaml)
	$(GO) run $(CMD_PATH) --config configs/config.yaml

.PHONY: run
run: dev ## Alias for `dev`

configs/config.yaml:
	@cp configs/config.example.yaml $@ && echo "created $@ from example — edit before real use"

configs/providers.yaml:
	@cp configs/providers.example.yaml $@ && echo "created $@ from example — set OPENAI_API_KEY/ANTHROPIC_API_KEY/DEEPSEEK_API_KEY before launch"

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

# --- Phase 5 compat suites ----------------------------------------------

# Wire (L1) compat already runs in `make test` (it's a normal Go test).
# This target is for the Python (L2) SDK suite, which needs:
#   - mockupstream listening on 127.0.0.1:19091
#   - gateway listening on 127.0.0.1:18080 wired to the mock
#   - uv-managed Python venv with pinned openai SDK
# We orchestrate all three from one Makefile target so a developer
# can validate the suite with one command.

.PHONY: compat-wire
compat-wire: ## Run the Go L1 wire-compat suite (subset of `make test`)
	go test -v ./test/ -run TestWireCompat

.PHONY: compat-python
compat-python: build ## Run the OpenAI Python SDK compat suite (needs `uv`)
	@command -v uv >/dev/null 2>&1 || { echo "uv not installed; see https://docs.astral.sh/uv/"; exit 1; }
	@# Defensive: kill any leftover compat processes from a prior failed run.
	@# `lsof -ti` returns PIDs only; xargs -r is a GNU extension so we
	@# pipe through `awk` for portability.
	@for p in 19091 18080; do \
		pids=$$(lsof -ti tcp:$$p 2>/dev/null) ; \
		[ -n "$$pids" ] && kill -9 $$pids 2>/dev/null || true ; \
	done
	@echo ">> starting mockupstream on :19091"
	@MOCK_ADDR=127.0.0.1:19091 go run ./scripts/mockupstream > .compat-mock.log 2>&1 & echo $$! > .compat-mock.pid
	@sleep 0.5
	@echo ">> starting gateway on :18080 against compat profile"
	@$(BIN_DIR)/$(BINARY) --config configs/config.compat.yaml > .compat-gateway.log 2>&1 & echo $$! > .compat-gateway.pid
	@sleep 0.8
	@echo ">> running pytest under uv"
	@cd test/compat/python && \
		XBEACON_BASE_URL=http://127.0.0.1:18080 \
		XBEACON_API_KEY=sk-compat-test \
		uv run --frozen pytest -v ; \
		status=$$? ; \
		cd ../../.. ; \
		kill -9 $$(cat .compat-mock.pid) 2>/dev/null ; rm -f .compat-mock.pid ; \
		kill -9 $$(cat .compat-gateway.pid) 2>/dev/null ; rm -f .compat-gateway.pid ; \
		if [ $$status -eq 0 ]; then \
			rm -f .compat-mock.log .compat-gateway.log ; \
		else \
			echo ">> retained .compat-{mock,gateway}.log for inspection (test failed)" ; \
		fi ; \
		exit $$status

.PHONY: compat
compat: compat-wire compat-python ## Run both L1 wire + L2 Python SDK compat suites

# --- Cleanup ------------------------------------------------------------

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(COVERAGE) coverage.html
