.PHONY: build test clean install help docker-build docker-push build-backend proto-backend \
	tilt-up-k8s tilt-down-k8s

# Binary name
BINARY_NAME=pocket-relay-miner

# Build directory
BUILD_DIR=bin

# Backend server directory
BACKEND_DIR=tilt/backend-server

# Docker image configuration
DOCKER_IMAGE?=ghcr.io/pokt-network/pocket-relay-miner:rc

# Version information
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT?=$(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

# Go build flags with version injection
LDFLAGS=-ldflags "\
	-s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.Commit=$(COMMIT)' \
	-X 'main.BuildDate=$(BUILD_DATE)'"

help: ## Display this help message
	@echo "Pocket RelayMiner Makefile"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

build: ## Build the pocket-relay-miner binary
	@echo "Building $(BINARY_NAME)..."
	@go build $(LDFLAGS) -o ./$(BUILD_DIR)/$(BINARY_NAME) .
	@echo "Build complete: ./$(BUILD_DIR)/$(BINARY_NAME)"

build-release: ## Build optimized release binary
	@echo "Building release binary..."
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=0 go build $(LDFLAGS) -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) .
	@echo "Release build complete: $(BUILD_DIR)/$(BINARY_NAME)"

install: ## Install the binary to $GOPATH/bin
	@echo "Installing $(BINARY_NAME) to $$GOPATH/bin..."
	@go install $(LDFLAGS) .
	@echo "Install complete"

test: ## Run tests (PKG=package_name for specific package, VERBOSE=1 for verbose output)
	@./scripts/gates/tests.sh

test_miner: ## Run miner tests exclusively with race detection (Rule #1: no flakes, no races, no mocks)
	@PKG=miner ./scripts/gates/race.sh

race: ## Run the whole tree under the race detector (Rule #1; PKG=package to narrow)
	@./scripts/gates/race.sh

gate: ## Run the quality gates (LEVEL=1 static, 2 +tests/race/coverage, 3 +live)
	@./scripts/gates/all.sh --level $(or $(LEVEL),2)

test-coverage: ## Run tests with coverage (use PKG=package for specific package)
	@COVERAGE_HTML=1 ./scripts/gates/coverage.sh

clean: ## Clean build artifacts
	@echo "Cleaning build artifacts..."
	@rm -f $(BINARY_NAME)
	@rm -rf $(BUILD_DIR)
	@rm -f coverage.out coverage.html
	@rm -f $(BACKEND_DIR)/backend
	@rm -f $(BACKEND_DIR)/pb/*.pb.go
	@echo "Clean complete"

tidy: ## Run go mod tidy
	@echo "Running go mod tidy..."
	@go mod tidy

fmt: ## Format code
	@echo "Formatting code..."
	@go fmt ./...
	@cd $(BACKEND_DIR) && go fmt ./...

lint: ## Run golangci-lint
	@echo "Running linters..."
	@golangci-lint run
	@cd $(BACKEND_DIR) && golangci-lint run

check-tracked-files: ## Verify no local-only files (planning docs, IDE config, secrets) are tracked
	@./scripts/check-tracked-files.sh

install-hooks: ## Install the git pre-commit hook (gofmt, build, vet, lint checks)
	@echo "Installing git hooks..."
	@chmod +x scripts/pre-commit-hook.sh
	@ln -sf ../../scripts/pre-commit-hook.sh .git/hooks/pre-commit
	@echo "Pre-commit hook installed."
	@echo "Before each commit it checks: gofmt, go build, go vet, tracked files, golangci-lint."
	@echo "It reports problems rather than fixing them; bypass once with 'git commit --no-verify'."

docker-build: ## Build Docker image (override with DOCKER_IMAGE env var)
	@echo "Building Docker image: $(DOCKER_IMAGE)..."
	@docker build -t $(DOCKER_IMAGE) .
	@echo "Docker build complete: $(DOCKER_IMAGE)"

docker-push: ## Push Docker image to registry
	@echo "Pushing Docker image: $(DOCKER_IMAGE)..."
	@docker push $(DOCKER_IMAGE)
	@echo "Docker push complete: $(DOCKER_IMAGE)"

proto-backend: ## Generate protobuf code for backend server
	@echo "Generating protobuf code for backend server..."
	@cd $(BACKEND_DIR) && protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative pb/demo.proto
	@echo "Protobuf generation complete"

build-backend: proto-backend ## Build the backend test server
	@echo "Building backend test server..."
	@cd $(BACKEND_DIR) && go mod tidy && go build -o backend main.go
	@echo "Backend build complete: $(BACKEND_DIR)/backend"

# =============================================================================
# Tilt Development Environments
# =============================================================================
# Pass additional tilt args via ARGS, e.g.: make tilt-up-k8s ARGS="--stream"



tilt-up-k8s: ## Start Kubernetes dev environment with Tilt
	@echo "Starting Kubernetes Tilt environment..."
	tilt up -f Tiltfile $(ARGS)

tilt-down-k8s: ## Stop Kubernetes dev environment
	@echo "Stopping Kubernetes Tilt environment..."
	tilt down -f Tiltfile $(ARGS)

.DEFAULT_GOAL := help
