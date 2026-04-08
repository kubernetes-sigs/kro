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

After a resource is processed, its `id` enters scope as a variable available to CEL expressions in
downstream resources. The value and type depend on the template shape — see below.

#### template

A Kubernetes resource declaration. The template shape determines how the controller handles it:

- **Owns** — specifies fields beyond identity (labels, annotations, spec, data). The Graph creates
  the resource if it doesn't exist, applies the specified fields via SSA, and tracks the resource
  for cleanup.
- **Watch** — specifies only identity (`apiVersion`, `kind`, `metadata.name`, `metadata.namespace`).
  The Graph reads the resource into scope without managing it. If the resource does not exist, the
  node is Pending.
- **Collection Watch** — specifies `apiVersion` and `kind` with an optional `selector` but no
  `metadata.name`. The Graph discovers matching resources and enters them into scope as an array.
  Create and delete events on matching objects trigger re-reconciliation.
- **Contribute** — specifies a subset of fields on a resource that another actor manages (e.g., only
  status, or only labels). The Graph applies exactly those fields and tracks them for cleanup. This
  is how a Graph writes status to a custom resource, adds labels to an existing object, or
  contributes any partial state.

After processing, the resource enters scope under its `id` — as the full Kubernetes object for Owns,
Watch, and Contribute templates, or as an array for Collection Watch.

```yaml
# Watch — reads an existing ConfigMap into scope
- id: config
  template:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shared-config

# Collection Watch — discovers all Pods matching a selector
- id: allPods
  template:
    apiVersion: v1
    kind: Pod
    selector:
      app: my-app

# Watch — reads the WebApp instance into scope
- id: webapp
  template:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: my-app

# Contribute — writes status fields to the WebApp
- id: webappStatus
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

#### forEach

Stamps the template once per item in a collection. The collection is a CEL expression referencing a
collection watch or any array in scope. Each iteration binds the item to a named variable available
within the template.

```yaml
- id: policies
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    metadata:
      name: default-deny
      namespace: ${ns.metadata.name}
    spec:
      podSelector: {}
      policyTypes:
        - Ingress
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

#### readyWhen

A list of CEL expressions. All must evaluate to `true` for the resource to be considered ready.
readyWhen is a health signal — it feeds the Graph's aggregated status and tells operators whether
the system has converged. It does not gate downstream execution. Dependents proceed as soon as the
resource is applied and its data is in scope, regardless of readyWhen. If a downstream CEL
expression references a field that does not yet exist on the resource, the expression fails to
evaluate and the dependent is not applied — data availability is an implicit gate. propagateWhen is
for the case where the field exists but is not yet valid.

Any object in scope exposes a `.ready()` CEL function that evaluates the object's standard
Kubernetes readiness conditions.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...
```

#### propagateWhen

A list of CEL expressions. All must evaluate to `true` for the resource's updated data to flow to
dependents. During transitions (e.g., a rolling update), dependents skip re-evaluation while
propagateWhen is unsatisfied — they retain their last-applied state. When propagateWhen passes,
dependents re-evaluate against the now-stable data.

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

## Dependencies

Dependencies are inferred from CEL expression references. If resource B contains
`${A.metadata.name}`, B depends on A. The dependency graph is a DAG — cycles are not permitted.
Resources with no dependency relationship are independent and are processed in parallel.

## Nested Graphs

A Graph whose template contains another Graph creates a nested scope. The inner Graph is a regular
Kubernetes object — it is created via the API server and reconciled independently by its own
reconciliation. Each level is a separate reconciliation loop with its own resource scope.

The combination of collection watch, forEach, and nested Graphs creates per-instance controllers. A
parent Graph watches a kind via collection watch, forEach stamps one child Graph per item, and each
child Graph independently reconciles resources for its item.

```yaml
# Parent Graph — watches all Namespaces, stamps a child Graph per Namespace
- id: namespaces
  template:
    apiVersion: v1
    kind: Namespace
    selector: {}

- id: perNamespace
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${ns.metadata.name}-resources
    spec:
      resources:
        - id: nsRef
          template:
            apiVersion: v1
            kind: Namespace
            metadata:
              name: ${ns.metadata.name}
        - id: policy
          template:
            apiVersion: networking.k8s.io/v1
            kind: NetworkPolicy
            metadata:
              name: default-deny
              namespace: $${nsRef.metadata.name}
            spec:
              podSelector: {}
              policyTypes:
                - Ingress
```

The parent's scope and the child's scope are independent. A spec change to one Namespace triggers
reconciliation of that child only — O(1) per instance, not O(N) re-evaluation of the collection.

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
resource types and is written via contribute templates targeting the custom resource's status
subresource.
