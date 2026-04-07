# Graph Revisions

Every structural mutation to a Graph produces an immutable GraphRevision. A
GraphRevision is a separate Kind — an independently addressable snapshot of the
resources a Graph manages. The Graph controller manages both Kinds.

## GraphRevision

A GraphRevision is namespace-scoped, in the same namespace as its parent
Graph. It contains the fully materialized resources that will be applied to
the cluster. The Graph spec is the authoring surface — terse, with CEL
expressions and implicit dependencies. The revision is the operational truth —
labels and metadata in their final form. The only runtime step remaining is
evaluating CEL expressions (`${...}`) against live state.

```yaml
apiVersion: internal.kro.run/v1alpha1
kind: GraphRevision
metadata:
  name: my-app-g00003
  labels:
    internal.kro.run/graph-generation: "3"
    internal.kro.run/graph-name: my-app
    internal.kro.run/hash: a1b2c3d4e5f6
  finalizers:
    - internal.kro.run/finalizer
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
    - id: ingress
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: ${deployment.metadata.name}-ingress
          labels:
            internal.kro.run/graph-generation: "3"
            internal.kro.run/graph-name: my-app
            internal.kro.run/node-id: ingress
            internal.kro.run/template-hash: 9c8b7a6f5e4d
        spec:
          rules:
            - host: my-app.example.com
              http:
                paths:
                  - path: /
                    pathType: Prefix
                    backend:
                      service:
                        name: ${service.metadata.name}
                        port:
                          number: 80
status:
  conditions:
    - type: Ready
      status: "True"
    - type: Propagated
      status: "True"
    - type: Active
      status: "True"
```

### Spec

The spec contains one field:

- `resources` — same structure as Graph resources (`id`, `template`,
  `readyWhen`, `includeWhen`, `forEach`) with the template fully materialized.
  Dependencies between resources are derived from CEL expression references and
  cached in memory, not persisted.

The spec is immutable, enforced by CEL validation (`self == oldSelf`). A
structural change to the Graph produces a new GraphRevision, never an update to
an existing one.

Because the revision contains the final resource representations rather than
input templates, consumers are decoupled from how those resources were produced.
If the Graph controller's internals change between versions, old revisions
remain valid — they describe what was actually produced, not what needs to be
re-interpreted. This is the migration path.

### Status

- `Ready` — all resources in this revision have been applied and are healthy.
  Starts `Unknown`, converges to `True` when fully propagated. Stays `True`
  after superseded — it is a fact about this revision, not about whether it's
  current.
- `Propagated` — the controller has loaded this revision's materialized spec
  and is actively reconciling resources from it. Distinguishes "designated as
  active" from "actually in use by the controller."
- `Active` — this is the current revision. Goes `False` when superseded,
  preserving the transition timestamp. Does not roll up to `Ready`.

### Ownership

Ownership is tracked via finalizers, not ownerReferences. OwnerReferences
don't work across scopes (namespace → cluster or vice versa) and bind to
UIDs that break on delete+recreate. Finalizers on the Graph ensure managed
resources and revisions are cleaned up before the Graph object is removed.
Deletion unwinds in reverse dependency order. See 005-ownership for field
ownership and tracking mechanics.

### Lineage

GraphRevisions use the Graph's `metadata.generation` as their identity —
propagated from the Graph to the revision to every managed resource via
`internal.kro.run/graph-generation`. One number, one source: "which version of
the Graph produced this." Gaps in the sequence mean materialization failed for that generation —
the Graph's spec changed but the revision could not be produced.

## Lifecycle

The Graph controller produces a new revision for every spec generation. A
content hash on each revision identifies semantically identical output across
generations — but every successful generation gets its own revision object.
This preserves a clean invariant: gaps in the generation sequence
unambiguously mean materialization failed.

The lifecycle has three phases:

1. **Detect** — input hash changed.
2. **Create** — produce the GraphRevision with fully materialized resources.
   If this fails, no revision is created; the failure is reported on the Graph.
   A revision can only exist if processing succeeded.
3. **Activate** — mark the new revision as active and the previous revision
   `Active=False`. The controller reconciles managed resources toward the new
   revision: resources present in both revisions are updated if their
   template-hash changed, resources only in the new revision are created,
   resources only in the old revision are released and conditionally deleted
   per the lifecycle algorithm in 005-ownership.

Old revisions are pruned when they no longer have resources in the cluster —
once all resources have been migrated to the active revision or deleted, the
old revision is removed. The active revision is always retained. If the
latest revision cannot be created, there is no automatic fallback to the
previous active revision.

## Why not

**Input templates instead of materialized output.** Couples every consumer to
the controller's internals: to interpret a historical revision, you need the
same processing logic that produced it. Materialized output decouples consumers
and provides a migration path when internals evolve. A revision's existence
proves processing succeeded, eliminating the "pending" state.

**Inline in Graph status.** Status reports observations, not historical intent.
Unbounded growth. GC means mutating the parent.

**Sub-resource.** API server extension work that doesn't carry its weight today.

**Graph IS the revision.** "Graph" would mean two things — mutable live object
and immutable snapshot. A marker field does the work a separate Kind should do.

**Two controllers.** GraphRevision has no independent lifecycle. Partial coupling
carries the cost of coordination without the benefit of independence.

**ownerReferences.** Don't work across scopes. Bind to UIDs that break on
delete+recreate. Finalizers handle teardown.
