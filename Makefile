.PHONY: build test clean install help docker-build docker-push build-backend proto-backend

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
	@echo "Running tests..."
	@if [ -n "$(PKG)" ]; then \
		if [ "$(PKG)" = "cache" ]; then \
			echo "Running cache tests sequentially (143 tests with shared miniredis)..."; \
			go test $(if $(VERBOSE),-v) -tags test -p 1 -parallel 1 ./$(PKG)/...; \
		else \
			go test $(if $(VERBOSE),-v) -tags test -p 4 -parallel 4 ./$(PKG)/...; \
		fi; \
	else \
		go test $(if $(VERBOSE),-v) -tags test -p 4 -parallel 4 ./...; \
	fi

test-coverage: ## Run tests with coverage (use PKG=package for specific package)
	@echo "Running tests with coverage..."
ifdef PKG
	@go test -v -tags test -p 4 -parallel 4 -coverprofile=coverage.out ./$(PKG)/...
else
	@go test -v -tags test -p 4 -parallel 4 -coverprofile=coverage.out ./...
endif
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

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

lint: ## Run golangci-lint
	@echo "Running linters..."
	@golangci-lint run

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

.DEFAULT_GOAL := help
