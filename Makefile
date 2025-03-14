export GIT_COMMIT ?= $(shell git rev-parse --short HEAD)
export BUILD_DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
export RELEASE_VERSION ?= dev-$(GIT_COMMIT)
OCI_REPO ?= ghcr.io/kro-run/kro

HELM_IMAGE ?= ${OCI_REPO}
KO_DOCKER_REPO ?= ${OCI_REPO}/kro

KOCACHE ?= ~/.ko
KO_PUSH ?= true
export KIND_CLUSTER_NAME ?= kro

LDFLAGS ?= -X github.com/kro-run/kro/pkg.Version=$(RELEASE_VERSION) \
           -X github.com/kro-run/kro/pkg.GitCommit=$(GIT_COMMIT) \
           -X github.com/kro-run/kro/pkg.BuildDate=$(BUILD_DATE)

WITH_GOFLAGS = GOFLAGS="$(GOFLAGS)"

HELM_DIR = ./helm
WHAT ?= unit

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. finch)
CONTAINER_TOOL ?= finch

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: test
test: generate fmt vet ## Run tests. Use WHAT=unit or WHAT=integration
ifeq ($(WHAT),integration)
	go test -v ./test/integration/suites/... -coverprofile integration-cover.out
else ifeq ($(WHAT),unit)
	go test -v ./pkg/... -coverprofile unit-cover.out
else
	@echo "Error: WHAT must be either 'unit' or 'integration'"
	@echo "Usage: make test WHAT=unit|integration"
	@exit 1
endif

##@ Build

.PHONY: generate
generate: ## Run go generate
	go generate ./...

.PHONY: fmt
fmt: ## Run go fmt
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: build
build: generate fmt vet ## Build controller binary.
	go build -ldflags=${LDFLAGS} -o bin/controller ./cmd/controller/main.go

.PHONY: run
run: generate fmt vet ## Run a controller from your host.
	go run ./cmd/controller/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: image-build
image-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: image-push
image-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64
.PHONY: docker-buildx
image-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name project-v3-builder
	$(CONTAINER_TOOL) buildx use project-v3-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm project-v3-builder
	rm Dockerfile.cross

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
KO ?= $(LOCALBIN)/ko

## Tool Versions
KO_VERSION ?= v0.17.1
KUSTOMIZE_VERSION ?= v5.2.1

.PHONY: ko
ko: $(KO) ## Download ko locally if necessary. If wrong version is installed, it will be removed before downloading.
$(KO): $(LOCALBIN)
	@if test -x $(LOCALBIN)/ko && ! $(LOCALBIN)/ko version | grep -q $(KO_VERSION); then \
		echo "$(LOCALBIN)/ko version is not expected $(KO_VERSION). Removing it before installing."; \
		rm -rf $(LOCALBIN)/ko; \
	fi
	test -s $(LOCALBIN)/ko || GOBIN=$(LOCALBIN) GO111MODULE=on go install github.com/google/ko@$(KO_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary. If wrong version is installed, it will be removed before downloading.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q $(KUSTOMIZE_VERSION); then \
		echo "$(LOCALBIN)/kustomize version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -rf $(LOCALBIN)/kustomize; \
	fi
	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) GO111MODULE=on go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: image
build-image: ko ## Build the kro controller images using ko build
	echo "Building kro image $(RELEASE_VERSION).."
	$(WITH_GOFLAGS) KOCACHE=$(KOCACHE) KO_DOCKER_REPO=$(KO_DOCKER_REPO) \
		GIT_COMMIT=$(GIT_COMMIT) RELEASE_VERSION=$(RELEASE_VERSION) BUILD_DATE=$(BUILD_DATE) \
		$(KO) build --bare github.com/kro-run/kro/cmd/controller \
		--local\
		--push=false --tags ${RELEASE_VERSION} --sbom=none

.PHONY: publish
publish-image: ko ## Publish the kro controller images to ghcr.io
	$(WITH_GOFLAGS) KOCACHE=$(KOCACHE) KO_DOCKER_REPO=$(KO_DOCKER_REPO) \
		GIT_COMMIT=$(GIT_COMMIT) RELEASE_VERSION=$(RELEASE_VERSION) BUILD_DATE=$(BUILD_DATE) \
		$(KO) publish --bare github.com/kro-run/kro/cmd/controller \
		--tags ${RELEASE_VERSION} --sbom=none

.PHONY: package-helm
package-helm: ## Package Helm chart
	cp ./config/crd/bases/* helm/crds/
	@sed -i '' 's/tag: .*/tag: "$(RELEASE_VERSION)"/' helm/values.yaml
	@sed -i '' 's/version: .*/version: $(RELEASE_VERSION)/' helm/Chart.yaml
	@sed -i '' 's/appVersion: .*/appVersion: "$(RELEASE_VERSION)"/' helm/Chart.yaml
	helm package helm

.PHONY: publish-helm
publish-helm: ## Helm publish
	helm push ./kro-${RELEASE_VERSION}.tgz oci://${HELM_IMAGE}

.PHONY:
release: build-image publish-image package-helm publish-helm

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = true
endif

.PHONY: deploy-kind
deploy-kind: export KO_DOCKER_REPO=kind.local
deploy-kind: ko
	$(KIND) delete clusters ${KIND_CLUSTER_NAME} || true
	$(KIND) create cluster --name ${KIND_CLUSTER_NAME}
	$(KUBECTL) --context kind-$(KIND_CLUSTER_NAME) create namespace kro-system
	make install
	# This generates deployment with ko://... used in image.
	# ko then intercepts it builds image, pushes to kind node, replaces the image in deployment and applies it
	helm template kro ./helm --namespace kro-system --set image.pullPolicy=Never --set image.ko=true | $(KO) apply -f -
	kubectl wait --for=condition=ready --timeout=1m pod -n kro-system -l app.kubernetes.io/component=controller
	$(KUBECTL) --context kind-${KIND_CLUSTER_NAME} get pods -A
