# Graph Revisions

When a Graph's spec changes, the controller needs to transition managed resources from the old
desired state to the new desired state in a safe order. This requires a three-way comparison: the
previous desired state, the new desired state, and the actual state in the cluster. Without a record
of the previous desired structure, the controller cannot compute this diff — which resources are
new, which changed, which should be removed, and in what order those operations are safe. A
GraphRevision is that record — an immutable snapshot of the desired state for a given generation of
the Graph spec.

A GraphRevision is a separate Kind, independently addressable. The Graph controller manages both
Kinds.

## GraphRevision

A GraphRevision is namespace-scoped, in the same namespace as its parent Graph. It is a snapshot of
the Graph's spec at a point in time. CEL expressions (`${...}`) are preserved as authored — they are
evaluated at reconcile time against live cluster state, not at snapshot time. Identity labels and
other operational metadata are applied at reconciliation, not stored in the revision.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: GraphRevision
metadata:
  name: my-app-g00003
  labels:
    internal.kro.run/graph-name: my-app
    internal.kro.run/graph-generation: "3"
  ownerReferences:
    - apiVersion: experimental.kro.run/v1alpha1
      kind: Graph
      name: my-app
      uid: ...
spec:
  nodes:
    - id: deployment
      readyWhen:
        - ${deployment.status.availableReplicas > 0}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: my-app
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
        spec:
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - port: 80
```

### Spec

The spec contains the Graph's node declarations — the same structure as Graph `spec.nodes`. The spec
is immutable. A structural change to the Graph produces a new GraphRevision, never an update to an
existing one.

Dependencies between nodes are derived from CEL expression references, not persisted.

### Status

- **`Ready`** — all resources in this revision have been applied and are healthy. Starts `Unknown`,
  converges to `True` when the controller has reconciled all resources to their desired state. The
  controller stops observing a superseded revision — its Ready condition reflects the last-known
  state, not a live signal.

  The revision spec is immutable (what to apply). The revision status is a write-only observation
  surface — it records what happened, but the controller's operational inputs are the DAG and the
  applied set, not revision status. The applied set is derived from
  [identity labels](003-ownership.md#identity-labels) on managed resources, not persisted in
  revision status.

## Lifecycle

GraphRevisions use the Graph's `metadata.generation` as their identity — propagated from the Graph
to the revision to every managed resource via the `generation` label. One number, one source: "which
version of the Graph produced this." Gaps in the generation sequence mean compilation failed — the
spec changed but the revision could not be produced.

When the Graph's `metadata.generation` advances, the controller snapshots a new revision. If
compilation fails, no revision is created; the failure is reported on the Graph. A revision can only
exist if compilation succeeded. The latest revision is always the reconciliation target — the
controller compares it against the previous revision to determine which resources to create, update,
or remove, then converges in dependency order.

A superseded revision is deleted once it is no longer needed — all of its resources have either been
migrated to the new revision or removed from the cluster. Revisions have ownerReferences to their
parent Graph — both are namespace-scoped, so ownerReferences work directly. On Graph deletion, the
Graph's finalizer holds removal until the controller completes a full unwind of managed resources in
reverse dependency order. Once the finalizer clears and the Graph is removed, the API server
cascading-deletes any remaining revisions.

Revisions are derived artifacts. If manually deleted, the controller regenerates the active revision
from the current Graph spec on the next reconcile. The applied set — derived from
[identity labels](003-ownership.md#identity-labels), not the revision — is the authoritative record
of what was written to the cluster.

## Why Not

**Inline in Graph status.** Status reports observations, not historical intent. Unbounded growth. GC
means mutating the parent.

**Sub-resource.** API server extension work that doesn't carry its weight today.

**Graph IS the revision.** "Graph" would mean two things — mutable live object and immutable
snapshot. A marker field does the work a separate Kind should do.

**Two controllers.** GraphRevision has no independent lifecycle. Partial coupling carries the cost
of coordination without the benefit of independence.

**Labels and finalizers for revision ownership.** Graph and GraphRevision are always in the same
namespace — cross-scope doesn't apply. OwnerReferences give cascading delete for free, eliminating
revision cleanup code. Managed resources still use labels and finalizers because they can be
cluster-scoped or in different namespaces.
