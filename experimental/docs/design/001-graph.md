# Graph

A Graph is kro's primitive for composing Kubernetes resources. It is a scope — a set of resources
with a shared lifecycle, dependency order, and data flow. Create a Graph and its resources are
created and continuously reconciled. Delete it and its resources are cleaned up. Nested Graphs
create nested scopes.

## Object

A Graph is a namespace-scoped Kubernetes custom resource.

```yaml
apiVersion: experimental.kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app
spec:
  nodes:
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

Template fields can contain `${...}` CEL expressions that reference other nodes by `id`. Each node's
`id` is a scope variable — after a node is applied, its full Kubernetes object (including status) is
available to downstream expressions. A standalone expression (`${expr}` as the entire string)
preserves the CEL return type. An embedded expression (`prefix-${expr}-suffix`) string-interpolates.

## Spec

### Nodes

`spec.nodes` is a list of node entries. Each entry has an `id` and a `template`. Declaration order
is not significant — evaluation order is determined by the dependency graph.

#### id

A string that names the node within the Graph's scope. Other nodes reference it by this name in CEL
expressions. Must be unique within the Graph (case-insensitive). Hyphens are not allowed — they are
parsed as subtraction by the CEL evaluator (e.g., `my-app` is `my` minus `app`). May be camelCased
for readability.

After a node is applied, its `id` enters scope as a variable available to CEL expressions in
downstream nodes. The value depends on the node's type — see below.

#### type

A node's type is the keyword it declares. Five types exist:

- **`template:`** — A full resource specification. The controller creates the resource if it
  doesn't exist, applies the specified fields via SSA, and tracks the resource for cleanup. The
  template declares the complete desired state — `apiVersion`, `kind`, `metadata`, and the
  resource fields (spec, data, etc.). Deleted on prune.
- **`patch:`** — A subset of fields on a resource another actor manages (e.g., only status, or
  only labels). The controller applies exactly those fields via SSA and tracks them for cleanup.
  Each field has exactly one writer — a `template:` can delegate specific fields to a `patch:`,
  but two nodes cannot write the same field. On prune, the fields are released; the resource is
  never deleted.
- **`ref:`** — Reference a resource outside this graph. The controller reads it without managing
  it.
- **`watch:`** — Observe a collection of resources. The controller enters the matched resources
  into scope and re-reconciles when they change.
- **`def:`** — Computed values into scope. The node produces no Kubernetes resource.

```yaml
# def: — reusable naming values, no Kubernetes resource created
- id: naming
  def:
    prefix: ${spec.name + '-' + spec.env}

# template: — creates and manages a Deployment using the definition
- id: deploy
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: ${naming.prefix + '-deploy'}
    spec:
      replicas: 3
      selector:
        matchLabels:
          app: ${naming.prefix}
      template:
        metadata:
          labels:
            app: ${naming.prefix}
        spec:
          containers:
            - name: app
              image: nginx

# ref: — reads an existing WebApp into scope
- id: webapp
  ref:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: my-app

# patch: — writes status fields to the WebApp
- id: webappStatus
  patch:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: ${webapp.metadata.name}
      namespace: ${webapp.metadata.namespace}
    status:
      deploymentReady: ${deploy.status.availableReplicas == deploy.spec.replicas}

# watch: — discovers all Pods matching a selector
- id: appPods
  watch:
    apiVersion: v1
    kind: Pod
    selector:
      app: ${naming.prefix}
```

#### forEach

Expands a node once per item in an array. The forEach declaration is a template for children —
everything on it (template, readyWhen, propagateWhen, CEL functions) applies per-child. The forEach
node is a logical parent that aggregates child outputs. Each child is a real node that manages one
resource. Child identity is derived from the parent's ID combined with the rendered resource key
(GVK + namespace + name).

The parent is ready when all children are ready, and updated when all children are updated.

For `def:` nodes, forEach produces an array of values instead of managed resources — no children
are created.

After evaluation, the parent enters scope as an array of child outputs. Downstream nodes depend on
the parent, not individual children. The parent enters scope (enabling downstream evaluation) once
all children have applied successfully.

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

# forEach + def: computed container list embedded in a Deployment
- id: containers
  forEach:
    w: ${spec.workers}
  def:
    name: ${w}
    image: ${spec.appImage}
    args: ["--worker=${w}"]

- id: deployment
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: ${spec.name}
    spec:
      template:
        spec:
          containers: ${containers}
```

#### includeWhen

A list of CEL expressions. All must evaluate to `true` for the node to be included. If any condition
is false, the node is skipped — nothing is applied and it does not enter scope. Downstream nodes
that depend on it cannot evaluate (the data is not in scope) and are also Excluded.

```yaml
- id: ingress
  includeWhen:
    - ${config.data.enableIngress == "true"}
  template: ...
```

#### readyWhen

A node is ready when its CEL expressions resolve and its apply or read succeeds — no implicit status
conditions check is performed. This is the default behavior when readyWhen is absent.

readyWhen overrides this default with explicit CEL conditions. All expressions must evaluate to
`true` for the node to be considered ready. readyWhen is a health signal — it feeds the Graph's
aggregated status and tells operators whether the system has converged. It does not gate downstream
execution.

Each node exposes a `.ready()` CEL function. `.ready()` is not transitive — if you want to assert
dependency readiness, do so explicitly:

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...

# .ready() in propagateWhen — gate inputs until dependency converges
- id: consumer
  propagateWhen:
    - ${deployment.ready()}
  template: ...

# Explicit transitive readiness — assert each dependency
- id: output
  propagateWhen:
    - ${deployment.ready()}
    - ${database.ready()}
  template: ...
```

#### propagateWhen

A list of CEL expressions. All must evaluate to `true` for the node to evaluate. When unsatisfied,
the node skips evaluation — it retains its last-applied state and is not re-applied. When satisfied,
the node evaluates normally against dependency outputs only — the node's own data is not in scope. A
common pattern is `propagateWhen: [${dep.ready()}]` — gate on a dependency's readiness.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...

- id: service
  propagateWhen:
    - ${deployment.ready()}
  template:
    apiVersion: v1
    kind: Service
    metadata:
      name: ${deployment.metadata.name}-svc
    spec:
      selector: ${deployment.spec.selector.matchLabels}
```

With forEach, `.ready()` and `.updated()` can control how quickly changes propagate — the
expression references the parent collection to gate evaluation.

```yaml
# Exponential rollout — budget doubles each wave
- id: deploys
  forEach:
    app: ${apps}
  propagateWhen:
    - >-
      ${deploys.filter(d, d.updated() && !d.ready()).size()
       < max(1, deploys.filter(d, d.updated() && d.ready()).size())}
  template: ...

# Linear rollout — 2 at a time
- id: deploys
  forEach:
    app: ${apps}
  propagateWhen:
    - ${deploys.filter(d, d.updated() && !d.ready()).size() < 2}
  template: ...

# Gate on ALL dependencies
- id: service
  propagateWhen:
    - ${service.dependencies().all(d, d.ready())}
  template: ...
```

#### finalizes

A node `id`. The resource is created when the target becomes a prune candidate — it does not exist
during normal operation. It must reach readyWhen before the target's removal completes.

Finalizers fire regardless of why the target is being removed — Graph teardown, spec mutation,
includeWhen toggle, or forEach scale-down.

```yaml
- id: snapshot
  finalizes: pvc
  template:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshot
    metadata:
      name: ${pvc.metadata.name}-final
    spec:
      source:
        persistentVolumeClaimName: ${pvc.metadata.name}
  readyWhen:
    - ${snapshot.status.readyToUse == true}
```

## CEL Functions

Any object in scope exposes functions maintained by the graph controller.

- **`.ready()`** — true when the node is applied and its readyWhen conditions pass.
- **`.updated()`** — true when the node is on the latest graph generation.
- **`.dependencies()`** — returns the scope values of all dependency nodes as a list.
  Enables `${node.dependencies().all(d, d.ready())}` — gate until every dependency is ready without
  naming them.

## Dependencies

Dependencies are inferred from CEL expression references. If node B's template contains
`${A.metadata.name}`, B depends on A. A consumer re-evaluates when the specific fields it depends on
change — not on every change to the dependency.

The dependency graph must be acyclic — cycles are rejected at compile time. Nodes with no dependency
relationship are independent and processed in parallel. All dependencies are hard by default — the
consumer waits for each dependency to be in scope before evaluating.

## Lazy Evaluation

A node that depends on another waits for it before evaluating. Some expressions have a meaningful
value even when a dependency is absent — a status condition can report `Unknown` while the nodes it
depends on are still being resolved.

Lazy dependencies are optional values in the evaluation context. CEL's optional types handle absent
data natively — `?` for field access, `.orValue()` for defaults:

```yaml
- id: deployment
  template: ...

- id: appStatus
  patch:
    status:
      conditions:
        - type: DeploymentReady
          status: ${deployment.ready().orValue(false) ? 'True' : 'Unknown'}
          message: ${deployment.ready().orValue(false)
            ? 'Deployment available'
            : 'Waiting for deployment'}
      replicas: ${deployment.?status.?availableReplicas.orValue(0)}
```

`appStatus` evaluates immediately. While `deployment` is absent, `.ready().orValue(false)` returns
`false` — the condition reports `Unknown`, and `.?replicas.orValue(0)` returns `0`. When `deployment`
completes and becomes ready, the consumer re-evaluates and the condition flips to `True`.

A lazy dependency does not make the consumer wait — it evaluates when its hard dependencies are
satisfied, regardless of whether lazy dependencies are present. `.ready()` on a hard dep returns
concrete `bool`. `.ready()` on a lazy dep (optional receiver) returns `optional(bool)` — the user
chains `.orValue(false)` to unwrap. Field access uses CEL optional types:
`.?field.orValue(default)` returns the default when the dependency is absent. When a lazy dependency
later completes, the consumer re-evaluates.

The compiler infers which dependencies are lazy from the expression syntax — a dependency accessed
only through `?.`, `[?]`, or `.ready().orValue()` / `.updated().orValue()` is lazy; a dependency
accessed directly (including bare `.ready()` without `.orValue()`) is hard.

## Nested Graphs

A Graph whose template contains another Graph creates a nested scope. The inner Graph is a regular
Kubernetes object — it is created via the API server and reconciled independently by its own
reconciliation. Each level is a separate reconciliation loop with its own resource scope.

The combination of `watch:`, forEach, and nested Graphs creates per-instance controllers. A parent
Graph observes a kind via `watch:`, forEach creates one child Graph per item, and each child Graph
independently reconciles resources for its item.

```yaml
# Parent Graph — watches all Namespaces, creates a child Graph per Namespace
- id: namespaces
  watch:
    apiVersion: v1
    kind: Namespace
    selector: {}

- id: perNamespace
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: experimental.kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${ns.metadata.name}-resources
    spec:
      nodes:
        - id: nsRef
          ref:
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

Each controller does exactly one pass on its own spec. Expressions in resource fields (e.g.,
`${...}` strings stored in a resource's spec) survive as opaque strings — they are not re-scanned.
Nested evaluation is recursive through Kubernetes persistence, not through string re-scanning.

To pass an expression through to a child Graph, use `$${...}`. The controller strips one `$`,
producing `${...}` in the output. The child's controller evaluates the resulting expression against
its own scope.

Escaping depth by target level:

- `${...}` — evaluated by this Graph
- `$${...}` — evaluated by the child Graph (one level down)
- `$$${...}` — evaluated two levels down

## Status

The Graph's status is the controller's observation of the Graph's current state. Status conditions
exist to make the operator's mental model correct — they answer "is this Graph healthy, and if not,
who needs to act?"

Two failure domains require different responses: a user error is a Graph developer problem (fix the
spec, resolve conflicts, correct permissions); a system error is an operator problem (infrastructure
or server failures). The condition structure reflects this split.

### Conditions

`status.conditions` follows the standard Kubernetes condition convention — each condition has
`type`, `status`, `reason`, `message`, and `lastTransitionTime`. `lastTransitionTime` is preserved
when the status value does not change between reconciles.

The Graph defines two conditions on orthogonal axes:

**`Compiled`** — the spec is valid. CEL expressions compiled, the dependency graph is acyclic, and
node declarations are structurally correct. Set once when the spec is processed. Permanent until the
spec changes. A `False` Compiled condition means the Graph will never converge until the developer
fixes the spec.

| Reason             | Meaning                          |
| ------------------ | -------------------------------- |
| `Compiled`         | Spec is valid                    |
| `ExpressionError`  | CEL expression is invalid        |
| `DependencyError`  | Nodes form a circular dependency |
| `DeclarationError` | Node declaration is malformed    |

**`Ready`** — a rollup of node evaluations. Each reason maps to the node state blocking convergence.
`True` means converged. `Unknown` means converging — the controller is making progress and no
intervention is needed. `False` means stuck — something requires operator action.

Alarm on Ready `False` or `Unknown` persisting beyond a reasonable convergence window. Since
Compiled rolls up into Ready (as `NotCompiled`), a single alarm on the Ready condition covers both
developer and operator failure modes.

| Reason        | Status    | Node State  | Meaning                                        |
| ------------- | --------- | ----------- | ---------------------------------------------- |
| `Ready`       | `True`    | Ready       | All resources reconciled                       |
| `NotReady`    | `Unknown` | NotReady    | Applied but readyWhen conditions not met       |
| `Pending`     | `Unknown` | Pending     | Waiting for upstream data                      |
| `Blocked`     | `Unknown` | Blocked     | Dependency in error state, waiting for resolve |
| `NotCompiled` | `False`   | —           | Spec invalid; rollup of Compiled=False         |
| `Conflict`    | `False`   | Conflict    | SSA field ownership contested by another actor |
| `Error`       | `False`   | Error       | Client request failed (4xx)                    |
| `SystemError` | `False`   | SystemError | Server or infrastructure failure (5xx)         |

```yaml
status:
  conditions:
    - type: Compiled
      status: "True"
      reason: Compiled
      lastTransitionTime: "2025-01-15T10:29:00Z"
    - type: Ready
      status: "True"
      reason: Ready
      message: "All 3 resources reconciled"
      lastTransitionTime: "2025-01-15T10:30:00Z"
  topologicalOrder:
    nodes: ["schema", "deployment", "service", "statusPatch"]
```

### Topological Order

`status.topologicalOrder` is a map with the key `nodes` holding the Graph's own DAG in topological
order. When the Graph pre-compiles a forEach child Graph, the child's topology is stored as a
sibling key named after the forEach node:

```yaml
topologicalOrder:
  nodes: ["a", "b", "c"]
  b: ["x", "y", "z"]
```

The child's topology is available before any child Graph CR exists.

The Graph's status contains only controller-managed fields. There are no user-defined status
fields on the Graph itself. User-defined status (e.g., `deploymentReady`, `address`) lives on custom
resource types and is written via `patch:` nodes targeting the custom resource's status subresource.

## Owner References

The controller does not cascade-delete — teardown is ordered by the DAG. A Graph with
`metadata.ownerReferences` inherits standard K8s cascade: when any owner is Terminating, the
controller self-deletes the Graph. The resulting teardown runs the full ordered path — `finalizes`
sequences fire, prune walks the reverse DAG.

To hold the owner in Terminating during teardown, combine ownerReferences with a `patch:` node
that places a finalizer on the owner:

```yaml
metadata:
  ownerReferences:
    - apiVersion: kro.run/v1alpha1
      kind: WebApp
      name: my-instance
      uid: ...
spec:
  nodes:
    - id: lifecycle
      patch:
        apiVersion: kro.run/v1alpha1
        kind: WebApp
        metadata:
          name: my-instance
          finalizers:
            - experimental.kro.run/graph
    - id: deployment
      template: ...
```

The ownerReference triggers self-deletion. The patch holds the owner until teardown prunes it —
SSA releases the finalizer, the owner completes deletion.
