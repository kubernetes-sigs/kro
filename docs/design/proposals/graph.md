# KREP-024: Graph

> **Implementation status (as of v1alpha1):** This proposal document has been partially reconciled with the shipped implementation in `pkg/graphengine/` and `pkg/controller/graph/`. Key architectural differences from earlier drafts — including unified `ref:` for collections (superseding `watch:`), explicit `graph:` subgraph nodes, `PatchSpec` structure and constraints (`subresource`/`body`, no `forEach`), collection expansion caps (`MaxCollectionSize`), and the `Accepted`/`ResourcesConverged`/`Ready` condition model — are reflected below. Features marked as "Planned" or "Not yet implemented" (such as KREP-006 `propagateWhen` and lifecycle signals) remain future work. Remaining design claims may still drift.

## Summary

Graph is a new `kro.run/v1alpha1` Kind — the atomic primitive for composing Kubernetes resources. A
Graph is a set of nodes evaluated in topological order. Create it and its resources converge; delete
it and they cascade away. It is the simplest possible unit of composition in kro.

A Graph is analogous to a **scope** in traditional programming languages. Its nodes form a flat
namespace — private, visible only within the Graph — with spec as input and status as output.
Nothing inside a Graph is reachable from outside except through the Kubernetes resource boundary:
the Graph object's own spec and status fields. Graphs are reconciled independently and in parallel.

RGD today combines CRD management, instance management, and resource composition into one object.
Graph separates these — providing resource composition alone — making each concern independently
usable. This enables patterns RGD cannot express: static resource bundles (no CRD needed),
singletons (multiple contributors, one resource), and decorators (react to existing resources
without defining a new Kind). Higher-level abstractions like RGD can be built _from_ Graph; every
feature added to Graph automatically benefits every abstraction layered on top.

Graph was validated by building the full RGD system from it. A single Graph implements the RGD
controller — creating CRDs, watching instances, managing resources, writing status — all through
composition rather than imperative Go code (see [examples/graph/rgd.yaml](../../../examples/graph/rgd.yaml)).

Beyond the RGD proof, Graph enables patterns that are simpler than what RGD can express today:

- [Namespace decorator](../../../examples/graph/namespace-decorator.yaml) — watch namespaces, create
  NetworkPolicies. No CRD, no schema, no instance. (The decorator pattern from KREP-003.)
- [Ingress fan-in](../../../examples/graph/ingress-fanin.yaml) — aggregate Services into a single
  Ingress with dynamic routes. (The aggregated-resource pattern from KREP-003.)
- [CoreDNS installation](../../../examples/graph/coredns.yaml) — install CoreDNS as a Graph. The
  static-bundle pattern: dependency-ordered, health-aware, one object replaces a Helm chart.
- [Singleton](../../../examples/graph/singleton.yaml) — fan-in with priority-based resolution when
  multiple actors claim the same resource.

### What this KREP covers

**Proposed:** The Graph Kind — node types (`template`, `patch`, `ref`, `graph`, `def`), dependency
inference from CEL expressions, the evaluation model (nodes evaluate when hard dependencies are in scope,
not when they are ready), status conditions (`Accepted`, `ResourcesConverged`, `Ready`), and nested composition. These are
new primitives that do not exist in KRO today.

**Inherited unchanged from RGD:** `includeWhen`, `forEach`, `readyWhen`, and CEL expression syntax.
These mechanisms carry forward with the same semantics and are not redefined by this KREP.

**Defers to KREP-006 (Planned / Not yet implemented):** `propagateWhen` gating semantics, `.ready()` and `.updated()` lifecycle
signals, collection-level rollout strategies, and budget syntax. These features are deferred to KREP-006 and are not yet implemented in the engine.

**Relationship to RGD:** Graph is proposed as a sibling primitive. RGD continues to work unchanged.
We have the option to implement RGD's internals on top of Graph in the future, but for the immediate
term both implementations live as siblings sharing significant code in the underlying graph engine.
Graph is additive — it does not replace or deprecate RGD.

## Proposed API

### The Graph Kind

```yaml
apiVersion: kro.run/v1alpha1
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

A Graph is namespaced. Its `spec.nodes` is a list where each node has an `id` and exactly one type
keyword. Evaluation order is derived from dependencies, not list position. The
`${deployment.metadata.name}` expression in the Service creates a dependency edge: the Service
cannot evaluate until the Deployment has been applied and its observed state is available.

### Node Types

Graph has five node types: `template`, `ref`, `def`, `graph`, and `patch`. Each is declared by a keyword on the node object. We choose explicit
keywords over fewer types with strategy flags so that a reader knows what a node does — and what
type it produces in scope — at a glance from the top-level keyword. Exactly one type keyword must be set per node.

#### `template:`

Creates and owns a Kubernetes resource via server-side apply (using field manager `kro.run/applyset`). On deletion of the Graph, the resource
is deleted.

#### `patch:`

Contributes fields to a resource you don't own. The resource must already exist. The node applies the contributed `body` via server-side apply under a dedicated per-node field manager (`kro-graphengine.patch.<hash>`). On prune, your
contributed fields are released (relinquished by applying an empty object under that manager) — the target resource itself is not deleted.

`PatchSpec` declares `apiVersion`, `kind`, `metadata` (`name` and optional `namespace`), optional `subresource` (`""` or `"status"`), and `body`. Note that `forEach` is not supported on patch nodes.

```yaml
- id: instanceStatus
  patch:
    apiVersion: apps.example.com/v1
    kind: WebApp
    metadata:
      name: my-webapp
    subresource: status
    body:
      status:
        endpoint: ${service.status.loadBalancer.ingress[0].hostname}
        ready: ${deployment.status.availableReplicas > 0}
```

#### `ref:`

Reads an existing resource or collection of external resources into scope. No write, no ownership, no cleanup.

- **Single resource:** Specified with `metadata.name`. Enters scope as an object:

```yaml
- id: config
  ref:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-config
      namespace: default
```

- **Collection of resources:** Specified with `metadata.selector`. Enters scope as a list:

```yaml
- id: allPods
  ref:
    apiVersion: v1
    kind: Pod
    metadata:
      selector:
        matchLabels:
          app: my-app
```

Downstream nodes use standard CEL list operations:
`${allPods.filter(p, p.status.phase == 'Running').size()}`.

#### `graph:`

Nests an inline child Graph spec under the node's `id`. The child's nodes form a lexical scope frame: they may capture parent nodes, but an individual CEL expression cannot mix scopes.

#### `def:`

Pure computation — no Kubernetes resource created or read. The result enters scope under the node's
`id`. This is Graph's implementation of the variables concept proposed in KREP-011.

```yaml
- id: naming
  def:
    prefix: ${deployment.metadata.name + '-' + deployment.metadata.namespace}
    labels:
      app: ${deployment.metadata.labels['app']}
```

#### Dynamic GVKs

A node's `apiVersion`, `kind`, and `metadata.name` can be CEL expressions. This enables patterns
where the target resource type isn't known at author time:

```yaml
- id: watchInstances
  ref:
    apiVersion: ${crd.spec.group}/${crd.spec.versions[0].name}
    kind: ${crd.spec.names.kind}
    metadata:
      selector: {}
```

### Node Modifiers

Node modifiers provide conditional logic, health checking, and repetition. In the implementation, modifiers are strictly validated per node type:

- `forEach`: Supported on `template` and `def` nodes. Stamps one instance per element (or cartesian product across multiple dimensions). Explicitly rejected on `graph`, `patch`, and `ref` nodes.
- `includeWhen`: Supported on `template`, `ref`, `def`, and `patch` nodes. When false, the node is skipped (and template resources are pruned). Rejected on `graph` nodes.
- `readyWhen`: Supported on `template`, `ref`, `def`, and `patch` nodes. Evaluated against scope to determine whether the node is ready. Rejected on `graph` nodes.
- `propagateWhen`: Planned / Not yet implemented (deferred to KREP-006).

| Modifier        | Supported Node Types             | Question it answers       | When false                           | Status / Defined in  |
| --------------- | -------------------------------- | ------------------------- | ------------------------------------ | -------------------- |
| `includeWhen`   | `template`, `ref`, `def`, `patch`| Should this node exist?   | Prune — resource deleted / skipped   | Shipped (KREP-008)   |
| `readyWhen`     | `template`, `ref`, `def`, `patch`| Is this node healthy?     | Signal only — Graph not Ready        | Shipped (from RGD)   |
| `forEach`       | `template`, `def`                | How many instances?       | N/A (expands node)                   | Shipped (KREP-002)   |
| `propagateWhen` | (Planned)                        | May this node mutate now? | Freeze — last-applied state persists | Planned (KREP-006)   |

#### Collection Expansion Caps and Safety

To prevent unbounded resource creation and runaway memory consumption, the runtime enforces collection limits:

- **Max Collection Size:** A single `forEach` expansion is capped at `DefaultMaxCollectionSize = 1000` instances. Expansions exceeding this limit fail runtime evaluation.
- **Max Dimensions:** Up to `DefaultMaxCollectionDimensions = 10` forEach axes may be declared.
- **Empty Dimensions:** If any dimension evaluates to an empty list, the cartesian product short-circuits to empty (0 instances).
- **Identity Uniqueness:** The runtime validates that each expanded resource produces a unique `(Group, Version, Kind, Namespace, Name)` tuple; duplicate identities are rejected with an error.

### Dependencies

Dependencies are inferred from CEL expressions. If node B's template contains `${A.field}`, B
depends on A. The compiler builds a DAG from these references and computes topological order. Cycles
are compile-time errors.

Each node's observed state enters scope under its `id`. Downstream nodes
reference these scope entries via CEL.

#### Hard Dependencies

A bare field reference creates a hard dependency:

```cel
${deployment.status.availableReplicas}
```

Node B cannot evaluate until node A has been applied and its observed state is in scope. This is the
standard behavior — identical to today's RGD.

#### Soft Dependencies

Soft dependencies are **not** inferred from CEL expression syntax. Optional chaining
(`${deployment.?status.?loadBalancer}`) creates an ordinary **hard** dependency, identical to a bare
reference: the referencing node still waits until the target has been applied and its observed state
is in scope. This is deliberate — inferring soft (non-gating) edges from `?.` previously let a node
render and apply against a not-yet-created dependency (materializing `.orValue(...)` fallbacks
prematurely), so expression-level optional chaining no longer weakens dependency ordering.

A dependency is classified as soft only when the compiler is explicitly told to treat a node's
references as soft, via the internal `WithSoftDependencies` compile option. A soft dependency creates
no DAG ordering edge; the target is seeded with an empty object in scope so an optional access
resolves to `optional.none()` and, for a bare optional field (`${id.?field}`), the field is omitted
from the rendered payload rather than serialized as `null`. `.orValue(omit())` (the CEL `omit()`
sentinel, gated behind the `CELOmitFunction` feature) and `.orValue(<fallback>)` remain available for
explicit control.

**Why soft dependencies exist:** Status writeback creates a dependency cycle: a status `patch:` node
references all managed resources, but can't hard-depend on all of them without blocking itself. The
synthesized status-writeback node is therefore compiled with `WithSoftDependencies` (and per-field
data-pending tolerance) so it runs on every pass, filling in whatever is available — this explicit
opt-in is the _only_ source of soft dependencies; it is never inferred from `?.` in user templates.

#### Evaluation Model and Compilation Cache

Reconciliation proceeds in topological order derived from hard dependency edges:

1. **Compilation & Cache:** Compilation is cached in an in-memory `Registry` keyed by the Graph's `(namespace, name)` and validated against an FNV-64a hash of the normalized `GraphSpec`. A `SchemaWatcher` tracks the CRD GroupKinds required by the Graph (including dynamic GVK subscriptions); when a tracked CRD changes, the cache is invalidated and the Graph is re-enqueued for compilation against fresh schemas.
2. **Evaluation & Scope:** A node evaluates as soon as its hard dependencies are in scope — meaning they have been applied and their observed state is available from the cluster. Nodes do not wait for upstream dependencies to pass `readyWhen`.
3. **Application & Field Management:**
   - `template:` nodes apply their desired manifest via Server-Side Apply (SSA) using the shared field manager `kro.run/applyset` with force ownership.
   - `patch:` nodes apply contributed fields under a dedicated per-node field manager (`kro-graphengine.patch.<hash>`). On delete or prune, releasing the contribution applies an empty object under that manager to relinquish field ownership without deleting the target object.
4. **Reconciliation Parallelism:** Within a single Graph, nodes evaluate serially in topological order; collection instances evaluate in bounded parallel (default ApplyConcurrency=20). Across Graphs, reconciliation is fully parallel via controller-runtime's work queue.

_(Note: `propagateWhen` gating and `.ready()` lifecycle signals are deferred to KREP-006 and not yet implemented in the engine.)_

### Nested Graphs

Graph supports two forms of nesting:

1. **Inline Subgraphs (`graph:` node):** An explicit `graph:` node embeds a child `GraphSpec` inline. The compiler compiles this into a child `SubProgram` frame with lexical scoping. Ancestor node references are captured as dependencies of the subgraph node, while expressions inside the subgraph cannot mix frames.
2. **Stamping Graph Custom Resources (`template:` node with `kind: Graph`):** A parent Graph can stamp child `Graph` custom resources into the cluster (for example, combined with `forEach`). The child Graph is applied as an independent Kubernetes object and reconciled asynchronously by the Graph controller.

#### Deferral Boundaries for Stamped Graphs

When stamping child Graph resources via a `template:` node, child CEL expressions live as literal strings inside the parent's template:

- `${...}` — evaluated by the current (parent) Graph
- `${'${...}'}` — a CEL string literal; the parent evaluates it to produce the text `${...}`, which
  the child Graph then evaluates at its own scope

```yaml
# Parent Graph (L0) evaluates this — bakes the RGD name into the child spec:
name: ${rgd.metadata.name}

# Parent produces literal "${rgd.spec.schema.group}" — child Graph (L1) evaluates it:
group: ${'${rgd.spec.schema.group}'}
```

This composes to arbitrary depth. Each layer evaluates one string literal, peeling off one level of quoting.

## Expressing RGD as Graph

The RGD system is three levels of nested Graphs:

```
L0: rgd-controller
├── Creates the ResourceGraphDefinition CRD
├── ref: reads all RGD objects (via selector)
└── forEach RGD → creates L1 Graph
    │
    L1: per-RGD (one per ResourceGraphDefinition)
    ├── ref: reads the specific RGD object
    ├── Creates the user's Kind CRD from RGD schema
    ├── ref: reads all instances of the user's Kind (via selector)
    └── forEach instance → creates L2 Graph
        │
        L2: per-instance (one per user CR)
        ├── ref: reads the specific instance
        ├── User's resources (from rgd.spec.resources)
        └── patch: writes status back to the instance
```

See [examples/graph/rgd.yaml](../../../examples/graph/rgd.yaml) for the full working implementation
with detailed commentary on each level.

## Status

A Graph object exposes three standard conditions managed by the controller:

- **`Accepted`** (`kro.run/v1alpha1` `GraphConditionTypeAccepted`): Reports whether the Graph specification passed validation and compilation (unique alphanumeric node IDs, valid CEL expressions, acyclic DAG).
  - Status `True` with reason `Compiled` and message `compiled <N> nodes`.
  - Status `False` with reason `InvalidGraph` and the compilation error message.
- **`ResourcesConverged`**: Reports the executor's terminal apply and readiness state.
  - Status `True` with reason `Applied` ("all nodes applied and ready").
  - Status `False` with reason `WaitingForReadiness` when apply succeeded but `readyWhen` expressions evaluate false.
  - Status `False` with reason `DataPending` when a node's CEL expression references data the cluster has not surfaced yet (e.g. pending status fields).
  - Status `False` with reason `ApplyFailed` when the executor encounters a hard error applying resources.
- **`Ready`** (`kro.run/v1alpha1` `GraphConditionTypeReady`): Root aggregate condition rolled up from `Accepted` and `ResourcesConverged`.
  - Status `True` when both `Accepted` and `ResourcesConverged` are `True`.
  - Status `False` when either condition is `False`.
  - Status `Unknown` while compilation or resource convergence is in progress.

| Condition Type       | Status  | Reason               | Meaning                                      |
| -------------------- | ------- | -------------------- | -------------------------------------------- |
| `Accepted`           | True    | `Compiled`           | Graph spec is valid and compiled             |
| `Accepted`           | False   | `InvalidGraph`       | Spec validation or compilation error         |
| `ResourcesConverged` | True    | `Applied`            | All nodes applied and ready                  |
| `ResourcesConverged` | False   | `WaitingForReadiness`| Applied, but readyWhen conditions not yet met|
| `ResourcesConverged` | False   | `DataPending`        | Waiting for upstream cluster data in scope   |
| `ResourcesConverged` | False   | `ApplyFailed`        | Hard failure during resource apply           |
| `Ready`              | True    | `Ready`              | All dependent conditions True (graph ready)  |
| `Ready`              | False   | _(from dependent)_   | Spec invalid or apply failed                 |
| `Ready`              | Unknown | _(from dependent)_   | Still reconciling or waiting on readiness    |

User-defined status does not live on the Graph object. It lives on custom resources and is written
via `patch:` nodes. The Graph's status contains only controller-managed conditions.

## Security Posture

Unlike cluster-scoped `ResourceGraphDefinition`, `Graph` introduces a new namespaced, user-creatable
kind whose executor performs privileged actions (cross-namespace writes, cluster-scoped RBAC
creation, foreign Secret reads, and prune) using the KRO controller's service account permissions.

Because of this privileged execution model, `Graph` must **not** be aggregated into the built-in
Kubernetes user roles (`edit`, `admin`, `view`). Instead, access to create or manage `Graph`
resources must be explicitly granted via separate, dedicated roles and is gated behind the
`GraphKind` feature gate (see the Helm `user-cluster-role.yaml` configuration).

A known limitation and follow-up is that a Graph's blast radius is not yet namespace-confined.
Future work on credential scoping (such as caller credentials, short-lived tokens, and per-Graph
service accounts) will provide finer-grained isolation and namespace confinement across KRO
primitives.

## Relationship to Existing KREPs

| KREP                            | Relationship                                                                                                                                                             |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| KREP-001 (Status Conditions)    | System conditions (`Accepted`, `ResourcesConverged`, `Ready`) exist on Graph objects — never on user resources. Users define their own status via `patch:` nodes.       |
| KREP-002 (Collections)          | Adopted with safety limits (`MaxCollectionSize = 1000`, `MaxCollectionDimensions = 10`). Supported on `template` and `def` nodes.                                      |
| KREP-003 (Decorators)           | A Decorator is naturally a Graph with `ref:` (selector) + `forEach`. No special runtime support needed.                                                                  |
| KREP-006 (Propagation Control)  | Planned / Not yet implemented: `propagateWhen` gating and lifecycle signals (`.ready()`, `.updated()`) are deferred to KREP-006 and not yet implemented in the engine. |
| KREP-008 (includeWhen)          | Graph implements `includeWhen` as a first-class modifier across `template`, `ref`, `def`, and `patch` nodes. Dependency inference works naturally.                      |
| KREP-011 (Variables)            | `def:` is Graph's implementation. Same semantics.                                                                                                                        |
| KREP-013 (Graph Revisions)      | Applies unchanged. Each nested Graph gets independent revisions.                                                                                                         |
| KREP-014 (Resource Lifecycles)  | Graph's node types implicitly define lifecycle: `template:` = delete-on-prune, `patch:` = release-fields-on-prune. Per-node lifecycle policies are a natural follow-on.  |
| KREP-018 (Partial Dependencies) | All CEL references (including `?.`) infer hard dependencies by default. Soft dependencies are declared explicitly via `WithSoftDependencies` (e.g. for status writeback). |
| KREP-019 (Deferred Fields)      | Conditional field omission via `omit()` is available when gated by `CELOmitFunction`; references in expressions still establish hard dependency edges.                  |

## Future Work

Graph was validated through an extensive prototyping effort (Krocodile) that explored how far the
primitive extends. Beyond the core API proposed here, the prototype implements:

- **`Kind`** — a simplified RGD with graph-like semantics. Defines a new Kubernetes Kind (CRD +
  per-instance Graphs) in a single object. Kind demonstrates how a higher-level abstraction composes
  Graph primitives.
- **Propagation control** — rate-limited rollouts, time-based gates, reactive controls (KREP-006
  covers the design; the prototype validates it composes with nested Graphs)
- **Prometheus metric emission** (`metric:`) — emit gauges driven by CEL expressions
- **Finalizer coordination** (`finalizes:`) — cross-resource cleanup ordering
- **Time-based scheduling** — `time.now()` evaluated once per reconcile pass; the system solves for
  the exact requeue deadline rather than polling
- **Cryptographic CEL functions** — `rsa.generateKey`, `x509.createCertificateRequest`, etc. enable
  TLS bootstrapping inline in a Graph

These features compose naturally with Graph but are separate design concerns, proposable
incrementally. The key finding from Krocodile is that Graph's recursive structure means each feature
added at one level automatically applies at every level of nesting. Nothing is special-cased per
layer.
