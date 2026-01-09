SHELL := /bin/bash
GO ?= go
BIN_DIR ?= bin

SERVER_PKG := ./cmd/server
SERVER_BIN := $(BIN_DIR)/server

.PHONY: help server build test fmt vet tidy clean

help:
	@echo "Targets:"
	@echo "  make server   - Run the API server (go run)"
	@echo "  make build    - Build server binary to $(SERVER_BIN)"
	@echo "  make test     - Run unit tests"
	@echo "  make fmt      - Format code with go fmt"
	@echo "  make vet      - Static analysis with go vet"
	@echo "  make tidy     - Sync go.mod/go.sum"
	@echo "  make clean    - Remove build artifacts"
	@echo "  make smoke1   - Run smoke test script (requires server running)"
	@echo "  make smoke2   - Run barge-in smoke test (requires server running)"
	@echo "  make smoke2_real - Run real-VAD barge-in smoke test (requires VAD)"

server:
	$(GO) run $(SERVER_PKG)

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(SERVER_BIN) $(SERVER_PKG)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)

.PHONY: smoke1
smoke1:
	bash scripts/smoke_part1.sh

.PHONY: smoke2
smoke2:
	bash scripts/smoke_part2.sh

.PHONY: smoke2_real
smoke2_real:
	bash scripts/smoke_part2_real.sh
