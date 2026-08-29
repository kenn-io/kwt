# Variables
BINARY_NAME := kwt
PACKAGE := go.kenn.io/kwt
VERSION = $(shell git describe --tags --always --dirty)
LDFLAGS = -s -w -X $(PACKAGE)/internal/cmd.version=$(VERSION)
GO_FILES := $(shell find . -name '*.go' -type f -not -path './vendor/*')

# Build variables
BUILD_DIR := build
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
ifeq ($(GOOS),windows)
GO_PATH_FIRST := $(shell go env GOPATH | sed 's/;.*//')
else
GO_PATH_FIRST := $(shell go env GOPATH | sed 's/:.*//')
endif
INSTALL_DIR ?= $(GO_PATH_FIRST)/bin
VERCEL_SCOPE ?= kenn-software
VERCEL_PROJECT ?= kwt-docs
TEST_PACKAGES ?= ./...
TEST_CLEAN_ENV := env -i \
	APPDATA="$(APPDATA)" \
	CGO_ENABLED="$(CGO_ENABLED)" \
	COMSPEC="$(COMSPEC)" \
	GOCACHE="$(GOCACHE)" \
	GOARCH="$(GOARCH)" \
	GOMODCACHE="$(GOMODCACHE)" \
	GOPATH="$(GOPATH)" \
	GOOS="$(GOOS)" \
	GOROOT="$(GOROOT)" \
	HOME="$(HOME)" \
	HOMEDRIVE="$(HOMEDRIVE)" \
	HOMEPATH="$(HOMEPATH)" \
	LANG="$(LANG)" \
	LC_ALL="$(LC_ALL)" \
	LC_CTYPE="$(LC_CTYPE)" \
	LOCALAPPDATA="$(LOCALAPPDATA)" \
	NIX_SSL_CERT_FILE="$(NIX_SSL_CERT_FILE)" \
	PATH="$(PATH)" \
	PATHEXT="$(PATHEXT)" \
	PROGRAMDATA="$(PROGRAMDATA)" \
	SSL_CERT_DIR="$(SSL_CERT_DIR)" \
	SSL_CERT_FILE="$(SSL_CERT_FILE)" \
	SYSTEMROOT="$(SYSTEMROOT)" \
	TEMP="$(TEMP)" \
	TMP="$(TMP)" \
	TMPDIR="$(TMPDIR)" \
	USERPROFILE="$(USERPROFILE)" \
	WINDIR="$(WINDIR)" \
	XDG_CACHE_HOME="$(XDG_CACHE_HOME)" \
	GOAUTH=off \
	GOENV=off \
	GOPROXY=https://proxy.golang.org \
	GOSUMDB=sum.golang.org \
	GOTOOLCHAIN=auto \
	GOVCS="*:off" \
	GIT_CONFIG_GLOBAL=/dev/null \
	GIT_CONFIG_NOSYSTEM=1 \
	GIT_TERMINAL_PROMPT=0
GO_TEST_BOOTSTRAP := $(TEST_CLEAN_ENV) go -C tools/testbootstrap test ./...
GO_TEST_RUNNER := $(TEST_CLEAN_ENV) KWT_HOME="$(KWT_HOME)" go -C tools/testbootstrap run . --

.PHONY: all build clean test test-verbose test-coverage lint fmt vet install help docs-install docs-assets docs-assets-test docs-build docs-serve docs-check docs-deploy

# Default target
all: clean build

## build: Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	@go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) cmd/kwt/main.go

## build-all: Build for multiple platforms
build-all: clean
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	# macOS AMD64
	@GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 cmd/kwt/main.go
	# macOS ARM64 (Apple Silicon)
	@GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 cmd/kwt/main.go
	# Linux AMD64
	@GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 cmd/kwt/main.go
	# Linux ARM64
	@GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 cmd/kwt/main.go
	# Windows AMD64
	@GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe cmd/kwt/main.go
	# Windows ARM64
	@GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-arm64.exe cmd/kwt/main.go
	@echo "Build complete. Binaries are in $(BUILD_DIR)/"

## clean: Clean build files
clean:
	@echo "Cleaning..."
	@rm -f $(BINARY_NAME)
	@rm -rf $(BUILD_DIR)
	@rm -f coverage.out coverage.html

## test: Run tests
test:
	@echo "Running tests..."
	@$(GO_TEST_BOOTSTRAP)
	@$(GO_TEST_RUNNER) $(TEST_PACKAGES)

## test-verbose: Run tests with verbose output
test-verbose:
	@echo "Running tests (verbose)..."
	@$(GO_TEST_RUNNER) -v $(TEST_PACKAGES)

## test-coverage: Run tests with coverage
test-coverage:
	@echo "Running tests with coverage..."
	@$(GO_TEST_RUNNER) -cover $(TEST_PACKAGES)

## test-coverage-report: Generate and open coverage report
test-coverage-report:
	@echo "Generating coverage report..."
	@$(GO_TEST_RUNNER) -coverprofile=coverage.out $(TEST_PACKAGES)
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

## bench: Run benchmarks
bench:
	@echo "Running benchmarks..."
	@$(GO_TEST_RUNNER) -bench=. -benchmem $(TEST_PACKAGES)

## docs-install: Install docs toolchain
docs-install:
	@cd docs && uv sync --frozen --no-dev

## docs-assets: Materialize website binaries from the orphan asset branch
docs-assets:
	@bash docs/scripts/sync-assets.sh

## docs-assets-test: Verify orphan asset hydration behavior
docs-assets-test:
	@bash docs/scripts/test-sync-assets.sh

## docs-build: Build Zensical docs
docs-build: docs-assets
	@cd docs && uv run --frozen bash ./zensical-docs.sh build

## docs-serve: Serve Zensical docs locally
docs-serve: docs-assets
	@cd docs && uv run bash ./zensical-docs.sh serve

## docs-check: Verify docs build
docs-check: docs-assets-test docs-build

## docs-deploy: Build docs and deploy to Vercel
docs-deploy: docs-build
	@vercel deploy --prod --yes --cwd docs/site --scope "$(VERCEL_SCOPE)" --project "$(VERCEL_PROJECT)"

## lint: Run golangci-lint
lint:
	@echo "Running golangci-lint..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed. Install with: brew install golangci-lint"; \
	fi

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint fmt ./...; \
	else \
		echo "golangci-lint not installed. Install with: brew install golangci-lint"; \
	fi

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

## mod: Tidy and verify go modules
mod:
	@echo "Tidying modules..."
	@go mod tidy
	@echo "Verifying modules..."
	@go mod verify

## install: Install the binary
install:
	@echo "Installing $(BINARY_NAME) to $(INSTALL_DIR)..."
	@mkdir -p "$(INSTALL_DIR)"
	@GOBIN="$(INSTALL_DIR)" go install -ldflags "$(LDFLAGS)" ./cmd/kwt

## check: Run all checks (fmt, vet, lint, test)
check: fmt vet lint test

## deps: Download dependencies
deps:
	@echo "Downloading dependencies..."
	@go mod download

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
