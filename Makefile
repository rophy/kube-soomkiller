# kube-soomkiller Makefile

.PHONY: help test-unit test-e2e image k3s-up k3s-down

.DEFAULT_GOAL := help

## Test

test-unit: ## Run linter and unit tests
	golangci-lint run ./...
	go test -v ./...

test-e2e: ## Run e2e tests (requires K3s cluster)
	bats test/e2e/

## Container

image: ## Build container image
	skaffold build

## K3s Cluster (Multipass)

k3s-up: ## Create K3s cluster with swap enabled
	./scripts/setup-k3s-multipass.sh up

k3s-down: ## Delete K3s cluster
	./scripts/setup-k3s-multipass.sh down

## Help

help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
