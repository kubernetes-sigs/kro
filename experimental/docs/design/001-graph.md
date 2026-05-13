# Graph

A Graph is kro's primitive for composing Kubernetes resources. Graphs define a scope of nodes that
can reference other nodes using [CEL expressions](https://cel.dev/). The graph manages the lifecycle
of its nodes, including the dependencies between them. Create a Graph and its nodes are created in
their topological order. Mutate it, and the graph converges on the newly desired state. Delete it
and nodes are pruned in the reverse order.

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

## Spec

### Nodes

`spec.nodes` is a list of nodes. The order of the nodes does not matter — evaluation order is
determined by the dependencies between nodes.

#### id

A string that identifies the node within the Graph's scope. Other nodes can reference this id CEL
expressions. Must be an alphanumeric string (case-insensitive) and unique within the Graph's scope.

#### type

A node's type is the keyword it declares. Six types exist:

- **`template:`** — Template a Kubernetes resource. The controller creates the resource if it
  doesn't exist, applies changes when the template changes, and deletes the resource on prune.
- **`patch:`** — Patch fields on an existing resource. Each field has exactly one writer —
  ownership conflicts are detected and surfaced before the write proceeds. A patch may apply fields
  to a resource in another graph. On prune, the fields are released from the resource.
- **`ref:`** — Reference a resource outside of this graph and make its fields available to other
  nodes in this graph.
- **`watch:`** — Watch all resources of a GroupKind matching a label selector and make their fields
  available to other nodes in this graph.
- **`def:`** — Define raw data for use by other nodes in this graph. Def nodes do not read or write
  Kubernetes resources.
- **`metric:`** — Emit a prometheus gauge driven by CEL. Value is an explicit CEL expression that
  evaluates to a number. Labels are direct CEL expressions evaluated in scope. Metric names are
  unique across Graphs. Propagation-driven — re-evaluates when upstream dependencies change. Does
  not publish to scope.

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

# metric: — emits a prometheus gauge with an explicit value
- id: podCount
  metric:
    type: gauge
    name: pod_count
    help: Total number of pods matching the app selector.
    value: ${size(appPods)}

# metric: — with forEach, emits a gauge per dimension
- id: podsByPhase
  forEach:
    phase: ${appPods.map(p, p.status.phase).distinct()}
  metric:
    type: gauge
    name: pods_by_phase
    labels:
      phase: ${phase}
    value: ${size(appPods.filter(p, p.status.phase == phase))}
```

## Dependencies

Dependencies between nodes are defined by CEL expressions. If node B's template contains
`${A.metadata.name}`, B has a dependency on A. Each CEL expression creates an edge in the graph. A
node cannot be evaluated until all of its edges can been evaluated.

### Soft Dependencies

Nodes can have soft dependencies on other nodes by defining CEL expressions with
[Optional Types](https://pkg.go.dev/github.com/google/cel-go/cel#OptionalTypes) (`?`, `.orValue()`).
A node's expression uses the data of another node if its available, but defines alternative values
if it's not.

```yaml
- id: deployment
  template: ...

- id: appStatus
  patch:
    apiVersion: kro.run/v1alpha1
    kind: WebApp
    metadata:
      name: my-app
    status:
      replicas: ${deployment.?status.?availableReplicas.orValue(0)}
      endpoint: ${service.?status.?loadBalancer.?ingress[0].?hostname.orValue('pending')}
```

`appStatus` evaluates immediately. While `deployment` is absent, `.?availableReplicas.orValue(0)`
returns `0`. While `service` has no load balancer, `.?hostname.orValue('pending')` returns
`'pending'`. When both resolve, the patch reflects the live values.

### CEL Functions

Nodes expose functions that enable other nodes to reason about their state.

- **`.ready()`** — true when the node is applied and its `readyWhen` conditions pass.
- **`.updated()`** — true when the node has been evaluated against the latest graph generation.
- **`.dependencies()`** — a list of hard and soft dependencies of the node. Useful to chain
  dependency readiness `readyWhen: [${node.dependencies().all(d, d.ready())}]`.
- **`time.now()`** — the current wall clock as a CEL-native `timestamp`. When it appears in a
  comparison, kro solves for the moment the comparison becomes true and enqueues reconciliation.
- **`.condition(type, status, reason, message)`** — constructs a Kubernetes status condition.
  Reads the scope entry's `.status.conditions` to find the existing condition by type. Preserves
  `lastTransitionTime` when status is unchanged; stamps `time.now()` on transition. Sets
  `observedGeneration` from `.metadata.generation`.
- **`plural(s)`** — English pluralization. Returns the plural form of the input string. Example:
  `plural("WebApp").lowerAscii()` → `"webapps"`.
- **`simpleSchema.toOpenAPI(schema, resources)`** — Converts a SimpleSchema map and resource list
  into a fully-structured OpenAPI v3 schema object.

## Modifiers

Modifiers modify the behavior of a node. They use CEL expressions that can reference other nodes.

### ForEach

The forEach modifier is a key-value pair that repeats a node for each value in a list CEL
expression. These child nodes are a set of logical nodes that depend on the parent. The forEach key
can be referenced in the node's CEL expressions. Additional node modifiers apply to the child nodes,
not the parent. The parent's state is a rollup of its children. Its `.ready()` is true when all children are ready, and `.updated()` is true when all children are updated.
Each child's identity is derived from the parent's ID and the child's GVK, Namespace, and
Name. Other nodes in the graph see the forEach node as a list of children when referenced in CEL.

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

### includeWhen

The `includeWhen` modifier is a list of boolean CEL expressions. When all are true, the node is
included in the graph, else it is skipped and its resource becomes a prune candidate. Nodes that
depend on excluded nodes are also excluded.

```yaml
- id: ingress
  includeWhen:
    - ${config.data.enableIngress == "true"}
  template: ...
```

### readyWhen

The `readyWhen` modifier is a list of boolean CEL expressions. When all are true, the node is ready.
If readyWhen is not defined, the node is ready as soon as it is evaluated. readyWhen is a health
signal — it does not gate dependents. Dependents proceed regardless of readyWhen. Use propagateWhen
to gate dependents on readiness. The graph is considered ready once all of its nodes are ready.

Each node exposes its readiness through a `.ready()` CEL function as a convenience for other nodes.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
  template: ...
```

### propagateWhen

The `propagateWhen` modifier is a list of boolean CEL expressions. When unsatisfied, the node and
its dependents are not re-evaluated. When satisfied, the node evaluates normally.

It's common to leverage readyWhen and propagateWhen to control evaluation between nodes.

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

It's common to combine forEach with propagateWhen to control how quickly changes happen in parallel
within a graph. Each child's propagateWhen counts it siblings to determine whether or not it should
propagate. The forEach evaluates serially and retains the stable order of inputs to avoid races.

```yaml
# Exponential rollout — budget doubles each wave
- id: deployments
  forEach:
    app: ${apps}
  propagateWhen:
    - >-
      ${deployments.filter(d, d.updated() && !d.ready()).size()
       < max(1, deployments.filter(d, d.updated() && d.ready()).size())}
  template: ...
```

```yaml
# Linear rollout — 2 at a time
- id: deployments
  forEach:
    app: ${apps}
  propagateWhen:
    - ${deployments.filter(d, d.updated() && !d.ready()).size() < 2}
  template: ...
```

### finalizes

The finalizes modifier causes a node to enter the graph when its target node becomes a prune
candidate, and does not exist otherwise. The target cannot be pruned until its finalizer is
evaluated. Finalizers trigger regardless of why the target is being pruned -- graph deletion, graph
mutation, includeWhen, or forEach.

It's common to combine `finalizes` with `readyWhen` to coordinate graceful removal of resources.

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

## Time

`time.now()` returns the current wall clock as a CEL-native `timestamp`. All duration arithmetic
uses standard CEL operators (`timestamp - timestamp → duration`, `timestamp + duration →
timestamp`). When `time.now()` appears in a comparison, kro solves for the moment that comparison
becomes true and enqueues reconciliation for exactly that time. The comparison operator is what gives
kro something to solve — it works wherever the comparison appears: gate expressions, ternaries,
value expressions.

```yaml
# Wait 2 hours after staging is ready before rolling out production.
- id: staging
  readyWhen:
    - ${staging.status.availableReplicas == staging.spec.replicas}
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-app-staging
      namespace: us-west-2
    spec:
      replicas: 2
      # ...

- id: production
  propagateWhen:
    - ${staging.ready()}
    - >-
      ${time.now() - timestamp(staging.status.conditions.filter(
        c, c.type == 'Available' && c.status == 'True'
      )[0].lastTransitionTime) >= duration('2h')}
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-app
      namespace: us-east-1
    spec:
      replicas: 10
      # ...
```

Kro solves `time.now() - lastTransitionTime >= 2h` → enqueue at `lastTransitionTime + 2h`. Two
hours after staging becomes ready, one reconciliation fires and `production` proceeds.

Without a comparison, `time.now()` is a raw value — nothing to solve, no enqueue. Writing
`time.now()` directly onto a resource produces a new value on every reconciliation. Use
`.condition()` for status timestamps — it settles by preserving `lastTransitionTime` when status is
unchanged. Raw `time.now()` in a write path without a settling mechanism is the user's
responsibility to manage.

### Status Conditions

`.condition(type, status, reason, message)` constructs a Kubernetes status condition with correct
`lastTransitionTime` semantics. Called on a scope entry that carries `.status.conditions` and
`.metadata.generation`:

```yaml
- id: instanceStatus
  patch:
    apiVersion: myapp.example.com/v1
    kind: MyApp
    metadata:
      name: ${schema.metadata.name}
      namespace: ${schema.metadata.namespace}
    status:
      conditions: ${[
        schema.condition('Ready',
          deployment.ready() && service.ready() ? 'True' : 'False',
          'Ready', 'All resources reconciled'),
        schema.condition('DatabaseReady',
          db.ready() ? 'True' : 'False',
          'Connected', 'Database connection established'),
      ]}
```

The function is equivalent to:

```cel
{
  "type": type,
  "status": status,
  "reason": reason,
  "message": message,
  "observedGeneration": schema.metadata.generation,
  "lastTransitionTime": schema.status.conditions.exists(c, c.type == type)
    ? (schema.status.conditions.filter(c, c.type == type)[0].status == status
        ? schema.status.conditions.filter(c, c.type == type)[0].lastTransitionTime
        : time.now())
    : time.now()
}
```

`lastTransitionTime` is preserved when status is unchanged. `time.now()` only stamps on actual
transitions.

### Why Not

**`tick(d)` as an explicit scheduling function.** Exposes scheduling mechanics to the user —
polling intervals to tune, side effects to reason about. A comparison involving `time.now()` gives
kro enough information to solve for the exact enqueue time.

**Automatic condition management by the runtime.** Users should decide what conditions exist, what
they mean, and when they transition. `time.now()` provides the clock, `.condition()` provides the
sugar, `patch:` writes the result — no implicit conditions created by the system.

## Nested Graphs

A node can define a Graph as its template to create a nested graph. The nested graph is a Kubernetes
object applied like any other template node. This relationship causes the nested graph to evaluate
independently from the parent graph. Like any other node, the parent can define fields of a nested
graph via CEL expressions, which are templated when the object is applied to the Kubernetes API.
However, the child graph executes independently from the parent -- the child's nodes cannot
reference the parent's, and vice versa.

When a graph is evaluated, each CEL expression `${...}` is evaluated into a concrete value. However,
child graphs need to define their own expressions separate from the parent graph's scope. CEL
expressions can be escaped using `$${...}` -- syntactic sugar for the string literal equivalent
`'{}'`. When a node is evaluated, the outer `$` is removed, and the inner `${...}` is written as a
string to the Kubernetes API in the child Graph's spec. The parent graph treats the child graph
purely as data during its evaluation, and does not evaluate the child graph's nodes.

It's common to combine nested graphs and CEL escapting with `watch`, `forEach`, `ref`, to create a
nested scope that evaluates in isolation. Below, the parent graph intentionally does not directly
reference parent's forEach `ns`, except by name, as any change to `ns` would cause the nested graph
to be mutated. Instead, the nested graph is configured to directly refence the `ns` itself within
its own scope.

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

## Status

The Graph's status exposes the the Graph's current state. Status conditions exist to make the
operator's mental model correct — they answer "is this Graph healthy, and if not, who needs to act?"

Two failure domains require different responses: a user error is a Graph developer problem (fix the
spec, resolve conflicts, correct permissions); a system error is an operator problem (infrastructure
or server failures). The condition structure reflects this split.

### Conditions

`status.conditions` follows the standard Kubernetes condition convention — each condition has
`type`, `status`, `reason`, `message`, `lastTransitionTime`, and `observedGeneration`.
`lastTransitionTime` is preserved when the status value does not change between reconciles.
`observedGeneration` is the `metadata.generation` the condition was last evaluated against — each
condition advances independently.

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
      observedGeneration: 1
      lastTransitionTime: "2025-01-15T10:29:00Z"
    - type: Ready
      status: "True"
      reason: Ready
      message: "All 3 resources reconciled"
      observedGeneration: 1
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

The Graph's status contains only controller-managed fields. There are no user-defined status fields
on the Graph itself. User-defined status (e.g., `deploymentReady`, `address`) lives on custom
resource types and is written via `patch:` nodes targeting the custom resource's status subresource.

## Owner References

The controller does not cascade-delete — teardown is ordered by the DAG. A Graph with
`metadata.ownerReferences` inherits standard K8s cascade: when any owner is Terminating, the
controller self-deletes the Graph. The resulting teardown runs the full ordered path — `finalizes`
sequences fire, prune walks the reverse DAG.

To hold the owner in Terminating during teardown, combine ownerReferences with a `patch:` node that
places a finalizer on the owner:

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

The ownerReference triggers self-deletion. The patch holds the owner until teardown prunes it — SSA
releases the finalizer, the owner completes deletion.

## Why Not

**`observed` as a user-visible scope variable.** Ref nodes already provide live resource state in
scope. The scope entry _is_ the observed state — adding a parallel variable duplicates data under a
different name.

**`immutable()` as a CEL function.** Write constraints (preventing field mutation after creation)
are a policy concern, not a value expression. CEL expressions produce values; they do not constrain
what values are acceptable.
