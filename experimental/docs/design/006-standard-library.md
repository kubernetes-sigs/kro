# Standard Library

The standard library is a set of reusable types built on the Graph primitive. Graph is core — the
only Go controller, the runtime that everything else compiles to. The stdlib types are Graphs that
compose into higher-level behavior, written in CEL. The stdlib is self-hosting: each type is
implemented by the types below it, but each could be implemented as a raw Graph.

```
Core:    Graph     — the runtime primitive (Go controller)
Stdlib:  Kind      — define a new Kubernetes Kind (CRD + per-instance Graphs)
         Decorator — watch instances of a Kind, create a sub-Graph per instance
         Singleton — declare a resource that should exist exactly once
```

Kind is the bootstrap root (the only raw Graph). Decorator and Singleton are Kinds.

## Kind

A Kind defines a new Kubernetes Kind and implements it. It declares a schema and nodes (the
resources to create per instance). Schemas use kro's simpleSchema syntax (see upstream kro
documentation). A Kind with empty `nodes: []` creates the CRD but produces no per-instance
resources.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Kind
metadata:
  name: webapp
spec:
  kind: WebApp
  group: experimental.kro.run
  versions:
    - name: v1alpha1
      schema:
        spec:
          image: string | default=nginx
          replicas: integer | default=1
          port: integer | default=80
        status:
          deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
  nodes:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.metadata.name}
          template:
            metadata:
              labels:
                app: ${schema.metadata.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.metadata.name}-svc
        spec:
          selector:
            app: ${schema.metadata.name}
          ports:
            - port: ${schema.spec.port}
```

`schema` is the per-instance scope variable — the resource itself. `${schema.spec.image}` resolves
to the instance's `.spec.image` field. Status expressions may reference other nodes and are
contributed back to the instance.

A Kind may declare `readyWhen` and `propagateWhen` at the spec level. Both are per-instance — they
evaluate in the same scope as the Kind's nodes.

- **`readyWhen`** defines when each instance is considered healthy. Produces `.ready()` per-instance.
  The Kind's overall readiness rolls up: all instances `.ready()` → Kind Ready.
- **`propagateWhen`** controls rollout pace across instances. It is a per-instance input gate with
  sibling visibility — the expression references the instances collection and uses `.ready()` and
  `.updated()` per-item. Semantics are identical to forEach propagateWhen.

Both are optional. Without them, all instances receive updates simultaneously and readiness rolls up
from node states. Exponential rollout is opt-in:

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Kind
metadata:
  name: webapp
spec:
  kind: WebApp
  group: experimental.kro.run
  # Exponential rollout — budget doubles each wave
  propagateWhen:
    - >-
      ${instances.filter(i, i.updated() && !i.ready()).size()
       < max(1, instances.filter(i, i.updated() && i.ready()).size())}
  readyWhen:
    - ${deployment.ready() && service.ready()}
  versions:
    - name: v1alpha1
      schema:
        spec:
          image: string | default=nginx
          replicas: integer | default=1
        status:
          deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
  nodes:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.metadata.name}
          template:
            metadata:
              labels:
                app: ${schema.metadata.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.metadata.name}-svc
        spec:
          selector:
            app: ${schema.metadata.name}
          ports:
            - port: ${schema.spec.port}
```

The mirroring is structural: Kind's implicit forEach over instances uses the same
propagateWhen/readyWhen machinery as any explicit forEach. Same CEL functions, same evaluation
model, same Ready-first ordering.

The Kind controller (`kind.yaml`) is a raw Graph — the bootstrap root. See implementation for
details.

## Decorator

A Decorator watches instances of an existing Kind and creates a sub-Graph per instance. It does not
define a new Kind — it watches an existing one and creates resources per instance. Collection watch
only — for a single-resource dereference, use a plain Graph with a `ref:` node.

Each sub-Graph gets an auto-prepended `item` ref node that fetches the specific instance, plus
the user's nodes. User nodes reference `${item.metadata.name}`, `${item.spec.foo}`, etc.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Decorator
metadata:
  name: namespace-policies
spec:
  watch:
    apiVersion: v1
    kind: Namespace
    selector: {}
  nodes:
    - id: policy
      template:
        apiVersion: networking.k8s.io/v1
        kind: NetworkPolicy
        metadata:
          name: default-deny
          namespace: ${item.metadata.name}
        spec:
          podSelector: {}
          policyTypes:
            - Ingress
```

Decorator is a Kind (`decorator.yaml`). Deletion is not ordered — resources are garbage-collected
through normal Kubernetes ownership.

## Singleton

A Singleton declares a single resource that should exist exactly once. When multiple Singletons
target the same resource (same GVK + namespace + name in their template), priority resolution
determines the claim holder. Same-priority Singletons targeting the same resource must have
identical templates — divergent templates at the same priority is a user error.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Singleton
metadata:
  name: cluster-admin-binding
spec:
  priority: 100
  template:
    apiVersion: rbac.authorization.k8s.io/v1
    kind: ClusterRoleBinding
    metadata:
      name: platform-admin
    subjects:
      - kind: Group
        name: platform-admins
        apiGroup: rbac.authorization.k8s.io
    roleRef:
      kind: ClusterRole
      name: cluster-admin
      apiGroup: rbac.authorization.k8s.io
```

One Singleton, one template, one resource. If you need a group of resources to be a singleton, the
template is a Graph.

### Priority Resolution

A Singleton wins when no other Singleton targeting the same resource has higher priority. Ties at
the same priority are broken by lexicographically lowest name. Resource identity is the template's
apiVersion + kind + namespace + name. Cluster-scoped resources use empty string for the namespace
component.

Singleton is a Kind (`singleton.yaml`). Each per-instance Graph watches all Singletons, computes the
winner for its target, and gates resource creation on `includeWhen` matching its own name.

## Installation

The standard library ships as embedded resources in the controller binary. Installation happens
through the controller's own reconciliation loop — no static CRDs beyond Graph and GraphRevision.

### CRDs

Only two CRDs are pre-installed during bootstrap:

- `graphs.experimental.kro.run` — the Graph primitive
- `graphrevisions.experimental.kro.run` — immutable revision snapshots

All other CRDs are created by the standard library itself:

- `kinds.experimental.kro.run` — created by `kind.yaml` (a Graph)
- `decorators.experimental.kro.run` — created by the Kind controller processing `decorator.yaml`
- `singletons.experimental.kro.run` — created by the Kind controller processing `singleton.yaml`

### Stdlib Resources

Three resources are embedded in `experimental/stdlib/` and applied in order:

| Resource         | `kind:` used | Purpose |
|------------------|--------------|---------|
| `kind.yaml`      | Graph        | Bootstrap root. Creates Kind CRD, watches/processes all Kinds |
| `decorator.yaml` | Kind         | Defines Decorator type, creates sub-Graphs per watched item |
| `singleton.yaml` | Kind         | Defines Singleton type, creates shared resolution Graph |

### Bootstrap Sequence

1. Bootstrap: apply Graph and GraphRevision CRDs via SSA, wait for Established
2. Controller starts
3. Stdlib Runnable applies all 3 resources via SSA with retry (kind.yaml first)
4. `kind.yaml` (a Graph) succeeds immediately — Graph CRD exists
5. `decorator.yaml` and `singleton.yaml` fail (Kind CRD not found) — retried on 2-second interval
6. Graph controller reconciles `kind.yaml` → creates Kind CRD
7. On retry: `decorator.yaml` and `singleton.yaml` succeed
8. Kind controller processes them → creates Decorator and Singleton CRDs
9. Stdlib fully materialized

## Constraints

**Namespace scoping.** Two rules, one per primitive.

*Collection watch* (`watch:` nodes) follows k8s list/watch semantics: absent `metadata.namespace`
means all namespaces, matching `ListOptions`, informer caches, and every client library. An explicit
namespace narrows the watch to one namespace. The Graph's own namespace is never used as a default —
a watch's scope is its targets, not where the Graph lives. The stdlib Kind and RGD controller
Graphs live in `kro-system` but watch instances across all namespaces; users can create Kind and RGD
instances anywhere. CRDs are cluster-scoped, so the type is available everywhere.

*Resources* (`template:`, `patch:`, `ref:`) default to the Graph's own namespace when
`metadata.namespace` is absent, matching upstream kro's behavior. Per-instance Graphs live in the
instance's namespace (or `kro-system` for cluster-scoped instances), so resources inside a
per-instance Graph default to the instance's namespace.

**Drift reconciliation during bootstrap.** When the Kind controller creates a CRD, the CRD
transitions to Established asynchronously. The owned-resource watch does not reliably trigger a
re-reconcile for this transition. The periodic drift timer (default 30m, configurable via
`--drift-interval`) re-evaluates the `includeWhen` gate and advances the bootstrap. In production
this is acceptable — bootstrap happens once. Tests use a shorter interval (5s).

**Deletion lifecycle.** Each per-instance Graph has an ownerReference pointing to the instance and
a `patch:` node that places a finalizer on the instance. When the instance is deleted, the
finalizer holds it in Terminating; the controller detects the owner's `deletionTimestamp` and
self-deletes the Graph; teardown prunes the patch node, releasing the finalizer.

## What Would Force a Fourth Primitive

A fourth primitive would be justified by a pattern that: recurs across users, cannot be expressed as
a Graph with specific `readyWhen`/`includeWhen` configuration, and is not an admission concern
(validation, mutation, defaults require webhooks, not resource-level primitives).

**Package management** is the closest candidate. Multiple teams declaring the same dependency works
today using Singleton + Kind composition. What's missing is URI-based content addressing
(`oci://...`), version constraint resolution, and transitive dependency declaration. These require
an OCI aggregation layer that doesn't exist yet. Scoped out until the infrastructure exists.

## Rejected Alternatives

**Static CRDs for all stdlib types.** Pre-installed schemaless CRDs bypassed the stdlib's own
CRD-creation mechanism. Removed in favor of self-bootstrapping with retry.

**GraphDefinition as the name for Kind.** Described the implementation mechanism rather than what
the user is doing. Controller authors implement Kinds — the name should reflect their mental model.

**Singleton with multi-resource nodes.** `spec.nodes: []` contradicted the name and complicated
resolution logic. Simplified to `spec.template: object` — one Singleton, one resource.

**Pre-compilation of Decorators and Kinds to Graphs.** Duplicates the controller's job. The types
are applied as-is and resolve through the standard reconciliation loop.

**Package as a stdlib primitive.** Conflated dependency declaration with content. Real package
management requires content-addressed references, not inlined manifests.

**Decorator with single-resource dereference.** A named-object reference is a `ref:` node in a
plain Graph. Decorator is exclusively for the "watch a kind, create sub-Graph per instance" pattern.

**Decorator as bootstrap root.** Doubled the Graph nesting depth for every Kind instance (6
reconciliation cycles vs 3). Kind as the bootstrap root keeps the critical path shorter.

**Decorator with flat forEach (no sub-Graphs).** Replaced with sub-Graphs per item for per-instance
isolation, GC-based cleanup, and independent reconciliation.
