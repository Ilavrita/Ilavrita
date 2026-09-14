BINARY := bin/ilavrita
PACKAGE := ./apps/ilavrita
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION ?= $(shell git rev-parse --verify --quiet HEAD || echo unknown)
LDFLAGS := -X main.version=$(VERSION) -X main.revision=$(REVISION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List available targets
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sort | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: ## Fetch the pinned PocketBase fork and install workspace dependencies
	./scripts/bootstrap.sh

.PHONY: build
build: ## Compile the Ilavrita server
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) $(PACKAGE)

.PHONY: run
run: ## Run the Ilavrita server from source
	go run $(PACKAGE) serve

.PHONY: test
test: ## Run the Go test suite
	go test ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format Go sources
	go fmt ./...

.PHONY: tidy
tidy: ## Reconcile go.mod and go.sum
	go mod tidy

.PHONY: docker
docker: ## Build the container image
	docker build -f build/docker/Dockerfile -t ilavrita/ilavrita:$(VERSION) .

.PHONY: clean
clean: ## Remove build output
	$(RM) -r bin dist
