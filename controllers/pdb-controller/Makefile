# Variables — align with the other controllers' contract.
DOCKER_USER ?= castai
DOCKER_IMAGE := $(DOCKER_USER)/castai-pdb-controller
VERSION ?= latest
PLATFORM ?=

# Variables — align with Helm default (Artifact Registry castai-hub)
DOCKER_IMAGE_NAME = us-docker.pkg.dev/castai-hub/library/castai-pdb
DOCKER_IMAGE_TAGS = latest 0.5

# Buildx multi-arch defaults (from original Makefile).
PLATFORMS = linux/amd64,linux/arm64

# Enable BuildKit
export DOCKER_BUILDKIT = 1

.PHONY: all build push docker-build docker-push docker-build-push clean help

# Default target — preserves the original Artifact Registry buildx flow.
all: build push

# Multi-arch build via buildx (original Artifact Registry flow).
build:
	@if ! docker buildx inspect multiarch-builder &>/dev/null; then \
		docker buildx create --use --name multiarch-builder; \
	fi
	docker buildx inspect multiarch-builder --bootstrap
	docker buildx build \
		--platform $(PLATFORMS) \
		$(foreach tag,$(DOCKER_IMAGE_TAGS),-t $(DOCKER_IMAGE_NAME):$(tag)) \
		--push \
		-f Dockerfile ../..

# Push target is informational (build uses --push to Artifact Registry; authenticate with gcloud/docker first).
push:
	@$(foreach tag,$(DOCKER_IMAGE_TAGS),echo "Image: $(DOCKER_IMAGE_NAME):$(tag)";)

# Build Docker image (context is the repo root so ../../apis, ../../clientset,
# ../../snapshot replace directives resolve).
docker-build:
	docker build $(if $(PLATFORM),--platform $(PLATFORM),) -f Dockerfile -t $(DOCKER_IMAGE):$(VERSION) ../..

# Push Docker image
docker-push: docker-build
	docker push $(DOCKER_IMAGE):$(VERSION)

# Build and push Docker image
docker-build-push: docker-build docker-push

# Clean up the buildx builder (kept from the original Makefile).
clean:
	-docker buildx rm multiarch-builder || true

help:
	@echo "Available targets:"
	@echo "  all               - Multi-arch buildx flow (default)"
	@echo "  build             - Multi-arch buildx (Artifact Registry)"
	@echo "  push              - Informational list of pushed tags"
	@echo "  docker-build      - Single-arch build (context: repo root)"
	@echo "  docker-push       - Push single-arch image"
	@echo "  docker-build-push - Build and push single-arch image"
	@echo "  clean             - Remove the buildx builder"
	@echo ""
	@echo "Variables (override via make VAR=value):"
	@echo "  DOCKER_USER=$(DOCKER_USER)"
	@echo "  VERSION=$(VERSION)"
	@echo "  PLATFORM=$(PLATFORM)  (set e.g. linux/amd64 to force --platform)"
