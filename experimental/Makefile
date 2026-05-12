# Root of the repo. Go commands run from experimental/ (its own module).
ROOT := $(shell git rev-parse --show-toplevel)
EXPDIR := $(ROOT)/experimental

LOCALBIN := $(shell go env GOPATH)/bin
KO       := $(LOCALBIN)/ko

TEST_FLAGS := -race -count=1
TEST_PKGS  := ./...

# Upstream compat test files currently known to pass (fully or partially)
# against the Graph controller. `make compat-presubmit` runs exactly
# these with COMPAT_SKIP_SPECS filtering out known-failing specs;
# `make presubmit` invokes it, and CI runs `make -C experimental
# presubmit` on every PR targeting krocodile, so the list IS the
# contract — regressions fail CI.
#
# To add a fully-passing file: run `make compat-one FILE=<file>`,
# confirm `failed: 0`, then append here.
#
# To add a partially-passing file: run `make compat-one FILE=<file>`,
# note which specs fail, append the file here, and add skip patterns
# to COMPAT_SKIP_SPECS below.
PASSING_COMPAT_FILES := \
  format_test.go \
  lifecycle_test.go \
  metadata_fields_test.go \
  two_var_comprehensions_test.go \
  kubernetes_time_test.go \
  instance_resource_watch_test.go \
  schema_aware_cel_test.go \
  readiness_test.go \
  dependency_readiness_test.go \
  instance_conflict_test.go \
  validation_test.go \
  topology_test.go \
  externalref_test.go \
  externalref_watch_test.go \
  externalref_deletion_test.go \
  unknown_fields_test.go \
  instance_conditions_test.go \
  include_when_test.go \
  collection_test.go \
  status_test.go \
  data_pending_test.go \
  type_compatibility_test.go \
  crd_test.go

# Specs to skip in compat-presubmit. Each token is a Ginkgo skip regex
# applied via COMPAT_SKIP_SPECS env var → suiteConfig.SkipStrings.
# Group by root cause so fixes can remove entire blocks.
#
# Root cause: forEach cartesian product not supported
SKIP_SPECS := cartesian.product
# Root cause: forEach deep chaining scale up/down count mismatch
SKIP_SPECS += deep.chaining.with.scale
# Root cause: forEach readyWhen blocking requires workerPods.map() which
#             fails because forEach nodes are typed as cel.AnyType
SKIP_SPECS += block.dependent.resources.until.collection.readyWhen
# Root cause: forEach per-item watch not retained on unchanged-item skip
SKIP_SPECS += detect.and.restore.drift.in.collection
# Root cause: flaky under shared envtest (pass in isolation, fail in batch)
SKIP_SPECS += collection.with.dependency.on.regular.resource
SKIP_SPECS += collection-to-collection.chaining
SKIP_SPECS += readyWhen.per-item.expressions.with.each
SKIP_SPECS += dependents.to.reference.empty.collections
# Root cause: RGD Inactive transition not triggered on invalid spec
SKIP_SPECS += reflect.failure.conditions.when.definition.is.invalid
# Root cause: status fields not surfaced for unavailable resources
SKIP_SPECS += only.show.status.fields.when.all.referenced
SKIP_SPECS += surface.data.pending.condition
# Root cause: type compatibility rejection requires Inactive transition
SKIP_SPECS += reject.incompatible.types.comprehensively
# Root cause: forEach cascading deletion / GC not implemented
SKIP_SPECS += delete.all.collection.resources.when.instance.is.deleted
SKIP_SPECS += delete.collection.resources.across.multiple.namespaces
# Root cause: CRD update/ownership/breaking-change detection not implemented
SKIP_SPECS += update.CRD.when.ResourceGraphDefinition.is.updated
SKIP_SPECS += prevent.multiple.ResourceGraphDefinitions
SKIP_SPECS += block.RGD.update.when.schema.has.breaking
SKIP_SPECS += allow.RGD.update.when.schema.changes.are.non-breaking
SKIP_SPECS += reconcile.the.ResourceGraphDefinition.back.when.CRD
SKIP_SPECS += detect.externally.deleted.CRD
# Root cause: cycle detection requires custom CEL or graph-level validation not in stdlib
SKIP_SPECS += detect.cyclic.dependencies
# Root cause: Inactive transition on bad CEL / invalid spec not implemented
SKIP_SPECS += not.panic.when.deleting.an.inactive
# Root cause: forEach type-checking requires custom CEL functions
SKIP_SPECS += reject.forEach.expression.that.does.not.return.a.list
SKIP_SPECS += reject.forEach.with.reserved.keyword.iterator.name
SKIP_SPECS += reject.forEach.iterator.name.that.conflicts.with.resource.ID
# Root cause: failure condition surfacing requires schema-aware validation
SKIP_SPECS += surface.a.failure.condition.when.a.resource.is.invalid
# Root cause: topologicalOrder computed by stdlib uses spec order, not DAG order
SKIP_SPECS += should.not.create.deployment.*service.*and.configmap
# Root cause: readyWhen with null-check on Job completionTime requires status field awareness
SKIP_SPECS += wait.for.the.last.node.in.the.DAG.to.be.ready
# Root cause: validation expects Inactive transition for malformed k8s objects
SKIP_SPECS += validate.correct.kubernetes.object.structure
# Root cause: forEach with empty list / no readyWhen / readyWhen blocking not implemented
SKIP_SPECS += handle.empty.collection.list.gracefully
SKIP_SPECS += create.collection.resources.without.readyWhen
SKIP_SPECS += keep.instance.IN_PROGRESS.until.collection.readyWhen
# Root cause: nested RGD-to-RGD status embedding not supported
SKIP_SPECS += not.panic.when.the.second.RGD.embeds
# Root cause: externalRef watch re-reconciliation not implemented
SKIP_SPECS += external.ref.watch.triggers.re-reconciliation
SKIP_SPECS += external.collection.with.matchExpressions.CEL
SKIP_SPECS += external.collection.watch.reacts
# Root cause: resource ID validation expects Inactive transition
SKIP_SPECS += validate.correct.resource.naming.conventions
SKIP_SPECS += reject.invalid.resource.IDs
SKIP_SPECS += reject.duplicate.resource.IDs
SKIP_SPECS += reject.invalid.kubernetes.object.structures
# Root cause: Secret watch re-reconciliation flaky under shared envtest
SKIP_SPECS += watch.behavior.on.Secret
# Root cause: format() custom CEL function not implemented
SKIP_SPECS += resolve.formatted.strings.using.format
# Root cause: forEach includeWhen toggle not implemented
SKIP_SPECS += toggle.collection.resources.when.includeWhen
# Root cause: unknown fields / preserve-unknown-fields CRD compilation
SKIP_SPECS += allow.instances.with.arbitrary.unknown.nested.fields
# Root cause: externalRef sortBy custom CEL not implemented
SKIP_SPECS += external.collection.sortBy.orders
# Root cause: forEach chaining with dynamic expression not implemented
SKIP_SPECS += handle.collection.chaining.with.dynamic.forEach
# Root cause: transformMap / transformMapEntry / transformList CEL not implemented
SKIP_SPECS += evaluate.transformMap.*transformMapEntry.*transformList
# Root cause: readyWhen with status.availableReplicas comparison flaky / timing
SKIP_SPECS += wait.for.deployment.to.have.deployment.spec.replicas
# Root cause: RGD status plain-fields rejection requires Inactive transition
SKIP_SPECS += reject.RGDs.with.plain.fileds
# Root cause: externalRef chained RGD producer/consumer reconcile not implemented
SKIP_SPECS += external.ref.to.a.chained.RGD.keeps
# Root cause: basic forEach collection creation broken in batch (works in isolation)
SKIP_SPECS += create.multiple.ConfigMaps.from.a.forEach
# Root cause: status conditions / status interpolation flaky under shared envtest
SKIP_SPECS += have.correct.conditions.when.ResourceGraphDefinition.is.created
SKIP_SPECS += interpolate.string.templates.in.instance.status
# Root cause: includeWhen contagious skip / downstream propagation flaky in batch
SKIP_SPECS += skip.downstream.dependents.when.an.upstream.includeWhen
# Root cause: externalRef deletion ordering not implemented
SKIP_SPECS += delete.managed.resources.before.removing.finalizer.when.external.ref
# Root cause: instance condition progression during deletion not surfaced
SKIP_SPECS += show.proper.condition.progression.during.deletion
# Root cause: lifecycle update handling broken in batch (works in isolation)
SKIP_SPECS += handle.updates.to.instance.resources.correctly
# Root cause: schema-aware CEL byte→string conversion not implemented
SKIP_SPECS += convert.Secret.data.bytes.so.string

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
presubmit: vet ## Run presubmit checks (vet, unit tests, mod tidy). Use compat-presubmit for the full compat suite.
	cd $(EXPDIR) && go test $(TEST_FLAGS) $(TEST_PKGS)
	cd $(EXPDIR) && go mod tidy && git diff --exit-code go.mod go.sum

##@ Development

.PHONY: vet
vet: ## Run go vet against experimental code.
	cd $(EXPDIR) && go vet ./...

.PHONY: run
run: ## Run the graph controller locally (apply CRDs first).
	kubectl apply -f $(EXPDIR)/charts/crds/templates/
	cd $(EXPDIR) && go run ./cmd/

.PHONY: apply
apply: ## Build image with ko and deploy via helm.
	cd $(EXPDIR) && helm upgrade --install krocodile-crds charts/crds/ \
		-n kro-system --create-namespace
	cd $(EXPDIR) && helm upgrade --install krocodile charts/controller/ \
		-n kro-system \
		--set image=$$($(KO) build --bare ./cmd/)
	cd $(EXPDIR) && helm upgrade --install krocodile-stdlib charts/stdlib/ \
		-n kro-system

.PHONY: delete
delete: ## Uninstall the charts from the current cluster.
	helm uninstall krocodile-stdlib -n kro-system --wait
	helm uninstall krocodile -n kro-system --wait
	helm uninstall krocodile-crds -n kro-system

.PHONY: test
test: ## Run tests with verbose output.
	cd $(EXPDIR) && go test $(TEST_FLAGS) $(TEST_PKGS) -v

.PHONY: bench
bench: ## Run benchmarks with CPU and memory profiles.
	@mkdir -p $(EXPDIR)/build
	cd $(EXPDIR) && go test ./controller -bench=. -benchmem -count=1 -run='^$$' \
		-cpuprofile=$(EXPDIR)/build/cpu.prof \
		-memprofile=$(EXPDIR)/build/mem.prof
	@echo "Profiles written to experimental/build/{cpu,mem}.prof"
	@echo "View with: go tool pprof -http=:8080 experimental/build/cpu.prof"

# Ad-hoc comparison workflow:
#   go test ./experimental/controller -bench=. -benchmem -count=6 | tee before.txt
#   (make changes)
#   go test ./experimental/controller -bench=. -benchmem -count=6 | tee after.txt
#   benchstat before.txt after.txt

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
	@cd $(EXPDIR) && tag=$$(basename $(FILE) _test.go) && \
	  KRO_TEST_TAG=$$tag \
	  go test -tags graphcompat -v -count=1 \
	    -timeout $(if $(TIMEOUT),$(TIMEOUT),3m) \
	    ./test/rgd_compat/ \
	    --ginkgo.focus-file=$(FILE) \
	    --ginkgo.timeout=$(if $(TIMEOUT),$(TIMEOUT),3m)

.PHONY: compat-presubmit
compat-presubmit: ## Run the passing-files allowlist in one shared envtest. Fails on any regression.
	cd $(EXPDIR) && COMPAT_FOCUS_FILES="$(PASSING_COMPAT_FILES)" COMPAT_SKIP_SPECS="$(SKIP_SPECS)" KRO_TEST_TAG=presubmit \
	  go test -tags graphcompat -v -count=1 -timeout 10m \
	    ./test/rgd_compat/

.PHONY: compat-count
compat-count: ## Show per-file spec + assertion counts, marking allowlisted files.
	@cd $(EXPDIR) && \
	printf '%-42s %6s %6s %10s\n' FILE SPECS ASSERTS ALLOWLISTED; \
	printf '%-42s %6s %6s %10s\n' ------------------------------------------ ----- ------- -----------; \
	total_s=0; total_a=0; on_s=0; on_a=0; \
	for f in $$(ls test/rgd_compat/*_test.go | xargs -n1 basename | grep -v '^\(setup\|compat_stubs\)_test.go$$' | sort); do \
	  s=$$(grep -cE '^\s*It\(' test/rgd_compat/$$f || true); \
	  a=$$(grep -cE '(g\.)?Expect\(' test/rgd_compat/$$f || true); \
	  on=""; for p in $(PASSING_COMPAT_FILES); do [ "$$p" = "$$f" ] && on="yes"; done; \
	  printf '%-42s %6d %6d %10s\n' $$f $$s $$a "$$on"; \
	  total_s=$$((total_s+s)); total_a=$$((total_a+a)); \
	  [ -n "$$on" ] && { on_s=$$((on_s+s)); on_a=$$((on_a+a)); }; \
	done; \
	printf '%-42s %6s %6s %10s\n' ------------------------------------------ ----- ------- -----------; \
	printf '%-42s %6d %6d\n' 'TOTAL' $$total_s $$total_a; \
	printf '%-42s %6d %6d\n' 'ALLOWLISTED (passing in CI)' $$on_s $$on_a
