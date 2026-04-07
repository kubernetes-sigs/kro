# Graph

A Graph is kro's primitive for composing Kubernetes resources. It is a scope — a set of resources
with a shared lifecycle, dependency order, and data flow. Create a Graph and its resources are
created and continuously reconciled. Delete it and its resources are cleaned up. Nested Graphs
create nested scopes.

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

Template fields can contain `${...}` CEL expressions that reference other resources by `id`. Each
resource's `id` is a scope variable — after a resource is applied or read, its full Kubernetes
object (including status) is available to downstream expressions. A standalone expression (`${expr}`
as the entire string) preserves the CEL return type. An embedded expression
(`prefix-${expr}-suffix`) string-interpolates.

## Spec

### Resources

`spec.resources` is a list of resource entries. Each entry has an `id` and a `template`. Declaration
order is not significant — execution order is determined by the dependency graph.

`spec.resources` can itself be a CEL expression that evaluates to an array of resource definitions.
This enables dynamic resource list construction (e.g., CEL list concatenation to compose a resource
list from parts).

#### id

A string that names the resource within the Graph's scope. Other resources reference it by this name
in CEL expressions. Must be unique within the Graph. Must be camelCase — hyphens are parsed as
subtraction by the CEL evaluator (e.g., `my-app` is interpreted as `my` minus `app`).

After a resource is processed, its `id` enters scope with a type that depends on the template shape:

- **Substance template** — the full Kubernetes object (including status after apply).
- **Metadata-only template** — the full Kubernetes object (read from cluster).
- **Selector** — an array of objects.
- **forEach** — an array of the applied objects. The forEach is a single node in the dependency
  graph; downstream resources depend on the collection as a unit, not on individual items. Per-item
  scope isolation is achieved by stamping a Graph per item (see Nested Graphs).

#### template

A Kubernetes resource to apply and manage. The Graph applies the template and manages the lifecycle
of the specified fields. Template fields can contain CEL expressions (`${...}`) that reference other
resources in scope.

The template shape determines behavior:

- **Substance** [TODO: I don't like this term. This is just a normal resource] — specifies fields
  beyond identity (labels, annotations, spec, data, status). The Graph manages those fields and
  tracks the resource for cleanup.
- **Metadata-only [TODO: this is an object watch, we're missing collection watch (kind+optional
  selector, no metadata)] ** — specifies only identity (`apiVersion`, `kind`, `metadata.name`,
  `metadata.namespace`). The Graph reads the resource into scope without managing it.
- **Partial** — specifies a subset of fields (e.g., only status). The Graph manages exactly those
  fields. This is how a Graph writes status back to an external [todo: this is overspecific, it
  could be any object] object.

See 005-ownership [todo: dont ref other docs] for the ownership model and field management
mechanics.

#### selector

Discovers a collection of resources matching label criteria and enters them into scope as an array.
Create and delete events on matching objects trigger re-reconciliation.

```yaml
- id: allPods
  template:
    apiVersion: v1
    kind: Pod
    selector: {} # optional
```

#### forEach

Stamps the template once per item in a collection. The collection is a CEL expression referencing a
selector or any array in scope. Each iteration binds the item to a named variable available within
the template.

```yaml
- id: controllers
  forEach:
    instance: ${watchInstances} [todo: can we use a better example? instances/controllers are not clear]
  template:
    apiVersion: kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${instance.metadata.name}-controller
    spec:
      resources: [...]
```

forEach is not aware of what it stamps [todo: is this note necessary?]. If the template happens to
be a Graph, the result is a nested scope — but that is a property of the Graph kind, not of forEach.
See Nested Graphs below.

#### readyWhen

A list of CEL expressions. All must evaluate to `true` for the resource to be considered ready.
readyWhen is a health signal — it feeds the Graph's aggregated status and tells operators whether
the system has converged. It does not gate downstream execution. Dependents proceed as soon as the
resource is applied and its data is in scope, regardless of readyWhen. [todo: add a note that you
can reference .ready() in cel on any object]

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...
```

#### propagateWhen

A list of CEL expressions. All must evaluate to `true` for the resource's updated data to flow
[propagate?] to dependents. During transitions (e.g., a rolling update), dependents skip
re-evaluation while propagateWhen is unsatisfied — they retain their last-applied state via the
template hash. When propagateWhen passes, dependents re-evaluate against the now-stable data.

readyWhen and propagateWhen are complementary: readyWhen is a health signal (feeds Graph status),
propagateWhen is a data flow gate (controls when dependents see new values). A resource without
propagateWhen propagates immediately — dependents re-evaluate on every reconcile.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  propagateWhen:
    - ${deployment.status.updatedReplicas == deployment.spec.replicas}
  template: ...
```

#### includeWhen

A list of CEL expressions. All must evaluate to `true` for the resource to be included. If any
condition is false, the resource is skipped — it is not created and does not enter scope. Downstream
resources that depend on it cannot evaluate (the data is not in scope) and are also absent.

```yaml
- id: ingress
  includeWhen:
    - ${config.data.enableIngress == "true"}
  template: ...
```

## Dependencies

Dependencies are inferred from CEL expression references. If resource B contains
`${A.metadata.name}`, B depends on A. The dependency graph is a DAG — cycles are not permitted.
Resources with no dependency relationship are independent and may be processed in parallel.

## Nested Graphs

A Graph whose template contains another Graph creates a nested scope. The inner Graph is a regular
Kubernetes object — it is created via the API server and reconciled independently by its own
reconciliation. Each level is a separate reconciliation loop with its own resource scope.

### Per-Instance Controller Pattern

The combination of selectors, forEach, and nested Graphs produces per-instance controllers:

1. A parent Graph watches a collection via a selector.
2. forEach stamps one child Graph per item in the collection.
3. Each child Graph reads its specific item via a metadata-only template.

```
parent (L0)          — watches collection, stamps child Graphs
└─ child (L1)        — reads its specific item, owns its resources
```

This separates collection membership from per-item reconciliation. The collection watch fires only
on create and delete — adding or removing items. Each child's metadata-only template watch fires on
changes to that specific item. A spec change to one item triggers reconciliation of that child only
— O(1) reactivity per instance, not O(N) re-evaluation of the entire collection.

### Evaluation Boundary

The parent's CEL evaluation produces the child Graph's spec as data. The child's controller
evaluates that spec independently. No shared state between levels beyond what is persisted to the
Kubernetes API server. The API server is the boundary between evaluation passes.

Each controller does exactly one pass on its own spec. Expressions inside data values (e.g.,
`${...}` strings stored in a resource's spec fields) survive as opaque strings — they are not
re-scanned. Nested evaluation is recursive through Kubernetes persistence, not through string
re-scanning.

To pass an expression through to a child Graph, use `$${...}`. The controller strips one `$`,
producing `${...}` in the output. The child's controller evaluates the resulting expression against
its own scope.

Escaping depth by target level:

- `${...}` — evaluated by this Graph
- `$${...}` — evaluated by the child Graph (one level down)
- `$$${...}` — evaluated two levels down

## Status

The Graph's status is the controller's observation of the Graph's current state.

### Conditions

`status.conditions` follows the standard Kubernetes condition convention — each condition has
`type`, `status`, `reason`, `message`, and `lastTransitionTime`. `lastTransitionTime` is preserved
when the status value does not change between reconciles.

The Graph defines two conditions on orthogonal axes:

**`Accepted`** — the spec is valid. CEL expressions compiled, the dependency graph is acyclic, and
resource definitions are structurally correct. Set once when the spec is processed. Permanent until
the spec changes. Alarm on `False` immediately — the Graph will never converge until the spec is
fixed.

| Reason              | Meaning                           |
| ------------------- | --------------------------------- |
| `Accepted`          | Spec is valid                     |
| `CompilationFailed` | CEL expression failed to compile  |
| `CycleDetected`     | Dependency graph contains a cycle |
| `InvalidSpec`       | Structural spec error             |

**`Ready`** — resources are reconciled and healthy. Convergent — may be `False` during normal
operation while resources are being created or becoming ready. Alarm on `False` or `Unknown` for too
long.

| Reason              | Meaning                                            |
| ------------------- | -------------------------------------------------- |
| `Ready`             | All resources reconciled                           |
| `Pending`           | CEL expressions cannot resolve; waiting for data   |
| `ResourcesNotReady` | Resources applied but readyWhen conditions not met |
| `ReconcileError`    | Fatal error during reconciliation                  |

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

The Graph's status contains only controller-managed conditions. There are no user-defined status
fields on the Graph itself. User-defined status (e.g., `deploymentReady`, `address`) lives on custom
resource types and is written via partial templates targeting the custom resource's status
subresource.

## Why Not

**Manual dependency ordering.** CEL reference analysis derives the graph automatically. Explicit
ordering adds maintenance burden without information the system cannot infer.

**Re-scanning evaluated output.** An earlier model considered multi-pass string evaluation where
output from one expression could be re-scanned for further expressions. This creates injection
vectors, makes each level's behavior depend on all prior levels, and prevents independent debugging.
The current model — one evaluation pass per controller, with the API server as the boundary — makes
each level independently observable and testable.
