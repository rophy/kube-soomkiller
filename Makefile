# kube-soomkiller Makefile

# Container runtime (docker or podman)
CONTAINER_RUNTIME ?= $(shell command -v podman 2>/dev/null || echo docker)
COMPOSE ?= $(shell command -v podman-compose 2>/dev/null || echo docker-compose)

# Image settings
IMAGE_NAME ?= kube-soomkiller
IMAGE_TAG ?= latest
IMAGE ?= $(IMAGE_NAME):$(IMAGE_TAG)

# Build settings
BINARY_NAME = kube-soomkiller
GO = go

.PHONY: all build test lint clean image push help

all: build

## Build

build: ## Build binary locally
	$(GO) build -o bin/$(BINARY_NAME) ./cmd/kube-soomkiller

build-linux: ## Build binary for Linux
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags="-s -w" -o bin/$(BINARY_NAME)-linux-amd64 ./cmd/kube-soomkiller

## Test

test: ## Run tests
	$(GO) test -v ./...

test-coverage: ## Run tests with coverage
	$(GO) test -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

lint: ## Run linter
	golangci-lint run ./...

## Container

image: ## Build container image
	./scripts/build-image.sh

push: image ## Push container image
	$(CONTAINER_RUNTIME) push $(IMAGE)

## Docker Compose

compose-build: ## Build using docker-compose
	$(COMPOSE) build

compose-test: ## Run tests using docker-compose
	$(COMPOSE) run --rm test

compose-lint: ## Run linter using docker-compose
	$(COMPOSE) run --rm lint

compose-dev: ## Run in dev mode with docker-compose
	$(COMPOSE) run --rm dev

## Development

deps: ## Download dependencies
	$(GO) mod download

tidy: ## Tidy go.mod
	$(GO) mod tidy

fmt: ## Format code
	$(GO) fmt ./...

vet: ## Run go vet
	$(GO) vet ./...

## Clean

clean: ## Clean build artifacts
	rm -rf bin/
	rm -f coverage.out coverage.html

## Help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
