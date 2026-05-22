# Variables
BINARY_NAME := tsc-controller
DOCKER_IMAGE := castai/tsc-controller
VERSION ?= latest
GO := go
GOFLAGS := -v
LDFLAGS := -w -s

# Go module settings
MODULE := github.com/castai/sales-engineering-snaphot-analyzer/workload-autopilot/controllers/tsc-controller

.PHONY: all build test clean docker-build docker-push lint fmt vet help

# Default target
all: build

# Build the binary
build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/

# Run tests
test:
	$(GO) test -v -race -cover ./...

# Clean build artifacts
clean:
	rm -f $(BINARY_NAME)
	rm -rf dist/

# Build Docker image
docker-build:
	docker build -t $(DOCKER_IMAGE):$(VERSION) .

# Push Docker image
docker-push: docker-build
	docker push $(DOCKER_IMAGE):$(VERSION)

# Run linter
lint:
	golangci-lint run ./...

# Format code
fmt:
	$(GO) fmt ./...

# Run go vet
vet:
	$(GO) vet ./...

# Check formatting
fmt-check:
	$(GO)fmt -l .

# Verify build
verify: fmt-check vet lint

# Create Kubernetes manifests manifests:
manifests:
	@mkdir -p manifests
	@echo "Creating Kubernetes manifests..."

# Development build
dev: build
	./$(BINARY_NAME) --kubeconfig=$(KUBECONFIG)

# Help
help:
	@echo "Available targets:"
	@echo "  build        - Build the binary"
	@echo "  test         - Run tests"
	@echo "  clean        - Clean build artifacts"
	@echo "  docker-build - Build Docker image"
	@echo "  docker-push  - Push Docker image"
	@echo "  lint         - Run linter"
	@echo "  fmt          - Format code"
	@echo "  vet          - Run go vet"
	@echo "  verify       - Run all checks"
	@echo "  dev          - Development build"
