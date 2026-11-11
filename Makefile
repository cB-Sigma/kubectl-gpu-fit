BINARY   := kubectl-gpu-fit
PKG      := github.com/cb-Sigma/kubectl-gpu-fit
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(PKG)/internal/cmd.Version=$(VERSION) \
	-X $(PKG)/internal/cmd.Commit=$(COMMIT) \
	-X $(PKG)/internal/cmd.Date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build a static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) .

.PHONY: install
install: ## Install the binary into GOBIN so that `kubectl gpu-fit` works
	CGO_ENABLED=0 go install -trimpath -ldflags '$(LDFLAGS)' .

.PHONY: test
test: ## Run the tests with the race detector
	go test ./... -race

.PHONY: cover
cover: ## Run the tests and print coverage per package
	go test ./... -cover

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (v2)
	golangci-lint run

.PHONY: fmt
fmt: ## Format the code
	golangci-lint fmt

.PHONY: check
check: vet lint test ## Everything CI runs

.PHONY: demo
demo: ## Print the demo report
	go run . --demo

.PHONY: readme
readme: ## Refresh the command output embedded in README.md
	go test ./internal/cmd -run TestReadmeOutput -update

.PHONY: fake-cluster
fake-cluster: ## Serve a fake API server + Prometheus and write fake-cluster.kubeconfig
	go run ./hack/fake-cluster -kubeconfig fake-cluster.kubeconfig

.PHONY: snapshot
snapshot: ## Build release archives locally without publishing (needs goreleaser)
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build outputs
	rm -rf bin dist coverage.out fake-cluster.kubeconfig
