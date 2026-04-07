# Graph

A Graph is kro's primitive for composing Kubernetes resources. It is a scope —
a set of resources with a shared lifecycle, dependency order, and data flow.
Create a Graph and its resources are created and continuously reconciled.
Delete it and they are cascade deleted. Nested Graphs create nested scopes.

## Object

A Graph is a namespace-scoped Kubernetes custom resource.

```yaml
apiVersion: kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app
spec:
  resources:
    - id: deployment
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

Template fields can contain `${...}` CEL expressions that reference other
resources by `id`. Each resource's `id` is a scope variable — after a resource
is applied or read, its full Kubernetes object (including status) is available
to downstream expressions. A standalone expression (`${expr}` as the entire
string) preserves the CEL return type. An embedded expression
(`prefix-${expr}-suffix`) string-interpolates.

## Spec

### Resources

`spec.resources` is a list of resource entries. Each entry has an `id` and one
primary field that determines its type: `template`, `externalRef`, or `template`
with `forEach`. Declaration order is not significant — execution order is
determined by the dependency graph.

`spec.resources` can itself be a CEL expression that evaluates to an array of
resource definitions. This enables dynamic resource list construction (e.g., CEL
list concatenation to compose a resource list from parts).

#### id

A string that names the resource within the Graph's scope. Other resources
reference it by this name in CEL expressions. Must be unique within the Graph.
Must be camelCase — hyphens are parsed as subtraction by the CEL evaluator
(e.g., `my-app` is interpreted as `my` minus `app`).

After a resource is processed, its `id` enters scope with a type that depends
on the resource kind:

- **template** — the full Kubernetes object (including status after apply).
- **externalRef by name** — the full Kubernetes object.
- **externalRef by selector** — an array of objects.
- **forEach** — an array of the applied objects. The forEach is a single node
  in the dependency graph; downstream resources depend on the collection as a
  unit, not on individual items. Per-item scope isolation is achieved by
  stamping a Graph per item (see Nested Graphs).

#### template

A Kubernetes object to create and own. The Graph applies it via server-side
apply and cascade deletes it when the Graph is deleted. Template fields can
contain CEL expressions (`${...}`) that reference other resources in scope.

#### externalRef

Reads an existing object from the cluster into scope without owning it. The
Graph does not create, modify, or delete it.

- **By name** — `metadata.name` and optionally `metadata.namespace`. Reads a
  single object. Changes to the object trigger re-reconciliation.
- **By selector** — `metadata.selector` with label match criteria (or `{}` for
  all). Reads a collection into scope as an array. Create and delete events on
  matching objects trigger re-reconciliation.

```yaml
# Single object
- id: config
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shared-config
      namespace: default

# Collection
- id: allPods
  externalRef:
    apiVersion: v1
    kind: Pod
    metadata:
      selector: {}
```

#### forEach

Stamps the template once per item in a collection. The collection is a CEL
expression referencing an externalRef selector or any array in scope. Each
iteration binds the item to a named variable available within the template.

```yaml
- id: controllers
  forEach:
    instance: ${watchInstances}
  template:
    apiVersion: kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${instance.metadata.name}-controller
    spec:
      resources: [...]
```

forEach is not aware of what it stamps. If the template happens to be a Graph,
the result is a nested scope — but that is a property of the Graph kind, not of
forEach. See Nested Graphs below.

#### readyWhen

A list of CEL expressions. All must evaluate to `true` for the resource to be
considered ready. Downstream resources that depend on this resource are blocked
until it is ready. If omitted, the resource is ready as soon as it is
successfully applied or read.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...
```

#### includeWhen

A list of CEL expressions. All must evaluate to `true` for the resource to be
included. If any condition is false, the resource is skipped — it is not
created, does not enter scope, and downstream resources that depend on it are
also excluded. Exclusion is contagious through the dependency graph.

```yaml
- id: ingress
  includeWhen:
    - ${config.data.enableIngress == "true"}
  template: ...
```

### Contribution

A template that underspecifies a resource — omitting required fields or
specifying only status and metadata — is a contribution. The controller detects
this from the template shape against the resource's OpenAPI schema and applies
it as a partial server-side apply with force ownership, leaving the rest of the
object untouched. This is how a Graph writes status back to an external object.

Contributions typically reference an externalRef by id in their template (e.g.,
`${webapp.metadata.name}`), which creates an implicit dependency through CEL
reference inference — the Graph reconciles the externalRef first, ensuring the
target object exists before writing. The contribution auto-splits into two API calls when the
template contains `.status` fields — one for metadata/spec via regular SSA, one
for the status subresource. Graph authors do not need to know about the
Kubernetes subresource split.

The Graph's status surfaces which resources were detected as contributions,
making the inference observable. If the OpenAPI schema is unavailable or
incorrect, the detection is visible in status rather than silently falling
back to a full apply.

```yaml
# webapp is an externalRef reading the WebApp instance
- id: statusContrib
  template:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: ${webapp.metadata.name}
      namespace: ${webapp.metadata.namespace}
    status:
      deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
      address: ${service.status.loadBalancer.ingress[0].hostname}
```

## Dependencies

Dependencies are inferred from CEL expression references. If resource B
contains `${A.metadata.name}`, B depends on A. The dependency graph is a DAG —
cycles are not permitted. Resources with no dependency relationship are
independent and may be processed in parallel.

The DAG determines:

- **Creation order** — topological order of the dependency graph.
- **Deletion order** — reverse topological order.
- **Readiness propagation** — a resource blocked by `readyWhen` blocks its
  dependents. A resource excluded by `includeWhen` excludes its dependents.
- **Error isolation** — independent branches of the DAG proceed even when one
  branch is blocked. A conflict or failure on one resource does not halt
  unrelated work.

## Nested Graphs

A Graph whose template contains another Graph creates a nested scope. The inner
Graph is a regular Kubernetes object — it is created via the API server and
reconciled independently by its own reconciliation. Each level is a
separate reconciliation loop with its own resource scope.

### Per-Instance Controller Pattern

The combination of externalRef(selector), forEach, and nested Graphs produces
per-instance controllers:

1. A parent Graph watches a collection via externalRef with a selector.
2. forEach stamps one child Graph per item in the collection.
3. Each child Graph reads its specific item via externalRef with a name.

```
parent (L0)          — watches collection, stamps child Graphs
└─ child (L1)        — reads its specific item, owns its resources
```

This separates collection membership from per-item reconciliation. The
collection watch fires only on create and delete — adding or removing items.
Each child's externalRef(name) watch fires on changes to that specific item.
A spec change to one item triggers reconciliation of that child only — O(1)
reactivity per instance, not O(N) re-evaluation of the entire collection.

### Evaluation Boundary

The parent's CEL evaluation produces the child Graph's spec as data. The
child's controller evaluates that spec independently. No shared state between
levels beyond what is persisted to the Kubernetes API server. The API server is
the boundary between evaluation passes.

Each controller does exactly one pass on its own spec. Expressions inside data
values (e.g., `${...}` strings stored in a resource's spec fields) survive as
opaque strings — they are not re-scanned. Nested evaluation is recursive
through Kubernetes persistence, not through string re-scanning.

To pass an expression through to a child Graph, use `$${...}`. The controller
strips one `$`, producing `${...}` in the output. The child's controller
evaluates the resulting expression against its own scope.

Escaping depth by target level:
- `${...}` — evaluated by this Graph
- `$${...}` — evaluated by the child Graph (one level down)
- `$$${...}` — evaluated two levels down

## Status

The Graph's status is the controller's observation of the Graph's current
state.

### Conditions

`status.conditions` follows the standard Kubernetes condition convention —
each condition has `type`, `status`, `reason`, `message`, and
`lastTransitionTime`. `lastTransitionTime` is preserved when the status value
does not change between reconciles.

The Graph defines two conditions on orthogonal axes:

**`Accepted`** — the spec is valid. CEL expressions compiled, the dependency
graph is acyclic, and resource definitions are structurally correct. Set once
when the spec is processed. Permanent until the spec changes. Alarm on `False`
immediately — the Graph will never converge until the spec is fixed.

| Reason              | Meaning                                        |
|---------------------|------------------------------------------------|
| `Accepted`          | Spec is valid                                  |
| `CompilationFailed` | CEL expression failed to compile               |
| `CycleDetected`     | Dependency graph contains a cycle              |
| `InvalidSpec`       | Structural spec error                          |

**`Ready`** — resources are reconciled and healthy. Convergent — may be `False`
during normal operation while resources are being created or becoming ready.
Alarm on `False` or `Unknown` for too long.

| Reason              | Meaning                                               |
|---------------------|-------------------------------------------------------|
| `Ready`             | All resources reconciled                               |
| `DataPending`       | CEL expressions cannot resolve; waiting for data       |
| `ResourcesNotReady` | Resources applied but readyWhen conditions not met     |
| `FieldConflict`     | SSA 409 — another actor owns fields the Graph manages  |
| `ReconcileError`    | Fatal error during reconciliation                      |

```yaml
status:
  conditions:
    - type: Accepted
      status: "True"
      reason: Accepted
      lastTransitionTime: "2025-01-15T10:29:00Z"
    - type: Ready
      status: "True"
      reason: Ready
      message: "All 3 resources reconciled"
      lastTransitionTime: "2025-01-15T10:30:00Z"
```

The Graph's status contains only controller-managed conditions. There are no
user-defined status fields on the Graph itself. User-defined status (e.g.,
`deploymentReady`, `address`) lives on custom resource types and is written
via contributions — a resource template targeting the custom resource's status
subresource.

## Ownership and Cleanup

### Tracking

Managed resources are tracked via labels (`internal.kro.run/graph-name`,
`internal.kro.run/graph-namespace`), not ownerReferences. OwnerReferences require
same-scope (namespace → namespace or cluster → cluster) and bind to UIDs that
break on delete+recreate. Labels work across scope boundaries.

A finalizer on the Graph object ensures managed resources are cleaned up before
the Graph is removed from the API server.

### Deletion

When a Graph is deleted, the controller deletes resources it successfully
applied — identified by the presence of a template-hash annotation
(`internal.kro.run/template-hash`) that proves the controller applied to them.
Resources that were never successfully applied (e.g., pre-existing objects that
caused a conflict on first apply) are not deleted. The resource pre-existed the
Graph and should survive its deletion.

Deletion unwinds in reverse dependency order. Child Graphs have their own
finalizers — the parent waits for each child to complete its own cleanup chain
before proceeding.

### Field Ownership

Owned resources are applied via server-side apply without force. If another
actor has taken ownership of a field the controller previously owned, the apply
returns a 409 Conflict. The controller surfaces this as a status condition
rather than silently overwriting. The conflict is permanent until the external
actor releases the field or the Graph spec changes.

Contributions are the exception — they apply with force because their purpose
is writing fields on objects someone else owns.

## Not Covered

- **Revisions.** Immutable snapshots of Graph state. See 002-revisions.
- **Performance.** Content-addressed apply gating, expression caching, metadata
  watch elision. See 003-performance.
- **Progressive rollout.** Canary, leveled promotion, health-gated waves.
- **Multi-cluster.** Cross-cluster resource management.

## Discarded Paths

**OwnerReferences for lifecycle binding.** OwnerReferences don't work across
scope boundaries (namespace-scoped Graph owning cluster-scoped CRD, or vice
versa). They bind to UIDs that break on delete+recreate. Labels plus finalizers
work universally and survive object recreation.

**Manual dependency ordering.** CEL reference analysis derives the graph
automatically. Explicit ordering adds maintenance burden without information
the system cannot infer.

**Re-scanning evaluated output.** An earlier model considered multi-pass string
evaluation where output from one expression could be re-scanned for further
expressions. This creates injection vectors, makes each level's behavior depend
on all prior levels, and prevents independent debugging. The current model —
one evaluation pass per controller, with the API server as the boundary — makes
each level independently observable and testable.

**ForceOwnership on owned resources.** Would silently steal fields from other
actors. The 409 Conflict signal is correct — it surfaces a real problem (two
actors managing the same field) rather than hiding it. Contributions use
ForceOwnership because writing to someone else's object is their explicit
purpose.

**OwnerReferences for deletion tracking.** Same cross-scope problem as
lifecycle binding. The annotation-based tracking (`internal.kro.run/applied-resources`)
records what the controller successfully applied, and the template-hash
annotation (`internal.kro.run/template-hash`) provides proof of ownership. Together they enable safe cleanup
without UID-based references.
