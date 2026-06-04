# KREP-024: Graph

## Summary

Graph is a new `kro.run/v1alpha1` Kind — the atomic runtime primitive for composing Kubernetes
resources. A Graph is a set of nodes evaluated in topological order. Create it and its resources
converge; delete it and they cascade away. It is the simplest possible unit of composition in kro.

ResourceGraphDefinition is a molecule built from this atom. RGD bundles together Kind definition,
instance management, and resource composition into a single convenient abstraction. Graph separates
these concerns, making each independently usable and enabling patterns that RGD cannot express:
static resource bundles (no CRD needed), singletons (multiple contributors, one resource), and
decorators (react to existing resources without defining a new Kind).

Graph was validated by building the full RGD system from it. A single Graph implements the RGD
controller — creating CRDs, watching instances, managing resources, writing status — all through
composition rather than imperative Go code. This implementation passes 23 of 36 upstream kro
compatibility test files, with remaining gaps due to prototype immaturity rather than structural
limitations (see [examples/graph/rgd.yaml](../../../examples/graph/rgd.yaml)).

## Motivation

RGD conflates three concerns:

1. **Kind definition** — creating a CRD with a schema
2. **Instance management** — watching instances and reconciling them
3. **Resource composition** — evaluating templates, managing dependencies

These are separable. An operator may want to compose resources without defining a new Kind (a static
bundle, like a Helm release). A platform may want to watch existing resources and react to them
without owning a CRD (a decorator). A system may want multiple actors to coordinate ownership of a
single resource (a singleton).

Graph provides (3) alone. Higher-level abstractions like RGD combine Graph with (1) and (2) — but
they are built _from_ Graph, not alongside it. This means every feature added to Graph automatically
benefits every abstraction layered on top.

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

A Graph is namespaced. Its `spec.nodes` is an ordered list where each node has an `id` and exactly
one type keyword. The `${deployment.metadata.name}` expression in the Service creates a dependency
edge: the Service cannot evaluate until the Deployment has been applied and its observed state is
available.

### Node Types

Graph has five node types. Each is declared by a keyword on the node object.

#### `template:`

Creates and manages a Kubernetes resource. On creation the resource is applied via server-side
apply. On deletion of the Graph, the resource is deleted. This is the direct equivalent of a
`template:` resource in today's RGD.

#### `patch:`

Contributes fields to a resource you don't own. The resource must already exist — `patch:` does not
create it. Fields are applied via server-side apply with a distinct field manager. On prune, the
field claims are released (the field manager is removed), but the resource is not deleted.

Common patterns: writing status back to a user-created instance, multiple writers to the same object
(like HPA scaling a Deployment alongside a Graph managing its template), or any case where you
express a partial claim on an existing resource.

```yaml
- id: instanceStatus
  patch:
    apiVersion: apps.example.com/v1
    kind: WebApp
    metadata:
      name: my-webapp
    status:
      endpoint: ${service.status.loadBalancer.ingress[0].hostname}
      ready: ${deployment.status.availableReplicas > 0}
```

#### `ref:`

Reads a single named resource into scope. No write, no ownership, no cleanup. The resource is
identified by apiVersion, kind, and metadata (name/namespace). It enters scope as an object —
downstream nodes can reference its fields via CEL.

```yaml
- id: config
  ref:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-config
      namespace: default
```

#### `watch:`

Reads all resources of a GroupVersionKind matching a label selector into scope as a list. No write,
no ownership, no cleanup.

```yaml
- id: allPods
  watch:
    apiVersion: v1
    kind: Pod
    selector:
      app: my-app
```

Downstream nodes use standard CEL list operations:
`${allPods.filter(p, p.status.phase == 'Running').size()}`.

#### `def:`

Defines raw data for use by other nodes. No Kubernetes resource is created or read — `def:` is pure
computation. The result enters scope under the node's `id` like any other node.

```yaml
- id: naming
  def:
    prefix: ${deployment.metadata.name + '-' + deployment.metadata.namespace}
    labels:
      app: ${deployment.metadata.labels['app']}
```

`def:` is Graph's implementation of the variables concept proposed in KREP-011. It satisfies the same
requirements — named intermediate computations, deduplication of repeated expressions, and composing
with `forEach` to build computed lists (e.g., a container list assembled from a spec field).

#### `includeWhen` and `forEach`

Graph retains `includeWhen` and `forEach` as node-level modifiers with the same semantics as in
today's RGD. `includeWhen` conditionally excludes a node (making it a prune candidate when false).
`forEach` stamps a node once per item in a collection. These are not redefined here — see KREP-008
and KREP-002 respectively.

#### Why ref and watch replace externalRef

`externalRef` in today's RGD overloads a single field to mean "read one named resource" and "watch a
collection by selector." `ref:` and `watch:` make the operation — and the scope type (object vs
list) — visible at a glance from the top-level keyword.

#### Everything is a CEL expression

Every string field in a node body — including `apiVersion`, `kind`, `metadata.name`, and selector
values — can be a CEL expression. This enables dynamic GVKs:

```yaml
- id: watchInstances
  watch:
    apiVersion: ${${schema.spec.schema.group}}/${${schema.spec.schema.apiVersion}}
    kind: ${${schema.spec.schema.kind}}
    selector: {}
```

When the compiler encounters a dynamic GVK, it uses a deferred-type path: the node compiles
permissively (untyped) until the first reconcile resolves the concrete GVK. This is what makes the
RGD-from-Graph pattern possible — the user's Kind isn't known at compile time.

### Observed State

Before evaluating a node's expressions, kro GETs the target resource from the API server. The live
object — including `metadata` and `status` — enters scope under the node's `id`. This is the
observed state: what exists before the node acts.

On first create (GET returns 404), the scope entry is an empty map. Expressions use optional
chaining (`.?`, `.orValue()`) to provide defaults. After apply, the apply response replaces the
scope entry — downstream nodes see post-apply state, not the pre-apply observation.

**Self-reference:** A node can reference its own `id` in its expressions to read its current state.
This enables patterns that require continuity across reconciles:

- **Condition transitions** — read existing `lastTransitionTime` and preserve it when status hasn't
  changed, stamp `time.now()` only on actual transitions
- **Latching** — don't regenerate a private key if one already exists in the Secret
- **Generation awareness** — compare `metadata.generation` against `status.observedGeneration` to
  detect whether a resource has processed its latest spec

### readyWhen

`readyWhen` is a list of CEL boolean expressions evaluated against a node's observed state. When all
are true, the node is considered _ready_.

```yaml
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas == deployment.spec.replicas}
  template:
    apiVersion: apps/v1
    kind: Deployment
    ...
```

**readyWhen is a health signal. It does not gate downstream nodes.**

This is an important behavioral difference from today's RGD. In the current implementation, a
downstream node cannot evaluate until all its dependencies pass `readyWhen`. In Graph, evaluation
proceeds as soon as dependencies are _in scope_ (have been applied and their observed state is
available). `readyWhen` determines:

1. Whether `.ready()` returns true for this node in CEL expressions
2. Whether the **Graph itself** is Ready — the Graph's Ready condition is the conjunction of all
   nodes' `readyWhen` results

This is analogous to how Helm determines release readiness: the release is "ready" when all its
resources pass their health checks, but resources are not blocked from being created by the health
state of other resources.

### propagateWhen

`propagateWhen` is the complement to `readyWhen`. Where `readyWhen` signals when a node is healthy,
`propagateWhen` gates when a node may be evaluated at all. Together they bookend a node's lifecycle:
`propagateWhen` controls when mutation _can start_; `readyWhen` signals when it is _complete_.

```yaml
- id: service
  propagateWhen:
    - ${deployment.ready()}
  template:
    apiVersion: v1
    kind: Service
    ...
```

When any `propagateWhen` expression evaluates to false, the node and all its dependents are not
re-evaluated — they remain in their previous state. When all expressions are true, evaluation
proceeds normally. The default is `[]` (no gate — evaluate immediately).

This is the mechanism for users who want the current RGD behavior where readyWhen gates dependents.
In Graph, that behavior is opt-in: add `propagateWhen: [${dep.ready()}]` to get explicit gating.

#### Lifecycle Methods

Two lifecycle methods expose node state for use in `propagateWhen` and other CEL expressions:

```cel
deployment.ready()    // true when readyWhen conditions are satisfied
deployment.updated()  // true when evaluated against the current graph generation
```

These enable propagation patterns:

```yaml
# Exponential rollout — budget doubles each wave
- id: deployments
  forEach:
    - app: ${apps}
  propagateWhen:
    - >-
      ${deployments.filter(d, d.updated() && !d.ready()).size()
       < max(1, deployments.filter(d, d.updated() && d.ready()).size())}
```

Because Graph is recursive, `propagateWhen` composes at every level of nesting. In the RGD
implementation: L1 can gate propagation to instances, and L2 can gate propagation to individual
resources — all from the same mechanism, no special-casing per layer. See KREP-006 for the broader
motivation and design discussion.

### Dependencies

Dependencies are inferred from CEL expressions. If node B's template contains `${A.field}`, B
depends on A. The compiler builds a DAG from these references and computes topological order. Cycles
are compile-time errors.

#### Hard Dependencies

A bare field reference creates a hard dependency:

```cel
${deployment.status.availableReplicas}
```

Node B cannot evaluate until node A has been applied and its observed state is in scope. This is the
standard behavior — identical to today's RGD.

#### Soft Dependencies

Optional chaining creates a soft dependency:

```cel
${deployment.?status.?loadBalancer.orValue("pending")}
```

A soft dependency is _never_ gated. If the dependency hasn't been evaluated yet, the expression
resolves to `optional.none()`, and `.orValue()` provides the fallback. If the dependency has been
evaluated, the full observed state is available.

**Why soft dependencies exist:** Status writeback creates a dependency cycle: the `patch:` node
references all managed resources, but can't hard-depend on all of them without blocking itself.
Optional chaining breaks the cycle — the status node runs on every pass, filling in whatever is
available, progressively transitioning from `IN_PROGRESS` to `ACTIVE`.

### Nested Graphs

When a node's template is itself a Graph, you get nested composition: a parent Graph that stamps a
child Graph. You don't strictly need `forEach` for this — a single template node can create one
child Graph — but the combination of `forEach` + template:{kind: Graph} is the powerful pattern. It
stamps one child Graph per item in the collection. This is how RGD emerges — a per-RGD Graph stamps
a per-instance Graph, each with its own scope, its own revisions, and its own lifecycle.

#### Deferral Boundaries

Child Graph CEL expressions live as literal strings inside the parent's template. The parent
compiler sees them as opaque data, not as CEL. The evaluation model uses a nesting convention to
manage this:

- `${...}` — evaluated at the current level
- `${${...}}` — the outer `${}` is stripped (producing a literal `${...}` string), evaluated one
  level down by the child's controller

```yaml
# Parent Graph (L0) evaluates this — bakes the RGD name into the child spec:
name: ${rgd.metadata.name}

# Parent strips outer ${}, child Graph (L1) evaluates the inner expression:
group: ${${rgd.spec.schema.group}}
```

This composes to arbitrary depth. `${${${...}}}` defers two levels.

#### Recursive Compilation

Recursive compilation enables parent-time error detection for child Graph expressions. When the
compiler detects nested `${${...}}` patterns, it type-checks inner expressions against the child's
statically known node list. When the child template is literally a Graph (the apiVersion/kind is
`kro.run/v1alpha1 Graph`), the compiler extracts the child spec, strips one deferral level, and runs
the full compilation pipeline on it.

This means: a typo in a child expression, a type mismatch in a child template, or a cycle in a
child's dependency graph are all reported on the parent at compile time. Without this, errors in
child expressions would only surface after the child CR is created and its controller attempts
compilation — a delayed feedback loop. Recursive compilation makes it immediate.

#### Lifecycle Coupling

Nested Graphs use `ownerReferences` to tie child lifecycle to parent. When the owner is deleted,
Kubernetes cascades the delete to the child Graph, triggering its ordered teardown. But Kubernetes
cascade is asynchronous — without coordination, the owner can complete deletion while teardown is
still running, orphaning managed resources. To prevent this, the child Graph places a finalizer on
the owner via a `patch:` node:

```yaml
metadata:
  ownerReferences:
    - apiVersion: apps.example.com/v1
      kind: WebApp
      name: my-instance
      uid: ...
spec:
  nodes:
    - id: lifecycle
      patch:
        apiVersion: apps.example.com/v1
        kind: WebApp
        metadata:
          name: my-instance
          finalizers:
            - kro.run/graph
    - id: deployment
      template: ...
```

The ownerReference triggers self-deletion when the instance enters Terminating. The `patch:` node
holds the instance until teardown completes — all template resources are pruned in reverse DAG
order, then the patch releases the finalizer as its last act, and the instance finishes deletion.
This pattern is load-bearing for the RGD implementation: each per-instance Graph (L2) holds a
finalizer on its instance, ensuring no managed resources are orphaned on deletion.

## How RGD Emerges from Graph

The RGD system is three levels of nested Graphs:

```
L0: rgd-controller
├── Creates the ResourceGraphDefinition CRD
├── Watches all RGD objects
└── forEach RGD → creates L1 Graph
    │
    L1: per-RGD (one per ResourceGraphDefinition)
    ├── ref: reads the specific RGD object
    ├── Creates the user's Kind CRD from RGD schema
    ├── Watches all instances of the user's Kind
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

Two conditions on orthogonal axes. Both use standard Kubernetes condition fields;
`lastTransitionTime` is preserved when status is unchanged; `observedGeneration` advances
independently per condition.

**`Compiled`** — is the spec valid? Set once when the spec is processed, permanent until the spec
changes. True on success; False on expression errors, dependency cycles, or malformed node
declarations.

**`Ready`** — has the graph converged? `True` means all nodes are applied and their `readyWhen`
conditions pass. `Unknown` means the controller is still making progress. `False` means something
requires human intervention.

Compiled rolls into Ready: if the spec fails compilation, Ready is `False` with reason
`NotCompiled`. A single alarm on `Ready` covers both compilation failures and runtime issues —
there is no need to monitor Compiled separately.

| Ready Reason  | Status    | Meaning                            |
|---------------|-----------|------------------------------------|
| Ready         | True      | All nodes applied and ready        |
| NotReady      | Unknown   | readyWhen not yet met              |
| Pending       | Unknown   | Waiting for upstream data          |
| NotCompiled   | False     | Spec is invalid (rollup of Compiled=False) |
| Conflict      | False     | SSA field ownership contested      |
| Error         | False     | Client request failed (4xx)        |
| SystemError   | False     | Infrastructure failure (5xx)       |

User-defined status does not live on the Graph object. It lives on custom resources and is written
via `patch:` nodes. The Graph's status contains only controller-managed fields — system conditions
like `Compiled` never appear on user resources.

## Relationship to Existing KREPs

| KREP                                       | Relationship                                                                                                                                                                                                                                                                                                                                                                               |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| KREP-001 (Status Conditions)               | Graph changes where conditions live. System conditions (`Compiled`, `Ready`) exist on Graph objects — never on user resources. Users define their own status via `patch:` nodes. This separates system health (observable on Graph objects) from user-facing status (controlled by the graph author).                                                                                      |
| KREP-002 (Collections)                     | Adopted unchanged. Graph extends it: when a forEach node's template is a Graph, you get recursive composition.                                                                                                                                                                                                                                                                              |
| KREP-003 (Decorators)                      | A Decorator is naturally a Graph with `watch:` + `forEach`. No special runtime support needed. Graph also resolves the Singleton problem KREP-003 identified — a Graph needs no schema or CRD to self-instantiate.                                                                                                                                                                         |
| KREP-006 (Propagation Control)             | This KREP defines `propagateWhen`'s core semantics. KREP-006 provides the broader motivation (rate controls, reactive controls, manual controls) and design discussion. Because Graph is recursive, propagateWhen composes at every level of nesting — an RGD gets propagation control over instances, and each instance gets propagation control over resources, from a single mechanism. |
| KREP-008 (includeWhen Resource References) | Graph implements `includeWhen` as a first-class modifier — when false, the node is excluded and becomes a prune candidate. This is distinct from `propagateWhen` (which freezes in place). In RGD, KREP-008 added complexity to make includeWhen reference upstream resources; in Graph, that works naturally because all modifiers participate in dependency inference.                   |
| KREP-011 (Variables)                       | `def:` is Graph's implementation. Same semantics.                                                                                                                                                                                                                                                                                                                                            |
| KREP-013 (Graph Revisions)                 | Applies to Graph unchanged. Because Graph is recursive, each nested Graph gets independent revisions. An RGD spec change revisions the L1 Graph; an instance spec change revisions L2. These are distinct — rolling back an RGD change does not require rolling back every instance.                                                                                                       |
| KREP-014 (Resource Lifecycles)             | Graph's node types implicitly define lifecycle: `template:` = delete-on-prune, `patch:` = release-fields-on-prune. A per-node `lifecycle.apply: Force` flag was explored in prototyping to handle contested field ownership. Per-node lifecycle policies (Orphan, Retain) are a natural follow-on.                                                                                         |
| KREP-018 (Partial Dependencies)            | CEL branching (`${cond ? A.field : B.field}`) and Graph's soft dependency mechanism (`?.`) together address partial dependency scenarios. Soft deps allow referencing a node without creating a hard gate — the taken-branch problem becomes less acute when the untaken branch uses optional chaining.                                                                                    |
| KREP-019 (Deferred Fields & Soft Deps)     | Graph's `?.` optional chaining is the implementation of this KREP. Soft deps allow progressive field population across reconcile passes.                                                                                                                                                                                                                                                   |

## Future Work

Graph was validated through an extensive prototyping effort (Krocodile) that explored how far the
primitive extends. Beyond the core API proposed here, the prototype implements:

- **`Kind` — a simplified RGD with graph-like semantics.** Defines a new Kubernetes Kind (CRD +
  per-instance Graphs) in a single object. Unlike RGD, Kind uses `readyWhen` and `propagateWhen`
  directly at the spec level, giving instance-level rollout control with no additional machinery.
  Kind is the intended successor to RGD; the RGD compatibility layer is built on top of Kind.
- **Propagation control** — rate-limited rollouts, time-based gates, reactive controls (KREP-006
  covers the design; the prototype validates it composes with nested Graphs)
- **Prometheus metric emission** (`metric:`) — emit gauges driven by CEL expressions
- **Finalizer coordination** (`finalizes:`) — cross-resource cleanup ordering; finalizer nodes
  materialize only when their target becomes a prune candidate
- **Time-based scheduling** — `time.now()` comparisons that solve for the exact future timestamp and
  enqueue reconciliation precisely
- **SSA ownership protocol** — field-level ownership tracking via server-side apply field managers,
  with a per-node `lifecycle.apply: Force` flag for contested ownership. On deletion, templates are
  fully pruned before patch field claims are released (ordering invariant).
- **Reconciliation algorithm** — wavefront evaluation with topological ordering and prune in reverse
  order
- **CEL lifecycle functions** — `.dependencies()` returns a node's upstream dependency list (enables
  `${node.dependencies().all(d, d.ready())}`); `.condition()` encapsulates the Kubernetes status
  condition pattern with `lastTransitionTime` preservation and `observedGeneration` tracking
- **Cryptographic CEL functions** — `rsa.generateKey`, `ecdsa.generateKey`, `ed25519.generateKey`,
  and `x509.createCertificateRequest` enable TLS bootstrapping inline in a Graph, eliminating the
  need for cert-manager in simple certificate use cases

These features compose naturally with Graph but are separate design concerns, proposable
incrementally. The key finding from Krocodile is that Graph's recursive structure means each feature
added at one level automatically applies at every level of nesting. propagateWhen at L1 controls
instance-level rollouts; the same mechanism at L2 controls per-resource rollouts. Graph Revisions at
L1 track RGD spec changes; the same mechanism at L2 tracks instance changes independently. Nothing
is special-cased per layer.
