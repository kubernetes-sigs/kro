# CEL Write-Back: Stateful Transitions in ResourceGraphDefinitions

**Author:** Rafael Panazzo (https://github.com/pnz1990)
**Status:** Draft / Open Question (see §Design Details and §Other Solutions Considered)
**Filed:** 2026-03-09

## Problem statement

kro's CEL expressions are strictly read-only: they evaluate against an immutable
snapshot of the parent instance CR and produce derived values that appear in
`status` fields and child resource templates. There is no mechanism for a CEL
expression to persist a computed value back into the instance CR across reconcile
cycles.

This blocks an entire class of controller patterns where the interesting logic is
a **stateful transition**: read the current state, compute the next state, persist
it so the next reconcile cycle sees it. The pattern appears across many production
domains:

- **Certificate rotation scheduling** — record `lastRotatedAt` after a rotation
  Job completes so future reconciles can compute when next rotation is due.
- **Progressive delivery phase advancement** — advance `phase` from `canary` to
  `stable` after a child Deployment's `readyWhen` is satisfied.
- **Autoscaling cooldown enforcement** — persist `lastScaleTime` so a 5-minute
  cooldown window can be enforced across reconcile cycles.
- **Workflow step retry counters** — increment `retryCount` on Job failure so
  the RGD knows whether to retry or give up.
- **Metered quota accumulation** — accumulate `totalRequestsSeen` against a quota
  limit as batch Jobs report processed counts.

Today, every one of these patterns requires a bespoke external controller that:
1. Watches for a triggering event,
2. Reads the parent CR,
3. Computes the next state, and
4. Issues a patch back to the parent CR.

This is exactly the reconcile-patch cycle kro already performs internally — just
without the ability to direct the output to a field that survives the next
reconcile. For Kubernetes-native applications that want kro as a complete
declarative state machine, this is a significant architectural gap.

## Proposal

Introduce a new virtual resource type in the RGD `resources` list — a
**write-back node** — that evaluates CEL expressions and persists the results
back into the instance CR rather than creating a Kubernetes resource. The node
integrates with the existing DAG, `includeWhen`, and idempotency machinery.

Two surface-level alternatives exist (§Design Details). They solve the same
problem with different trade-offs around the Kubernetes `spec`/`status`
ownership convention and GitOps compatibility. This proposal presents both and
asks the kro maintainers to choose.

#### Overview

A write-back node in a `resources` list looks structurally like any other
resource entry but carries a `type:` discriminator (`specPatch` or `stateWrite`)
and a map of CEL expressions instead of a resource template. When the reconciler
reaches a write-back node in the DAG, it evaluates the expressions, diffs the
results against the current CR state, and issues a patch if anything changed.

```yaml
resources:
  # Existing resource nodes (unchanged)
  - id: rotationJob
    template:
      # ... Job template ...

  # New: write-back node
  - id: recordRotation
    type: stateWrite             # or: specPatch (see §Design Details)
    includeWhen:
      - "${rotationJob.status.succeeded == 1}"
    state:
      lastRotatedAt: "${rotationJob.status.completionTime}"
```

Write-back nodes participate in the DAG fully: they can depend on child resource
statuses, and other nodes can declare dependencies on them. The ordering semantics
are identical to existing resource nodes.

#### Design details

##### Background: spec/status ownership

Before presenting the alternatives, the ownership convention they operate against:

**The convention:** `spec` is user-owned desired state; `status` is
controller-owned observed state. The `status` subresource enforces this split at
the API level.

**The HPA/Argo CD problem** is the canonical example of controller-writes-spec
conflict: HPA writes `spec.replicas`; Argo CD syncs `spec.replicas: 1` from Git;
a reconcile loop results. The ecosystem trajectory since ~2019 (Gateway API,
Crossplane, ACK) is moving toward controller-owned `status` fields for this
reason.

**Kubernetes Late Initialization** (official pattern, documented in
`contributors/devel/sig-architecture/api-conventions.md`) explicitly blesses
controllers writing `spec` fields the user has not set — HPA (`spec.replicas`),
the scheduler (`spec.nodeName`). The key constraint: never overwrite a
user-provided value. SSA field ownership enforces this mechanically.

Both alternatives are evaluated against this backdrop.

---

##### Alternative A: `specPatch` — CEL Write-Back to Instance `spec`

A `specPatch` node evaluates CEL expressions and patches specified fields in the
parent instance CR's `spec` using a dedicated SSA field manager
(`kro.run/specpatch`).

```yaml
resources:
  - id: applyDoT
    type: specPatch
    includeWhen:
      - "${schema.spec.poisonTurns > 0 || schema.spec.burnTurns > 0}"
    patch:
      heroHP:      "${max(0, schema.spec.heroHP - (schema.spec.poisonTurns > 0 ? 5 : 0) - (schema.spec.burnTurns > 0 ? 8 : 0))}"
      poisonTurns: "${max(0, schema.spec.poisonTurns - 1)}"
      burnTurns:   "${max(0, schema.spec.burnTurns   - 1)}"
```

**How it works:**
1. `includeWhen` is evaluated — if false, node is skipped (treated as ready per
   the existing `IsIgnored` convention).
2. Each `patch` expression is evaluated against `schema.spec.*` and any ready
   child resource statuses.
3. Idempotency check: if computed values already match the current instance spec
   → no-op (no patch, no watch event, no new reconcile).
4. If different: SSA patch is issued to the parent instance CR's `spec` using
   field manager `kro.run/specpatch`.
5. The spec change triggers a watch event; a new reconcile is enqueued.

**Technical feasibility** (grounded in `kubernetes-sigs/kro @ main`, March 2026):

- `pkg/graph/node.go`: add `NodeTypeSpecPatch NodeType = 5` — one line.
- `pkg/graph/builder.go`: detect `type: specPatch` in `buildRGResource`; parse
  `patch` entries as `FieldDescriptor`s with `StandaloneExpression: true`,
  flowing through the existing `validateAndCompileTemplates` pipeline (CEL
  type-checking is free). Admission constraints: patch keys must exist in
  `schema.spec`; types must be compatible; `readyWhen` and `forEach` disallowed.
- `pkg/runtime/node.go`: `GetDesired()` returns `nil, nil` for new node type.
  New method `EvaluateSpecPatch() (map[string]any, error)`. Audit all other
  `NodeType` dispatch sites for explicit no-op cases.
- `pkg/controller/instance/resources.go`: new branch in `reconcileNodes` calls
  `EvaluateSpecPatch()`, diffs, issues SSA patch via the same client used for
  `applyManagedFinalizerAndLabels`.
- **RBAC:** no new grants required — kro's service account already holds
  `update`/`patch` on all instance CRs.
- **Estimated scope:** ~300 lines of Go across 4 files plus test coverage.

**Risks:**

| Risk | Severity | Mitigation |
|------|----------|------------|
| GitOps reconcile loop (HPA/Argo CD problem) | Serious | SSA field manager is partial mitigation. Argo CD SSA is not the default as of 2026. RGD docs must explicitly require `ignoreDifferences` or SSA mode for `specPatch`-targeted fields. |
| Infinite reconcile loop (non-convergent expression) | Medium | Idempotency check is the hard guard. Admission-time warning when same field appears in `patch` and expression without `includeWhen`. |
| `spec` ownership inversion | Architectural | Fields targeted by `specPatch` become controller-owned but live in `spec`. CRD can be annotated with `x-kro-managed: true` — non-standard but establishes convention. |
| Reconcile amplification latency | Acceptable | N sequential state transitions require N round-trips (~1s each on a loaded cluster). |

---

##### Alternative B: `stateWrite` — CEL Write-Back to a Managed `status` Sub-Object

A `stateWrite` node evaluates CEL expressions and patches a dedicated
`status.state` sub-object within the instance CR. kro already owns the `status`
subresource of every instance CR it manages. Writing to `status` is a controller
action by definition — this alternative is compatible with the Kubernetes
convention without caveats.

```yaml
resources:
  - id: applyDoT
    type: stateWrite
    includeWhen:
      - "${schema.spec.poisonTurns > 0 || schema.spec.burnTurns > 0}"
    state:
      heroHP:      "${max(0, schema.spec.heroHP - (schema.spec.poisonTurns > 0 ? 5 : 0) - (schema.spec.burnTurns > 0 ? 8 : 0))}"
      poisonTurns: "${max(0, schema.spec.poisonTurns - 1)}"
      burnTurns:   "${max(0, schema.spec.burnTurns   - 1)}"
```

Computed values are written to `status.state.<fieldName>`. Other RGD expressions
reference them as `${schema.status.state.heroHP}` (see CEL context note below).

**How it works:**
1. `includeWhen` is evaluated.
2. Each `state` expression is evaluated.
3. Idempotency check: if values already match `status.state.*` → no-op.
4. Controller patches `status.state.*` via the `/status` endpoint —
   **does not advance spec `resourceVersion`** and does not conflict with GitOps.
5. Status change triggers a watch event; new reconcile is enqueued.

**Key difference from Alternative A:** GitOps tools (`kubectl apply`, Argo CD,
Flux) never write `status`. A Git manifest declaring an instance CR with certain
`spec` fields will never conflict with kro writing `status.state.*`. No
`ignoreDifferences` configuration is required.

**CEL context extension required:**
Today, `buildContext` in `runtime/node.go` calls `withStatusOmitted` for the
instance object, stripping `status` from CEL evaluation context:
```go
// For schema (instance), strip status - users should only access spec/metadata.
if depID == graph.InstanceNodeID {
    obj = withStatusOmitted(obj)
}
```
`stateWrite` nodes that implement counters must read `status.state.*` to compute
the next value. This requires either:
- A `schema.state.*` shorthand that reads from `status.state` but is blocked from
  reading the rest of `status`; or
- Lifting `withStatusOmitted` only for `stateWrite` nodes, scoped to
  `status.state.*`.

Either is feasible. This is the primary additional complexity vs. Alternative A.

**Bootstrap (first reconcile):** `status.state` does not exist on first reconcile.
Expressions must use `has()`:
```yaml
state:
  retryCount: "${has(schema.status.state.retryCount) ? schema.status.state.retryCount + 1 : 1}"
```
The `has()` macro is standard CEL and already supported in kro.

**Schema:** The generated CRD gains a `status.state` object field with properties
inferred from `stateWrite` nodes. RGD authors declare initial values in
`schema.status.state` using the existing `"type | default=value"` syntax.

**Risks:**

| Risk | Severity | Mitigation |
|------|----------|------------|
| `status` stretched beyond "observed state" | Architectural | Crossplane `status.atProvider` is accepted precedent. Retry counters, phase strings, and timestamps are arguably "controller-observed state." |
| No user-direct-write path to `status.state` | Medium | Users cannot `kubectl patch status/state/phase` without `/status` subresource rights. Workaround: `spec.desiredPhase` field that `stateWrite` reads. |
| Watch event on status change | Same as Alt A | Identical reconcile amplification profile. |
| Read-your-writes within one reconcile cycle | Same as Alt A | Inherent to event-driven reconcile model. Updated `status.state` value visible only on next reconcile cycle. |

---

##### Side-by-side comparison

| Dimension | Alt A: `specPatch` | Alt B: `stateWrite` |
|---|---|---|
| GitOps compatibility | **Requires `ignoreDifferences` or SSA mode** | Compatible by construction |
| Kubernetes convention | Late Initialization (officially blessed) | Stretches `status` = observed state |
| Ecosystem trajectory | Cautious (SSA required for safety) | With Crossplane `atProvider`, Argo Workflows |
| User DX: reading state | `spec.heroHP` — natural | `status.state.heroHP` — less natural |
| Bootstrap (first reconcile) | No issue — `spec` fields exist at creation | Requires `has()` guard or schema defaults |
| Implementation complexity | Lower (~300 lines Go, 4 files) | Medium (requires lifting `withStatusOmitted`) |
| Infinite loop risk | Idempotency + `includeWhen` guard | Same |
| Reconcile amplification | Same | Same |

---

##### Real-world use cases (both alternatives)

**Certificate rotation scheduling:**
```yaml
# Alt A
- id: recordRotation
  type: specPatch
  includeWhen:
    - "${rotationJob.status.succeeded == 1 && schema.spec.lastRotatedAt != rotationJob.status.completionTime}"
  patch:
    lastRotatedAt:   "${rotationJob.status.completionTime}"
    nextRotationDue: "${rotationJob.status.completionTime + schema.spec.rotationInterval}"

# Alt B
- id: recordRotation
  type: stateWrite
  includeWhen:
    - "${rotationJob.status.succeeded == 1 && schema.status.state.lastRotatedAt != rotationJob.status.completionTime}"
  state:
    lastRotatedAt:   "${rotationJob.status.completionTime}"
    nextRotationDue: "${rotationJob.status.completionTime + schema.spec.rotationInterval}"
```

With Alt A, `spec.lastRotatedAt` and `spec.nextRotationDue` must be excluded from
the GitOps manifest or configured as `ignoreDifferences`. With Alt B, they are in
`status.state` and GitOps tooling never touches them.

**Progressive delivery phase advancement:**
```yaml
# Alt A
- id: advanceToCanary
  type: specPatch
  includeWhen:
    - "${schema.spec.phase == 'blue-only' && greenDeployment.status.readyReplicas >= schema.spec.canaryReplicas}"
  patch:
    phase: "canary"

# Alt B
- id: advanceToCanary
  type: stateWrite
  includeWhen:
    - "${schema.status.state.phase == 'blue-only' && greenDeployment.status.readyReplicas >= schema.spec.canaryReplicas}"
  state:
    phase: "canary"
```

With Alt B, an operator who wants to manually reset the phase must patch
`status/state/phase` directly (requires `/status` subresource rights). This may
be undesirable for operational tooling.

**Workflow step retry counter:**
```yaml
# Alt A — no has() guard needed, spec field has schema default
- id: incrementRetry
  type: specPatch
  includeWhen:
    - "${step1Job.status.failed > 0 && schema.spec.retryCount < schema.spec.maxRetries}"
  patch:
    retryCount: "${schema.spec.retryCount + 1}"

# Alt B — requires has() guard on first reconcile
- id: incrementRetry
  type: stateWrite
  includeWhen:
    - "${step1Job.status.failed > 0 && (has(schema.status.state.retryCount) ? schema.status.state.retryCount : 0) < schema.spec.maxRetries}"
  state:
    retryCount: "${(has(schema.status.state.retryCount) ? schema.status.state.retryCount : 0) + 1}"
```

---

##### What write-back nodes cannot do (out of scope regardless of alternative)

These patterns require an external controller regardless of which alternative is
implemented. This section exists to set honest expectations.

- **Per-request entropy** — CEL is deterministic; there is no `rand()` or
  `uuid()`. Random seeds require an external entropy source (e.g., a CR UID
  assigned by the API server).
- **Array element mutation by index** — `monsterHP[2] = 0` cannot be expressed
  in standard CEL. A `kro.lists.set` extension is a separate proposal.
- **Transactional multi-field mutations within one reconcile cycle** — if
  `fieldB = f(fieldA)` and `fieldA` is itself being written by a prior write-back
  node, the new value of `fieldA` is not visible within the same reconcile cycle.
- **Time-triggered transitions** — kro's reconciler is event-driven; `requeueAfter`
  is a separate feature request.
- **Cross-instance mutations** — write-back nodes can only target the instance CR
  of the current reconcile.

---

##### Implementation notes (code-level)

Applies to both alternatives unless noted.

`api/v1alpha1/types.go`: Add `Type string` and either `Patch map[string]string`
(Alt A) or `State map[string]string` (Alt B) to the `Resource` struct.

`pkg/graph/node.go`: Add `NodeTypeSpecPatch NodeType = 5` (Alt A) or
`NodeTypeStateWrite NodeType = 5` (Alt B).

`pkg/graph/builder.go`: Detect new `type` field in `buildRGResource`. Parse
`patch`/`state` entries as `FieldDescriptor`s. Add admission-time constraints.
**Alt B only:** include `status.state.*` in the schema passed to CEL validation.

`pkg/runtime/node.go`: `GetDesired()` returns `nil, nil` for new node type. New
method `EvaluateWriteBack() (map[string]any, error)`. **Alt B only:** lift
`withStatusOmitted` for `stateWrite` nodes, scoped to `status.state`.

`pkg/controller/instance/resources.go`: New branch in `reconcileNodes` calls
`EvaluateWriteBack()`, diffs, and issues the appropriate patch (`spec` for Alt A,
`status` for Alt B).

## Other solutions considered

**Alternative C: `function` — gRPC/webhook escape hatch**

Rather than extending the CEL DSL, a `function` virtual resource type would
delegate state transition logic to an external gRPC service or HTTP webhook. The
endpoint receives the full current instance CR state and returns a patch to apply.
This is the model Crossplane adopted with Composition Functions.

```yaml
resources:
  - id: applyDoT
    type: function
    endpoint: "grpc://dot-applier.rpg-system.svc:8080"
    includeWhen:
      - "${schema.spec.poisonTurns > 0}"
```

**Why it is not the primary answer here:**

- Every RGD author who uses `function` nodes must deploy, version, and operate a
  gRPC service alongside their RGD. Alternatives A and B require no additional
  infrastructure.
- The use cases this proposal targets (counters, timestamps, phase advancement)
  are simple enough for CEL. gRPC is the right escape hatch for logic that
  genuinely cannot be expressed in CEL (loops, external I/O, probabilistic
  computation) — not for arithmetic on integer fields.
- Crossplane's lesson is "know when the DSL has reached its natural limit." CEL
  write-back is a natural extension of CEL's existing role in kro; it is not
  exceeding that limit.

Alternative C is a complementary escape hatch, not a substitute for Alternatives
A or B. A roadmap that includes (A or B) for common cases and C for complex cases
is more complete than either alone.

**External bespoke controller (status quo)**

The current workaround is an external controller that watches for events, reads
the parent CR, computes the next state, and patches. This works but eliminates
kro's value proposition for the affected workflows. It also requires cluster
operators to deploy and maintain additional binaries and RBAC for each use case.

## Scoping

#### What is in scope for this proposal?

- New virtual resource type (`specPatch` or `stateWrite`) in RGD resource lists.
- CEL expression evaluation for computed field values, reusing the existing
  `FieldDescriptor`/`validateAndCompileTemplates` pipeline.
- Integration with existing `includeWhen` (conditional execution).
- Idempotency: skip write when computed values already match current state.
- DAG ordering: write-back nodes can depend on child resource statuses and other
  write-back nodes can depend on them.
- Schema validation at admission time: target fields must be declared; types must
  be compatible; `readyWhen` and `forEach` are disallowed on write-back nodes.
- Admission-time warning when a node reads and writes the same field without a
  convergent `includeWhen`.

#### What is not in scope?

- Array element mutation by index — requires a separate CEL library extension
  (`kro.lists.set`).
- Per-request entropy / non-deterministic expressions — CEL is deterministic by
  design; not changing that.
- Time-triggered transitions (`requeueAfter`) — separate feature request.
- Cross-instance mutations — out of scope by definition.
- Transactional atomicity across multiple write-back nodes in one reconcile cycle.
- The `function` gRPC escape hatch — a follow-up proposal if the community wants
  it.

## Testing strategy

#### Requirements

- A running kro-enabled cluster (or envtest with CRD installation).
- At least one RGD per use case (certificate rotation, retry counter, phase
  advancement) for integration tests.

#### Test plan

- **Unit tests** (`pkg/graph`, `pkg/runtime`): new node type dispatches correctly;
  expression evaluation returns expected maps; `GetDesired()` returns nil; all
  existing switch dispatch sites have explicit no-op cases.
- **Admission tests**: `specPatch`/`stateWrite` with non-existent target field →
  admission rejection; type mismatch → rejection; `readyWhen` on write-back node
  → rejection; self-referencing patch without `includeWhen` → warning.
- **Integration tests** (envtest or live cluster): write-back node writes expected
  value to spec/status; idempotency — no second write if value unchanged; DAG
  ordering — write-back node blocked until dependency resource is ready;
  `includeWhen = false` → node skipped, no patch issued; convergence — counter
  increments to expected value then stops.
- **GitOps compatibility test** (Alt A): `ignoreDifferences` configured → no loop;
  SSA field manager → Argo CD does not overwrite `specPatch`-managed fields.

## Discussion and notes

**Open questions for kro maintainers:**

1. **Which alternative aligns better with kro's long-term API philosophy?** The
   `spec`/`status` split is a fundamental Kubernetes convention. Does kro want to
   follow it strictly (Alt B) or pragmatically (Alt A, with caveats)?

2. **Is the `withStatusOmitted` restriction intentional as a general rule, or
   only for non-write-back nodes?** Alt B requires relaxing it for `stateWrite`
   nodes. Is there a risk of non-write-back nodes accidentally referencing stale
   status values if the restriction is relaxed?

3. **Should write-back nodes be a first-class `type:` field on resource entries,
   or a top-level `transitions:` stanza in the RGD schema?** A `transitions:`
   stanza would make the state machine semantics explicit and avoid a "resource
   entry that creates no resource." The cost is a larger API surface change.

4. **What is the intended behavior when a `specPatch` conflicts with a concurrent
   user write?** Optimistic concurrency (409 retry) is the standard answer, but
   the retry policy and behavior on repeated conflict need to be defined.

5. **Should kro emit a `SpecManagedByKro` condition on instance CRs with
   `specPatch` nodes?** This would provide a discoverable signal to operators and
   GitOps tools that certain `spec` fields are controller-managed.

**Precedent references:**

- Kubernetes Late Initialization: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
- Cluster API controller-assigned fields (`spec.providerID`): https://cluster-api.sigs.k8s.io/reference/api/spec-and-status
- Crossplane `status.atProvider`: https://docs.crossplane.io/latest/concepts/managed-resources/#atprovider
- Crossplane Composition Functions: https://docs.crossplane.io/latest/concepts/composition-functions/
- Argo Workflows `status`-only state model: https://argoproj.github.io/argo-workflows/fields/
- HPA + Argo CD `ignoreDifferences`: https://argo-cd.readthedocs.io/en/stable/user-guide/diff-strategies/
- KREP-002: Collections — closest precedent for a new virtual node type with new DAG semantics.

**Origin context:**

This proposal originated from building [Krombat](https://github.com/pnz1990/krombat),
a Kubernetes-native turn-based RPG that uses kro ResourceGraphDefinitions as the
game state machine. The write-back limitation manifests concretely there — DoT
ticks (`heroHP -= 5`, `poisonTurns -= 1`) must survive across reconcile cycles,
but today the Go backend handles all HP mutations and patches the Dungeon CR spec
directly. That external controller pattern is exactly what this proposal aims to
make unnecessary for the simple cases.

The Krombat use cases that *cannot* be addressed by this proposal — combat dice
rolls using Attack CR UIDs as random seeds, per-monster HP arrays, and room
transition logic with multi-field atomic mutations — remain in the Go backend
permanently. The proposal is honest about this boundary.
