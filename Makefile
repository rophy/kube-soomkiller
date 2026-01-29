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

.PHONY: help all build test lint clean image push \
	k3s-up k3s-down k3s-status k3s-kubeconfig k3s-ssh

.DEFAULT_GOAL := help

all: build ## Build all targets

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

## K3s Cluster (Multipass)

k3s-up: ## Create K3s cluster with swap enabled
	./scripts/setup-k3s-multipass.sh up

k3s-down: ## Delete K3s cluster
	./scripts/setup-k3s-multipass.sh down

k3s-status: ## Show K3s cluster status
	./scripts/setup-k3s-multipass.sh status

k3s-kubeconfig: ## Output K3s kubeconfig
	./scripts/setup-k3s-multipass.sh kubeconfig

k3s-ssh: ## SSH to K3s server (use K3S_NODE=name for other nodes)
	./scripts/setup-k3s-multipass.sh ssh $(K3S_NODE)

## Help

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
