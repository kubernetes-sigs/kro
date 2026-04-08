# Graph Revisions

When a Graph's spec changes, the controller needs to transition managed resources from the old
desired state to the new desired state in a safe order. This requires a three-way comparison: the
previous desired state, the new desired state, and the actual state in the cluster. Without a record
of the previous desired structure, the controller cannot compute this diff — which resources are new,
which changed, which should be removed, and in what order those operations are safe. A GraphRevision
is that record — an immutable snapshot of the desired state for a given generation of the Graph spec.

A GraphRevision is a separate Kind, independently addressable. The Graph controller manages both
Kinds.

## GraphRevision

A GraphRevision is namespace-scoped, in the same namespace as its parent Graph. It contains the
materialized resources that will be applied to the cluster. The Graph spec is the authoring
surface — terse, with CEL expressions and implicit dependencies. The revision is the operational
form — internal labels injected, template hashes computed, resource set finalized. CEL expressions
referencing other resources (`${...}`) remain unevaluated and are resolved at runtime against live
cluster state.

```yaml
apiVersion: internal.kro.run/v1alpha1
kind: GraphRevision
metadata:
  name: my-app-g00003
  labels:
    internal.kro.run/graph-generation: "3"
    internal.kro.run/graph-name: my-app
    internal.kro.run/hash: a1b2c3d4e5f6
  ownerReferences:
    - apiVersion: kro.run/v1alpha1
      kind: Graph
      name: my-app
      uid: ...
spec:
  resources:
    - id: deployment
      readyWhen:
        - ${deployment.status.availableReplicas > 0}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: my-app
          labels:
            internal.kro.run/graph-generation: "3"
            internal.kro.run/graph-name: my-app
            internal.kro.run/node-id: deployment
            internal.kro.run/template-hash: f7e8d9c0b1a2
        spec:
          replicas: 3
          selector:
            matchLabels:
              app: my-app
          template:
            metadata:
              labels:
                app: my-app
            spec:
              containers:
                - name: app
                  image: nginx
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${deployment.metadata.name}-svc
          labels:
            internal.kro.run/graph-generation: "3"
            internal.kro.run/graph-name: my-app
            internal.kro.run/node-id: service
            internal.kro.run/template-hash: 1a2b3c4d5e6f
        spec:
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - port: 80
```

### Spec

The spec contains one field:

- `resources` — same structure as Graph resources (`id`, `template`, `readyWhen`, `includeWhen`,
  `forEach`) with internal metadata injected and template hashes computed. Dependencies between
  resources are derived from CEL expression references and cached in memory, not persisted.

The spec is immutable, enforced by CEL validation (`self == oldSelf`). A structural change to the
Graph produces a new GraphRevision, never an update to an existing one.

Because the revision contains the final resource representations rather than input templates,
consumers are decoupled from how those resources were produced. If the Graph controller's internals
change between versions, old revisions remain valid — they describe what was actually produced, not
what needs to be re-interpreted. This is the migration path.

### Status

- **`Ready`** — all resources in this revision have been applied and are healthy. Starts `Unknown`,
  converges to `True` when the controller has reconciled all resources to their desired state. Stays
  `True` after superseded — it is a fact about this revision's resources, not about whether it's
  current.

## Lifecycle

GraphRevisions use the Graph's `metadata.generation` as their identity — propagated from the Graph
to the revision to every managed resource via `internal.kro.run/graph-generation`. One number, one
source: "which version of the Graph produced this." Gaps in the generation sequence mean
materialization failed — the spec changed but the revision could not be produced.

A content hash on each revision identifies semantically identical output across generations. Every
successful generation gets its own revision object regardless — the content hash enables the
controller to detect when a spec change produced no actual resource changes and skip the rollforward.

When the Graph's `metadata.generation` advances, the controller materializes a new revision. If
materialization fails, no revision is created; the failure is reported on the Graph. A revision can
only exist if processing succeeded. The latest revision is always the reconciliation target — the
controller compares it against the previous revision to determine which resources to create, update,
or remove, then converges in dependency order.

A superseded revision is deleted once it is no longer needed — all of its resources have either been
migrated to the new revision or removed from the cluster. Revisions have ownerReferences to their
parent Graph — both are namespace-scoped, so ownerReferences work directly. On Graph deletion, the
Graph's finalizer holds removal until the controller completes a full unwind of managed resources in
reverse dependency order. Once the finalizer clears and the Graph is removed, the API server
cascading-deletes any remaining revisions.

Revisions are derived artifacts. If manually deleted, the controller regenerates the active revision
from the current Graph spec on the next reconcile. The resource tracking index on the Graph — not
the revision — is the authoritative record of applied resources.

## Why Not

**Input templates instead of materialized output.** Couples every consumer to the controller's
internals: to interpret a historical revision, you need the same processing logic that produced it.
Materialized output decouples consumers and provides a migration path when internals evolve. A
revision's existence proves processing succeeded, eliminating the "pending" state.

**Inline in Graph status.** Status reports observations, not historical intent. Unbounded growth. GC
means mutating the parent.

**Sub-resource.** API server extension work that doesn't carry its weight today.

**Graph IS the revision.** "Graph" would mean two things — mutable live object and immutable
snapshot. A marker field does the work a separate Kind should do.

**Two controllers.** GraphRevision has no independent lifecycle. Partial coupling carries the cost
of coordination without the benefit of independence.
