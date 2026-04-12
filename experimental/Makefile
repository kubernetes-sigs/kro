# Root of the repo (where go.mod lives). All go commands run from there.
ROOT := $(shell git rev-parse --show-toplevel)

LOCALBIN         := $(ROOT)/bin
KO               := $(LOCALBIN)/ko
ENVTEST          := $(LOCALBIN)/setup-envtest
ENVTEST_VERSION  ?= 1.35.x

TEST_FLAGS := -race -count=1
TEST_PKGS  := ./experimental/...

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: presubmit

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Presubmit

.PHONY: presubmit
presubmit: vet envtest ## Run presubmit checks (vet, tests, mod tidy)
	cd $(ROOT) && KUBEBUILDER_ASSETS="$${KUBEBUILDER_ASSETS:-$$($(ENVTEST) use $(ENVTEST_VERSION) --bin-dir $(LOCALBIN) -p path)}" go test $(TEST_FLAGS) $(TEST_PKGS)
	cd $(ROOT) && go mod tidy && git diff --exit-code go.mod go.sum

##@ Development

.PHONY: envtest
envtest: ## Download setup-envtest if necessary.
	test -s $(ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: vet
vet: ## Run go vet against experimental code.
	cd $(ROOT) && go vet ./experimental/...

.PHONY: run
run: ## Run the graph controller locally (bootstraps CRDs automatically).
	cd $(ROOT) && go run ./experimental/cmd/ --bootstrap

.PHONY: apply
apply: ## Build and deploy the graph controller to the current cluster.
	$(KO) apply -f $(ROOT)/experimental/deploy/

.PHONY: test
test: envtest ## Run tests with verbose output.
	cd $(ROOT) && KUBEBUILDER_ASSETS="$${KUBEBUILDER_ASSETS:-$$($(ENVTEST) use $(ENVTEST_VERSION) --bin-dir $(LOCALBIN) -p path)}" go test $(TEST_FLAGS) $(TEST_PKGS) -v

.PHONY: bench
bench: ## Run benchmarks.
	cd $(ROOT) && go test ./experimental/controller -bench=. -benchmem -count=1 -run='^$$'
