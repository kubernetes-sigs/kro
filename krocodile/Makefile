# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Resolve GOBIN. Defaults to $(go env GOPATH)/bin when unset.
ifeq (,$(shell go env GOBIN))
GOBIN = $(shell go env GOPATH)/bin
else
GOBIN = $(shell go env GOBIN)
endif

# Where to install tool binaries that this Makefile manages itself.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# Project metadata used by builds.
GIT_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_HASH    ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS = -buildid= \
	-X main.version=$(GIT_VERSION) \
	-X main.commit=$(GIT_HASH) \
	-X main.date=$(BUILD_DATE)

# Tooling pinned versions.
CONTROLLER_GEN_VERSION ?= v0.20.0
GOLANGCI_LINT_VERSION  ?= v2.11.4

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT  ?= $(LOCALBIN)/golangci-lint

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Print this help message.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: fmt
fmt: ## Run go fmt on all packages.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet on all packages.
	go vet ./...

.PHONY: generate
generate: controller-gen ## Regenerate DeepCopy methods (zz_generated.deepcopy.go).
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD YAML into config/crd/.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint and apply fixes.
	$(GOLANGCI_LINT) run --fix ./...

.PHONY: test
test: ## Run unit tests with race detection and coverage profile.
	go test -race -coverprofile=cover.out ./pkg/... ./cmd/...

# Integration tests need an envtest control plane. The default below
# matches the kubebuilder-installed assets on macOS arm64; override with
# KUBEBUILDER_ASSETS=<path> if you installed them elsewhere.
KUBEBUILDER_ASSETS ?= $(HOME)/Library/Application Support/io.kubebuilder.envtest/k8s/1.31.0-darwin-arm64

.PHONY: test-integration
test-integration: ## Run envtest-backed integration suites.
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" go test -count=1 -timeout=300s ./test/integration/...

.PHONY: test-coverage
test-coverage: test ## Run tests and print coverage summary.
	@echo ""
	@echo "=== Coverage summary ==="
	@go tool cover -func=cover.out | tail -1
	@echo ""
	@echo "Run 'make coverage-html' to view the HTML report."

.PHONY: coverage-html
coverage-html: ## Open the coverage HTML report (requires cover.out).
	@test -f cover.out || (echo "cover.out not found — run 'make test' first" && exit 1)
	go tool cover -html=cover.out

.PHONY: verify
verify: fmt vet lint test ## Run fmt + vet + lint + test in one shot.

##@ Build

.PHONY: build
build: generate manifests fmt vet ## Build the controller binary into ./bin/controller.
	go build -ldflags="$(LDFLAGS)" -o $(LOCALBIN)/controller ./cmd/controller

.PHONY: run
run: ## Run the controller locally (uses $KUBECONFIG).
	go run ./cmd/controller

.PHONY: clean
clean: ## Remove generated build artifacts (bin/ and cover.out).
	rm -rf $(LOCALBIN) cover.out

##@ Tooling

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen into ./bin if missing or out of date.
$(CONTROLLER_GEN): $(LOCALBIN)
	@if test -x $(CONTROLLER_GEN) && ! $(CONTROLLER_GEN) --version 2>/dev/null | grep -q $(CONTROLLER_GEN_VERSION); then \
		echo "controller-gen version mismatch, reinstalling $(CONTROLLER_GEN_VERSION)"; \
		rm -f $(CONTROLLER_GEN); \
	fi
	test -x $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Install golangci-lint into ./bin if missing or out of date.
$(GOLANGCI_LINT): $(LOCALBIN)
	@if test -x $(GOLANGCI_LINT) && ! $(GOLANGCI_LINT) --version 2>/dev/null | grep -q $(GOLANGCI_LINT_VERSION); then \
		echo "golangci-lint version mismatch, reinstalling $(GOLANGCI_LINT_VERSION)"; \
		rm -f $(GOLANGCI_LINT); \
	fi
	test -x $(GOLANGCI_LINT) || \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
		| sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION)
