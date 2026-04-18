# Root of the repo (where go.mod lives). All go commands run from there.
ROOT := $(shell git rev-parse --show-toplevel)

LOCALBIN := $(shell go env GOPATH)/bin
KO       := $(LOCALBIN)/ko

TEST_FLAGS := -race -count=1
TEST_PKGS  := ./experimental/...

# Upstream compat test files currently known to pass fully against the
# Graph controller. `make compat-presubmit` runs exactly these; `make
# presubmit` invokes it, and CI (.github/workflows/krocodile.yaml) runs
# `make -C experimental presubmit` on every PR targeting krocodile, so
# the list IS the contract — regressions fail CI.
#
# To add a file: run `make compat-one FILE=<file>` and confirm
# `failed: 0`, then append here.
PASSING_COMPAT_FILES := \
  format_test.go \
  lifecycle_test.go \
  metadata_fields_test.go \
  two_var_comprehensions_test.go \
  kubernetes_time_test.go \
  instance_resource_watch_test.go \
  instance_conflict_test.go \
  schema_aware_cel_test.go

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
presubmit: vet ## Run presubmit checks (vet, all tests including compat allowlist, mod tidy)
	cd $(ROOT) && COMPAT_FOCUS_FILES="$(PASSING_COMPAT_FILES)" KRO_TEST_TAG=presubmit \
	  go test $(TEST_FLAGS) -tags graphcompat $(TEST_PKGS)
	cd $(ROOT) && go mod tidy && git diff --exit-code go.mod go.sum

##@ Development

.PHONY: vet
vet: ## Run go vet against experimental code.
	cd $(ROOT) && go vet ./experimental/...

.PHONY: run
run: ## Run the graph controller locally (bootstraps CRDs automatically).
	cd $(ROOT) && go run ./experimental/cmd/ --bootstrap

.PHONY: apply
apply: ## Build and deploy the graph controller to the current cluster.
	cd $(ROOT) && $(KO) apply --bare -f experimental/deploy/

.PHONY: test
test: ## Run tests with verbose output.
	cd $(ROOT) && go test $(TEST_FLAGS) $(TEST_PKGS) -v

.PHONY: bench
bench: ## Run benchmarks.
	cd $(ROOT) && go test ./experimental/controller -bench=. -benchmem -count=1 -run='^$$'

##@ Upstream compat
#
# The compat suite at experimental/test/rgd_compat/ runs upstream kro
# core integration tests against the Graph controller. This is ONE of
# several integration harnesses in the repo — everything below applies
# only to that suite.
#
# Specs are slow (30-60s each). The suite has 38 files. Iterate per-file,
# not whole-suite:
#
#   1. `make compat-one FILE=<name>_test.go` runs one file with
#      --ginkgo.focus-file. Ginkgo prints per-spec pass/fail to stdout;
#      the controller subprocess writes build/controller-<tag>.log so
#      each file has its own log (see testenv.go: KRO_TEST_TAG).
#   2. Read build/controller-<tag>.log to trace what the controller did.
#      Map test → Graph name: an RGD becomes
#      <rgd>-resourcegraphdefinition in kro-system; an instance becomes
#      <ns>-<instance>-<kind-lower> in the instance's namespace.
#   3. Tweak stdlib (experimental/stdlib/*.yaml) or the controller.
#      Prefer stdlib CEL — the controller is shared substrate for every
#      graph, not just the compat surface.
#   4. Rerun step 1. Once all specs in the file pass, append the file
#      to PASSING_COMPAT_FILES at the top of this Makefile. CI will
#      then regression-guard it.
#
# Out-of-scope files per PR #137: annotation_label_test.go,
# applyset_test.go, externalref_*_test.go, graphrevision*_test.go,
# rgd_conditions_test.go, webhook_denial_test.go.
#
# `make compat-count` prints per-file spec + assertion counts and
# marks which files are on the CI allowlist. Run it anytime to see
# coverage progress.

.PHONY: compat-one
compat-one: ## Run one compat test file. Usage: make compat-one FILE=lifecycle_test.go [TIMEOUT=3m]
	@cd $(ROOT) && tag=$$(basename $(FILE) _test.go) && \
	  KRO_TEST_TAG=$$tag \
	  go test -tags graphcompat -v -count=1 \
	    -timeout $(if $(TIMEOUT),$(TIMEOUT),3m) \
	    ./experimental/test/rgd_compat/ \
	    --ginkgo.focus-file=$(FILE) \
	    --ginkgo.timeout=$(if $(TIMEOUT),$(TIMEOUT),3m)

.PHONY: compat-presubmit
compat-presubmit: ## Run the passing-files allowlist in one shared envtest. Fails on any regression.
	cd $(ROOT) && COMPAT_FOCUS_FILES="$(PASSING_COMPAT_FILES)" KRO_TEST_TAG=presubmit \
	  go test -tags graphcompat -v -count=1 -timeout 10m \
	    ./experimental/test/rgd_compat/

.PHONY: compat-count
compat-count: ## Show per-file spec + assertion counts, marking allowlisted files.
	@cd $(ROOT) && \
	printf '%-42s %6s %6s %10s\n' FILE SPECS ASSERTS ALLOWLISTED; \
	printf '%-42s %6s %6s %10s\n' ------------------------------------------ ----- ------- -----------; \
	total_s=0; total_a=0; on_s=0; on_a=0; \
	for f in $$(ls experimental/test/rgd_compat/*_test.go | xargs -n1 basename | grep -v '^\(setup\|compat_stubs\)_test.go$$' | sort); do \
	  s=$$(grep -cE '^\s*It\(' experimental/test/rgd_compat/$$f || true); \
	  a=$$(grep -cE '(g\.)?Expect\(' experimental/test/rgd_compat/$$f || true); \
	  on=""; for p in $(PASSING_COMPAT_FILES); do [ "$$p" = "$$f" ] && on="yes"; done; \
	  printf '%-42s %6d %6d %10s\n' $$f $$s $$a "$$on"; \
	  total_s=$$((total_s+s)); total_a=$$((total_a+a)); \
	  [ -n "$$on" ] && { on_s=$$((on_s+s)); on_a=$$((on_a+a)); }; \
	done; \
	printf '%-42s %6s %6s %10s\n' ------------------------------------------ ----- ------- -----------; \
	printf '%-42s %6d %6d\n' 'TOTAL' $$total_s $$total_a; \
	printf '%-42s %6d %6d\n' 'ALLOWLISTED (passing in CI)' $$on_s $$on_a
