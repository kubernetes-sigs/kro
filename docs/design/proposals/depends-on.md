# KREP-028: Explicit Resource Dependencies (`dependsOn`)

Authors: @lmilbaum

Tracking issue: [#110](https://github.com/kubernetes-sigs/kro/issues/110).
Prior art: [#331](https://github.com/kubernetes-sigs/kro/pull/331) (closed),
[#764](https://github.com/kubernetes-sigs/kro/pull/764) (readiness gating),
[KREP-024](./graph.md) / [#1355](https://github.com/kubernetes-sigs/kro/pull/1355).

## Summary

This proposal adds an optional `dependsOn: [<resourceID>...]` field to RGD
resources (and to the shared Graph `Node`). An explicit dependency adds an
ordering-only edge to the existing DAG: the dependent resource is applied after
its dependencies are ready, and deleted before them. Cycles are checked across
implicit (CEL-derived) and explicit edges together.

CEL inference stays the primary mechanism. `dependsOn` is an escape hatch for
real dependencies that carry no data, which today can only be expressed with
workarounds that hide the intent. The field ships as alpha, behind a feature
gate.

## Problem statement

### How ordering works today

- **Edges come only from CEL references.** Template variables, `forEach`, and
  `includeWhen` expressions create DAG edges. There are two builders:
  `buildDependencyGraph` in `pkg/graph/builder.go` (RGD path, which feeds
  `status.topologicalOrder` and `status.resources[].dependencies` through
  `resourcesInfoFromGraph` in
  `pkg/controller/resourcegraphdefinition/controller_reconcile.go`), and
  `buildDependencyGraph` / `analyzeNodeRefs` in
  `pkg/graphengine/compiler/compiler.go` (runtime).
- **`readyWhen` cannot create edges.** `analyzeReadyWhen` in `compiler.go`
  restricts `readyWhen` to references to the node itself.
- **RGD instances gate dependents on readiness.** The instance controller sets
  `exec.GateReadiness = true` (`pkg/controller/instance/controller.go`). With
  the gate on, `Simple.Apply` in `pkg/graphengine/executor/simple.go` skips a
  node with `waiting for dependency "<id>"` until each of its dependencies has
  reached a ready state. A dependency with an empty `readyWhen` is ready once
  applied (`CheckReadiness` in `pkg/graphengine/runtime/node.go`).
- **Standalone Graphs do not gate.** As described in
  [graph.md](./graph.md#evaluation-model-and-compilation-cache), a node
  evaluates as soon as the data it references is in scope; it does not wait
  for upstream `readyWhen`.
- **Deletion uses persisted waves.** During compilation, the apply-order wave
  of a node is `1 + max(wave of its hard dependencies)`, leaves are wave 1, and
  `Def` nodes are excluded (`pkg/graphengine/runtime/runtime.go`, the
  apply-order block near `ApplyOrder()`). The executor stamps the value as
  `internal.kro.run/apply-order` (`pkg/graphengine/executor/simple.go`). On
  instance deletion, `reconcileDeletion` in `pkg/controller/instance/deletion.go`
  deletes only the highest remaining wave, selected by `highestDeletionWave`. A
  child that has a `deletionTimestamp` but still exists stays in the highest
  wave and blocks lower waves. See
  [ordered-instance-deletion.md](./ordered-instance-deletion.md).
- **Prune and Graph teardown do not wait between waves.** Pruning on update or
  on an `includeWhen` flip sorts orphans by descending wave in
  `pruneGraphEngineOrphans` (`pkg/controller/instance/controller_graph_engine.go`)
  but issues the deletes in one pass. Standalone Graph teardown (`Delete` in
  `pkg/graphengine/executor/simple.go`) deletes in reverse recorded order, also
  without waiting.

### Where implicit dependencies are not enough

These are listed strongest first.

1. **Deletion of controller-managed resources (strongest).** Consider an RGD
   with a controller Deployment that runs as a ServiceAccount, and a custom
   resource that the controller reconciles and that carries a finalizer:

   ```yaml
   resources:
     - id: sa
       template: { apiVersion: v1, kind: ServiceAccount, ... }
     - id: controller            # references sa -> wave 2
       template:
         apiVersion: apps/v1
         kind: Deployment
         spec:
           template:
             spec:
               serviceAccountName: ${sa.metadata.name}
     - id: bucket                # no CEL link to controller -> wave 1
       template:
         apiVersion: example.com/v1
         kind: Bucket            # finalizer removed by the controller
   ```

   On deletion, the highest wave (`controller`, wave 2) is deleted first. The
   controller stops, so nothing removes the finalizer from `bucket`. `bucket`
   stays in wave 1 indefinitely, the instance remains in state `DELETING` with
   `ResourcesReady=Unknown` and reason `UnderDeletion`, and any cloud
   resources behind `bucket` are orphaned if someone removes the finalizer by
   hand.

   Adding `bucket.dependsOn: [controller]` moves `bucket` to wave 3. It is
   deleted first and deletion waits until it is gone, and only then is the
   controller deleted.

   Points that follow from this example:
   - Finalizer-bearing custom resources (for example ACK, Crossplane managed
     resources, or cert-manager objects) need their controller alive until the
     resource is gone.
   - Waves follow CEL chain depth, not meaning. The controller often has the
     higher wave, because it references the ServiceAccount.
   - Resources in the same wave are deleted in the same reconcile, so their
     DELETE requests race. A dependency on the controller is only expressible
     as a higher wave.
   - The same failure occurs when RBAC (a ClusterRoleBinding) or the
     controller's ServiceAccount is deleted before the resource that needs it.
   - Prune ordering is best-effort (see above), so the guarantee holds for
     full instance deletion only.

2. **Admission-time injection.** The example in issue #110 has a
   `podidentityassociation`, a `serviceaccount`, and a `deployment` (presumably
   the ACK EKS `PodIdentityAssociation`; the issue body does not name ACK). The
   Deployment does not reference the association, so its pods can be created
   before the association exists. Pods created before the association lack
   credentials. AWS documents that the Pod Identity webhook intercepts only
   newly created pods
   ([AWS blog](https://aws.amazon.com/blogs/containers/amazon-eks-pod-identity-a-new-way-for-applications-on-eks-to-obtain-iam-credentials),
   [EKS docs](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-how-it-works.html)).
   eksctl issue [#7709](https://github.com/eksctl-io/eksctl/issues/7709)
   reports a pod created right after the association without the
   `eks-pod-identity-token`. With `dependsOn` and a `readyWhen` on the
   association, the ordering is explicit:

   ```yaml
   - id: podidentityassociation
     template: { apiVersion: eks.services.k8s.aws/v1alpha1, kind: PodIdentityAssociation, ... }
     readyWhen:
       - ${podidentityassociation.status.conditions.exists(c, c.type == 'ACK.ResourceSynced' && c.status == 'True')}
   - id: deployment
     dependsOn: [podidentityassociation]
     template: { apiVersion: apps/v1, kind: Deployment, ... }
   ```

3. **Controller and CRD prerequisites.** In a comment on #110, rushmash91 gives
   the example of an IAM controller whose roles must exist before other
   controllers use them. Creation in such cases is often eventually consistent
   anyway. The value of `dependsOn` here is correct status (`waiting for
   dependency`) and, above all, deletion order. A CRD and a CR that uses it in
   the *same* RGD still fail at compile time, because schema resolution is
   eager ([deferred-schema-resolution.md](./deferred-schema-resolution.md),
   [#1293](https://github.com/kubernetes-sigs/kro/issues/1293)). That proposal
   is not implemented, and no `DeferredSchemaResolution` gate exists in
   `pkg/features`. `dependsOn` complements it and does not replace it.

### Current workarounds and their drawbacks

- **(a) A dummy annotation**, for example
  `metadata.annotations: {example.com/after: ${podidentityassociation.metadata.name}}`.
  Drawbacks: it writes meaningless data onto the cluster object; keys under the
  reserved `kro.run/` and `internal.kro.run/` prefixes are rejected by
  `validateNoKROOwnedAnnotations` in `pkg/graph/validation.go`, apart from the
  one validated `kro.run/deletion-policy` key; the intent is
  invisible, so a reviewer who "cleans up" the line silently removes the
  deletion safety; on a pod template it can trigger rollouts if the referenced
  value changes; for collections
  it needs contrived values such as `${string(size(coll))}`; and it is
  impossible on `externalRef`, which has no template.
- **(b) A tautological `includeWhen`**, for example
  `${controller.metadata.name != ''}`. It works because `includeWhen`
  references add hard edges (`analyzeIncludeWhen` in `compiler.go`). It
  overloads "should this exist?" and inherits exclusion contagion
  (`computeIgnored` in `pkg/graphengine/runtime/node.go`).

Both workarounds are invisible in `status.resources` as intentional ordering.
To be precise about the gap: a workaround produces the same edge as
`dependsOn` (the same DAG, gating, and apply-order). The case for this KREP is
therefore about intent, robustness, and ergonomics. The main issue is that
deletion safety becomes an invisible side effect of an unrelated-looking line.
It is not a missing capability in RGD.

## Proposal

### Overview

Explicit edges are additive on top of implicit edges and never replace them.
They are pure ordering: they have no effect on scope or on which data a node
can see.

### API

The field takes the YAML from issue #110, using the current `id:` field rather
than `name:`:

```yaml
resources:
  - id: controller
    template: { ... }
  - id: bucket
    dependsOn: [controller]
    template: { ... }
```

The Go sketch, added to `v1alpha1.Resource` in
`api/v1alpha1/resourcegraphdefinition_types.go`. The limit of 64 is a proposal:
it is set above the 32 used for the other `[]string` set fields
(`ShortNames` and `Categories` in `resourcegraphdefinition_types.go`), so that a
resource can order after several prerequisites. Maintainers should confirm the
value (Discussion, question 7):

```go
// DependsOn lists IDs of other resources in this RGD that must be ready
// before this resource is applied and that are deleted only after it is gone.
// Use only for dependencies not expressed through CEL references.
// +kubebuilder:validation:Optional
// +kubebuilder:validation:MaxItems=64
// +kubebuilder:validation:items:Pattern=`^[a-z][a-zA-Z0-9]*$`
// +listType=set
DependsOn []string `json:"dependsOn,omitempty"`
```

`+listType=set` makes the API server reject duplicates; the same file already
uses this marker. The pattern mirrors `lowerCamelCaseRegex` in
`pkg/graph/validation.go`.

The same field, with the same markers and doc comment, is added to the Graph
`Node` in `api/v1alpha1/graph_types.go`. `resourceToNode` in `pkg/graphengine/rgdadapter/graph_translate.go` copies it,
as it already does for `readyWhen` and `includeWhen`. GraphRevision snapshots
embed the RGD spec, so codegen updates both CRDs in `helm/crds/`.

### Semantics

- **Creation.** Topological order is computed over the combined edge set. The
  list-position tie-break (`dag.Vertex.Order`) is unchanged.
- **Readiness gating, RGD.** Automatic, through `GateReadiness` and
  `FirstUnreadyDep`. A dependency is ready when its `readyWhen` holds, or when
  it is applied if it has no `readyWhen`. Authors usually want a `readyWhen` on
  the target, because "applied" is weaker than "usable".
- **Readiness gating, standalone Graph.** The dependent waits until the explicit
  dependency has been applied and published in the current pass. This needs a
  small executor check, because without gating, ordering today comes only from
  pending data. The check runs for explicit edges even though `GateReadiness`
  is off on the Graph path, which is set only by the instance controller
  (`pkg/controller/instance/controller.go`). A dependent blocked this way is
  handled as the RGD gate handles it in `Simple.Apply`
  (`pkg/graphengine/executor/simple.go`): it is recorded in
  `ApplyResult.Unresolved` and never applied, and the apply returns a soft
  `ErrNotReady` that the Graph controller turns into a timed requeue
  (`pkg/controller/graph/controller.go`). Its previously recorded resources are
  kept, not pruned, because `diffManagedResources`
  (`pkg/controller/graph/tracking.go`) keeps any previous entry whose owning
  node is unresolved. Pruning of other nodes is not withheld. This is narrower
  than the instance path, where `pruneGate` in
  `pkg/controller/instance/controller_graph_engine.go` can withhold the whole
  prune for the cycle.
- **Deletion.** Explicit edges raise the dependent's apply-order wave, so
  instance deletion removes the dependent first and waits until it is gone. The
  deletion code is unchanged.
- **Prune and Graph teardown.** Issue order is better, but there is no waiting.
  This is a limitation and future work (see Discussion, question 6).
- **Updates.** Adding or removing `dependsOn` changes the spec hash and issues a
  new GraphRevision. The hash must sort `DependsOn` the way it sorts
  `ReadyWhen` and `IncludeWhen`. Today `normalizeSpec` in
  `pkg/graph/hash/hash.go` sorts only those two, so this is part of the
  implementation, not an existing behavior. Children have their wave annotations re-stamped through
  SSA, which is metadata-only churn. If an instance is deleted before the new
  revision reconciles, the old waves are expected to apply. This follows from
  the rollout behavior in
  [ordered-instance-deletion.md](./ordered-instance-deletion.md#rollout-behavior).
- **Validation.** Checks go in `validateResourceGraphDefinition`
  (`pkg/graph/validation.go`), with clear messages:
  - An unknown ID is an error. `dag.AddDependencies` also reports it
    (`node X does not exist`, `pkg/dag/dag.go`).
  - A self-reference is an error (`self references are not allowed`,
    `pkg/dag/dag.go`).
  - Duplicates are rejected by the CRD (`listType=set`) and by an explicit
    builder check.
  - Naming an iterator variable or `schema` is rejected as an unknown resource.
  - When the gate is off, a non-empty `dependsOn` is a build error and is never
    silently ignored. `validateDeletionPolicyAnnotation` sets the precedent.
- **Cycles.** There is a single DAG with both edge kinds. `dag.AddDependencies`
  reports a `CycleError` with its path (`pkg/dag/dag.go`). The message should
  mark which edges in the cycle are explicit.
- **Status.** `status.topologicalOrder` and the `TOPOLOGICALORDER` printcolumn
  include explicit edges automatically. A new optional field,
  `Dependency.Explicit bool`, is true only when the edge exists solely because
  of `dependsOn`. The doc strings on `Dependency` and `ResourceInformation`
  currently say "references it in a CEL expression", and will be updated. The
  instance already reports `waiting for dependency "<id>"`.

### Interactions

- **`includeWhen`.** Recommended behavior: an explicit-only edge to an excluded
  dependency counts as satisfied and does **not** propagate exclusion. Exclusion
  contagion exists because a CEL reference cannot resolve when its target is
  absent. An ordering edge has nothing to resolve. The executor already treats
  ignored nodes as ready (`pkg/graphengine/executor/simple.go`). If the same
  edge is also implicit, the implicit (contagious) semantics win. The
  alternative is to be contagious, as CEL is; that is open question 1. Making
  this non-contagious requires the runtime to distinguish data dependencies from
  ordering dependencies. `computeIgnored` currently walks `n.deps`, which is
  built from `HardDepIDs()`.
- **`forEach` ([collections.md](./collections.md)).** A dependency on a
  collection waits for the whole collection. A per-item `readyWhen` is AND-ed
  across items (`checkCollectionReadiness` in `node.go`), and an empty
  collection is ready. A collection as the dependent gates all of its items.
  Waves are per node, so all items share one wave. Item-to-item edges are out of
  scope; use CEL for those.
- **`externalRef`.** As a target, the external object must exist (a not-found
  result is a soft `ErrNotReady` in `simple.go`) and its `readyWhen` must hold.
  It has no deletion effect, because references are never deleted or stamped.
  As a dependent, the read is deferred. For selector references, an empty match
  is not an error: `applyRefCollection` in `simple.go` returns an empty slice
  with a nil error, which is published, and `checkCollectionReadiness` treats
  an empty collection as ready. A failed selector `List` is a hard error:
  `applyRefCollection` returns the wrapped `List` error, which is neither
  `ErrNotReady` nor one of the soft runtime errors recognised by
  `isSoftRuntimeErr`, so `applyNodeByKind`'s caller records it with
  `recordHard` in `Simple.Apply`. The only soft case for a ref collection is a
  dynamic-GVK target whose CRD is not installed yet (`errSchemaNotReady` from
  `mappingFor`, which runs before the `List`).
- **Status writeback node.** Unaffected. It uses soft dependencies through
  `WithSoftDependencies`.
- **Graph kind ([KREP-024](./graph.md)).** Patch nodes cannot be referenced in
  CEL, because `extractDependencies` in `compiler.go` rejects references to them.
  `dependsOn` is therefore the only way to order a resource after a patch. This
  is a new capability on Graph. `Def` nodes as targets are rejected, because
  they have no cluster state and are excluded from apply-order. The same applies
  to RGD `Def` nodes proposed in [KREP-027](https://github.com/kubernetes-sigs/kro/pull/1519).
  Only IDs in the same frame are allowed. Captured ancestor IDs are not in v1.
- **Decorators ([KREP-003](./decorators.md)).** No special handling.
- **Related proposals.** Complementary to deferred schema resolution (above),
  to propagation control
  ([KREP-006](https://github.com/kubernetes-sigs/kro/pull/861),
  [KREP-026](https://github.com/kubernetes-sigs/kro/pull/1510)), and to the
  closed level-based proposals
  ([KREP-005](https://github.com/kubernetes-sigs/kro/pull/859),
  [KREP-023](https://github.com/kubernetes-sigs/kro/pull/1215)).

### Addressing the objection in #331

The objection, from the closing comment on #331, was: "we won't proceed with
this unless if we have a good reason to - where implicit dependencies aren't
enough." This KREP answers it as follows.

1. **The deletion and admission cases above fail today, and not obviously.**
   The only fix available now is a workaround whose purpose is invisible.
2. **Two sources of truth.** Edges are additive and never subtractive. A
   redundant edge (explicit and implicit together, or explicit and already
   implied transitively) produces a Warning Event on the RGD. The RGD controller
   already has event recorders (`newEventRecorder` in
   `pkg/controller/resourcegraphdefinition/controller.go`). `kro validate`
   (`cmd/kro/commands/validate`) reports the same thing. It is not a condition,
   so `Ready` is unaffected. `status.resources[].dependencies[].explicit` makes
   the edges auditable. Documentation should present `dependsOn` as a last
   resort, similar to Terraform's `depends_on`. This repo already uses
   `depends_on` in `examples/aws/eks-cluster-mgmt/terraform/hub/eks-capability-iam.tf`.
3. **Over-declared edges slow rollouts.** This cost can be bounded. Each edge
   onto a dependency that is not yet ready costs at least one extra reconcile,
   either from a watch event or from a backoff requeue (`pkg/controller/backoff`).
   An edge can also add one deletion wave, when it lengthens the longest
   dependency chain (a node's wave is `1 + max(dependency waves)`), and each
   such wave waits until its resources are fully absent. The cost is visible in
   status. Mitigations are the redundancy
   warning and, later, a per-edge mode such as delete-only ordering (see Other
   solutions considered, item 6).
4. **The alpha gate means the feature can be removed.** It can be withdrawn
   without a breaking change while it is alpha.

### Feature gate

`ExplicitDependencies`, alpha, default off, in `pkg/features/features.go`,
alongside `CELOmitFunction`, `DeletionPolicy`, and `GraphKind`.

### Implementation sketch

This is for reviewability. This PR contains no code.

- **API.** `api/v1alpha1/resourcegraphdefinition_types.go` (`Resource`,
  `Dependency`), `api/v1alpha1/graph_types.go` (`Node`), and codegen for
  `helm/crds/`.
- **Builder.** `pkg/graph/node.go` (`ResourceSpec`, `NodeMeta.Dependencies`),
  `rgResourceSpec` in `pkg/graph/builder.go`, and
  `buildDependencyGraph` in `pkg/graph/builder.go`, which appends explicit
  edges after the CEL edges. Validation goes in `pkg/graph/validation.go`.
- **Compiler.** `compiler.Dependency` (`pkg/graphengine/compiler/program.go`)
  gains an `Explicit` flag. `HardDepIDs()` (same file) includes explicit
  dependencies.
- **Runtime.** `computeIgnored` in `pkg/graphengine/runtime/node.go` walks only
  data dependencies.
- **Executor.** An explicit-dependency gate for the standalone Graph path, which
  does not use `GateReadiness`.
- **Hashing.** `pkg/graph/hash/hash.go`.
- **Adapter.** `resourceToNode` in `pkg/graphengine/rgdadapter/graph_translate.go`.
- Both compilers must agree on the edges. This is covered by extending
  `pkg/graphengine/rgdadapter/graph_translate_contract_test.go`.

## Other solutions considered

1. **Status quo, or a dummy CEL reference.** This works, but it hides intent and
   is fragile. Rejected for the reasons in the Problem statement.
2. **Annotation-based ordering** (for example `kro.run/depends-on`, following
   the precedent of `kro.run/deletion-policy`). The annotation would land on the
   managed object, it is an unvalidated string, it does not work on
   `externalRef`, and the `kro.run/` prefix is reserved for controller-owned
   semantics. Rejected.
3. **Ordering by list position.** This would add edges between unrelated
   resources and serialize every RGD. Resource order is deliberately
   non-semantic: `pkg/graph/hash/hash.go` sorts resources by ID, so reordering
   them creates no new revision, and list position is only an intra-level
   tie-break. Rejected.
4. **Waves or phases** (Argo CD sync-waves, Helm hook weights). These are coarse
   global integers. They duplicate the existing DAG and do not compose with
   `includeWhen` or collections. [deferred-schema-resolution.md](./deferred-schema-resolution.md)
   already notes that kro "has a DAG". Rejected.
5. **Comparison with Flux, Terraform, and Pulumi.** Flux `Kustomization` and
   `HelmRelease` `dependsOn` wait for `Ready` between objects. Terraform
   `depends_on` also affects destroy order. Pulumi has a `dependsOn` resource
   option. kro follows the same model: implicit by default, with an explicit
   escape hatch.
6. **Object form** `dependsOn: [{id, phase: Create|Delete|Both}]`. This leaves
   room for delete-only edges, which do not slow rollouts, but costs verbosity.
   Moving from `[]string` to a struct later is a breaking change, so this is a
   key maintainer decision (open question 2).
7. **An internal compile option only, not exposed on Graph.** Rejected, because
   it conflicts with the one-engine principle of KREP-024.

## Scoping

### What is in scope for this proposal?

- The RGD `dependsOn` field and the Graph `Node` field, with the Graph semantics
  described above.
- Validation, cycle detection, the `Dependency.Explicit` status field, the
  redundancy warning, and the feature gate.

### What is not in scope?

- Item-level edges between collection items.
- Edges to ancestor frames.
- Waiting for absence on prune and on standalone Graph teardown.
- Cross-RGD and cross-instance dependencies.
- Per-edge modes such as delete-only ordering.
- Fixing the CRD-plus-CR compile failure, which belongs to
  [deferred-schema-resolution.md](./deferred-schema-resolution.md).

## Testing strategy

### Requirements

- envtest integration tests in `test/integration/suites/core`.
- chainsaw end-to-end tests in `test/e2e/chainsaw`.
- No cloud resources. Controllers are simulated with blocking finalizers.

### Test plan

- **Unit.**
  - Builder and compiler edges.
  - Unknown, self-reference, and duplicate errors.
  - Rejection when the gate is off.
  - The message for a mixed cycle.
  - Hash order-insensitivity for `DependsOn`.
  - Runtime apply-order with explicit edges.
  - Non-contagious exclusion through `includeWhen`.
  - The explicit-dependency gate on the Graph path.
  - Contract test: both compilers produce identical edges.
- **Integration.** Modeled on `ordered deletion waits for the higher dependency
  wave` in `test/integration/suites/core/externalref_deletion_test.go`:
  - Two resources with no CEL link, where `b.dependsOn: [a]`.
  - Assert that `b` has the apply-order annotation `2`.
  - Put a blocking finalizer on `b`, then delete the instance.
  - Assert that `a` still exists while `b` is being deleted.
  - Remove the finalizer. Both resources are then gone.
  - A creation-gating test modeled on `dependency_readiness_test.go`.
  - `includeWhen` false on the target: the dependent is still created.
  - A collection as the target.
  - Standalone Graph path: a dependent whose `dependsOn` target is not ready is
    recorded as unresolved and not applied. Its previously recorded resources are
    not pruned, and the apply returns a soft `ErrNotReady` that requeues. Once
    the target is ready, the dependent is applied.
- **E2E.** A chainsaw case in which a Deployment has `dependsOn` a ConfigMap,
  guarded by `readyWhen`.
- **Upgrade.** A case in `test/upgrade` for wave re-stamping.

### Graduation

- **Alpha.** Gate off by default. Website docs in
  `website/docs/docs/concepts/expressions/03-dependencies-ordering.md`, and
  deletion documentation.
- **Beta.** Default on after one release with no semantic changes. The
  redundancy warning has shipped. The `includeWhen` and object-form decisions
  are settled.
- **GA.** Ships with the v1 API.

## Discussion and notes

Open questions:

1. **`includeWhen`.** Should an explicit-only edge to an excluded dependency be
   non-contagious (recommended) or contagious like CEL? Non-contagious requires
   the runtime to distinguish data dependencies from ordering dependencies.
2. **API shape is a one-way door.** `[]string` matches #110, Flux, and Terraform.
   `[{id, phase}]` leaves room for delete-only edges later.
3. **Graph exposure.** Should `Node.DependsOn` be on the public Graph API in
   phase 1 (behind `GraphKind`), or should an RGD-only internal compile option
   come first? Should explicit edges be allowed to patch nodes and to `graph:`
   subgraph nodes, and what makes a subgraph "applied"?
4. **Status field naming.** `Dependency.Explicit`, or a `source`/`type` enum?
5. **Redundancy warning surface.** A Warning Event, `kro validate` output, or
   both? Should transitive redundancy be flagged, or only direct redundancy?
6. **Waiting for absence.** Should prune (`pruneGraphEngineOrphans`) and the
   standalone Graph `Delete` wait for absence? Is this KREP or a follow-up?
   Today the deletion guarantee holds only for full instance deletion.
7. **Gate name and limit.** Is `ExplicitDependencies` the right name? Is
   `MaxItems=64` right? The proposal uses 64, while the existing `[]string` set
   fields use 32 (`ShortNames` and `Categories`).
8. **KREP number.** This assumes 028 follows open
   [#1519](https://github.com/kubernetes-sigs/kro/pull/1519) (KREP-027). The
   maintainers may reassign it.
