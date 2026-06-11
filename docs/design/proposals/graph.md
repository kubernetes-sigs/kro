# KREP-024: Graph

## Summary

Graph is a new `kro.run/v1alpha1` Kind — the atomic runtime primitive for composing Kubernetes
resources. A Graph is a set of nodes evaluated in topological order. Create it and its resources
converge; delete it and they cascade away. It is the simplest possible unit of composition in kro.

RGD today combines Kind definition, instance management, and resource composition into one object.
Graph separates these — providing resource composition alone — making each concern independently
usable. This enables patterns RGD cannot express: static resource bundles (no CRD needed),
singletons (multiple contributors, one resource), and decorators (react to existing resources
without defining a new Kind). Higher-level abstractions like RGD can be built _from_ Graph; every
feature added to Graph automatically benefits every abstraction layered on top.

Graph was validated by building the full RGD system from it. A single Graph implements the RGD
controller — creating CRDs, watching instances, managing resources, writing status — all through
composition rather than imperative Go code. This implementation passes 23 of 36 upstream kro
compatibility test files, with remaining gaps due to prototype immaturity rather than structural
limitations (see [examples/graph/rgd.yaml](../../../examples/graph/rgd.yaml)).

Beyond the RGD proof, Graph enables patterns that are simpler than what RGD can express today:
- [Namespace decorator](../../../examples/graph/namespace-decorator.yaml) — watch namespaces, create
  NetworkPolicies. No CRD, no schema, no instance. (The decorator pattern from KREP-003.)
- [Ingress fan-in](../../../examples/graph/ingress-fanin.yaml) — aggregate Services into a single
  Ingress with dynamic routes. (The aggregated-resource pattern from KREP-003.)
- [CoreDNS installation](../../../examples/graph/coredns.yaml) — install CoreDNS as a Graph.
  The static-bundle pattern: dependency-ordered, health-aware, one object replaces a Helm chart.
- [Singleton](../../../examples/graph/singleton.yaml) — fan-in with priority-based resolution
  when multiple actors claim the same resource.

### What this KREP covers

**Proposed:** The Graph Kind — node types (`template`, `patch`, `ref`, `watch`, `def`), dependency
inference from CEL expressions, status conditions (`Compiled`, `Ready`), self-references, nested
composition, and the decoupling of `readyWhen` from dependency gating (consistent with KREP-006's
design discussion). In Graph, `readyWhen` is a health signal — it does not block downstream nodes.
RGD's existing gating-on-readiness behavior is unchanged.

**Inherited unchanged from RGD:** `includeWhen`, `forEach`, and CEL expression syntax. These
mechanisms carry forward with the same semantics and are not redefined by this KREP.

**Adopted from KREP-006:** The `.ready()` and `.updated()` lifecycle signals, and the core semantics
of `propagateWhen` as an explicit evaluation gate. Sections in this document define how these compose
with Graph's recursive structure. Rollout strategies (`exponentiallyUpdated`, `linearlyUpdated`),
collection-level defaults, and budget syntax are KREP-006's to define.

**Relationship to RGD:** Graph is proposed as a sibling primitive. RGD continues to work unchanged.
We have the option to implement RGD's internals on top of Graph in the future, but for the immediate
term both implementations live as siblings sharing significant code in the underlying graph engine.
Graph is additive — it does not replace or deprecate RGD.

**Security posture:** Creating a Graph gives its author full control over everything KRO can do —
creating, patching, deleting, and watching arbitrary Kubernetes resources within the permissions
granted to the KRO controller's service account. A Graph author operates at the same privilege level
as today's RGD author: the "infrastructure author" persona who defines what resources exist and how
they relate.

KRO's dual-persona model is preserved at higher layers. Authors (broad permissions) write
RGDs/Graphs; consumers (narrow permissions scoped to the CRD the author created) interact only with
the Kinds those Graphs implement, never with Graph objects directly. Graph is author-facing — end
users never create or modify Graphs. Future work on credential scoping — short-lived tokens, caller
credentials (analogous to how CloudFormation uses the caller's IAM role rather than the service's),
or per-Graph service accounts — applies uniformly to all KRO primitives and is out of scope for this
KREP.

## Motivation: Graph as Scope

A Graph is an **isolated scope**. Its nodes form a flat namespace — private, visible only within the
Graph — with spec as input and status as output. Nothing inside a Graph is reachable from outside
except through the Kubernetes resource boundary: the Graph object's own spec and status fields.

This isolation has three structural effects:

1. **Concurrency is a consequence of scope boundaries.** A `forEach` over 1000 instances stamps 1000
   independent Graphs, each with its own scope and lifecycle, each reconciled independently.
   Parallelism isn't a feature of the evaluation loop — it emerges from the isolation. Within a
   single scope, serial evaluation is correct and sufficient: CEL evaluates in microseconds, each
   node is one API call, and the API server — not the graph — is the throughput constraint.

2. **Propagation is scope-local.** A `propagateWhen` gate within a child Graph is invisible to the
   parent. The parent sees only the child's `Ready` condition. Rollout control composes without
   leaking — a canary strategy inside one instance doesn't slow or block sibling instances.

3. **Communication is explicit.** Graphs talk to each other only through the resources that bind
   them: the spec of a child Graph stamped by a parent, a `ref` to a resource managed by another
   Graph, or a `watch` on a Kind whose instances are managed elsewhere. There is no shared memory,
   no implicit coupling, no cross-scope references.

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
      matchLabels:
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

`def:` is Graph's implementation of the variables concept proposed in KREP-011. Graphs that compose
at scale — like the RGD implementation — require named intermediate computations to remain
maintainable; `def:` provides them without creating cluster resources.

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
    apiVersion: ${'${schema.spec.schema.group}'}/${'${schema.spec.schema.apiVersion}'}
    kind: ${'${schema.spec.schema.kind}'}
    selector:
      matchLabels: {}
```

When the compiler encounters a dynamic GVK, it uses a deferred-type path: the node compiles
permissively (untyped) until the first reconcile resolves the concrete GVK. This is what makes the
RGD-from-Graph pattern possible — the user's Kind isn't known at compile time.

### Node Modifiers

`includeWhen` and `forEach` are node-level modifiers — they can be applied to any node type.
`includeWhen` conditionally excludes a node (making it a prune candidate when false). `forEach`
stamps a node once per item in a collection. Both retain the same semantics as in today's RGD and
are not redefined here — see KREP-008 and KREP-002 respectively.

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
resolves to `optional.none()` — the field is omitted from the apply, not set to a zero value.
`.orValue()` provides an explicit fallback when absence isn't acceptable. If the dependency has been
evaluated, the full observed state is available.

**Why soft dependencies exist:** Status writeback creates a dependency cycle: the `patch:` node
references all managed resources, but can't hard-depend on all of them without blocking itself.
Optional chaining breaks the cycle — the status node runs on every pass, filling in whatever is
available, progressively transitioning from `IN_PROGRESS` to `ACTIVE`.

### Self-References

Before evaluating a node's expressions, kro GETs the target resource from the API server. The live
object — including `metadata` and `status` — enters scope under the node's `id`. This is the
observed state: what exists before the node acts. On first create (GET returns 404), the scope entry
is an empty map — expressions use optional chaining (`.?`, `.orValue()`) to provide defaults. After
apply, the apply response replaces the scope entry — downstream nodes see post-apply state, not the
pre-apply observation.

Because observation precedes evaluation, a node can reference its own `id` in expressions to read
its current state. This enables patterns that require continuity across reconciles:

- **Condition transitions** — read existing `lastTransitionTime` and preserve it when status hasn't
  changed, stamp `time.now()` only on actual transitions
- **Latching and rotation** — don't regenerate a private key if one already exists in the Secret;
  rotate by checking the existing key's age against a policy threshold
- **Generation awareness** — compare `metadata.generation` against `status.observedGeneration` to
  detect whether a resource has processed its latest spec

Two derived signals expose node state for use in CEL expressions (adopted from KREP-006):

```cel
deployment.ready()    // true when readyWhen conditions are satisfied
deployment.updated()  // true when evaluated against the current graph.metadata.generation
```

`.ready()` is syntactic sugar over self-reference — it evaluates the node's `readyWhen` against its
observed state. `.updated()` reflects whether the node has been applied in the current generation
(`graph.metadata.generation`, which increments on spec changes): its value is persisted as an
annotation on the managed resource, so it survives controller restarts and is visible on GET.

### Propagation

Today's RGD gates downstream nodes on upstream readiness — a node cannot evaluate until its
dependencies pass `readyWhen`. This is implicit propagation control: readiness and ordering are
coupled. KREP-006 argues these should be separate concerns: `readyWhen` signals when a node is
_healthy_; `propagateWhen` gates when a node may _mutate_. Graph implements this decoupling.

#### `readyWhen`

`readyWhen` is a list of CEL boolean expressions evaluated against the node's live state. When all
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

In Graph, `readyWhen` is purely a health signal. Downstream nodes proceed as soon as their
dependencies are _in scope_ (applied, observed state available) — they do not wait for readiness.
This means resources that don't need to wait — like a Service referencing a Deployment's labels —
can apply immediately rather than blocking until the Deployment is fully rolled out.

`readyWhen` determines:

1. Whether `.ready()` returns true for this node in CEL expressions
2. Whether the **Graph itself** is Ready — the Graph's Ready condition is the conjunction of all
   nodes' `readyWhen` results

#### `propagateWhen`

When you _do_ want to gate on readiness (or any other condition), `propagateWhen` makes the ordering
explicit and author-controlled:

```yaml
- id: migration
  propagateWhen:
    - ${database.ready()}
  template:
    apiVersion: batch/v1
    kind: Job
    metadata:
      name: schema-migration
    spec:
      template:
        spec:
          containers:
            - name: migrate
              image: myapp/migrate:latest
          restartPolicy: Never
```

When any `propagateWhen` expression evaluates to false, the node retains its last-applied state —
it is not re-evaluated. When all expressions are true, evaluation proceeds normally. The default is
`[]` (no gate — evaluate immediately). Where `includeWhen: false` prunes a node (deleting its
resource), `propagateWhen: false` freezes it — the resource persists in its last-applied state.

The separation gives authors control over what waits and what doesn't, rather than coupling all
ordering to health checks. Most nodes need no gate at all — they apply as soon as their
dependencies are in scope. The few that need sequencing declare it explicitly.

#### Collections

A forEach node is a single node in the DAG. Downstream nodes depend on it atomically — they see only
the final state after all items are processed.

Self-reference extends naturally to collections. A forEach node can reference its own collection in
`propagateWhen` because it reads observed state, not evaluation output. Before iterating, the
controller computes each item's resource identity from the iteration variable and batch-GETs all
items. The collection enters scope as a list of observed states, each carrying `.updated()` and
`.ready()` from the cluster.

Items are then processed serially. For each item: evaluate `propagateWhen` against the current
collection scope. If satisfied, evaluate the template, apply, and replace that item's scope slot
with the apply response (which now carries the generation stamp). Items later in the iteration see
the effects of items earlier — this is how propagation budgets tighten within a single reconcile
pass.

This makes collection-level propagation budgets expressible:

> *The budget pattern below illustrates how `propagateWhen` composes with `forEach`. The rollout
> API — including built-in strategies, budget syntax, and collection defaults — is defined by
> KREP-006.*

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

The budget counts in-flight items (updated but not yet ready) against landed items (updated and
ready). On first evaluation nothing is updated — the budget allows one item through. As items land,
the budget grows, doubling capacity with each wave.

Because Graph is recursive, `propagateWhen` composes at every level of nesting. A parent Graph can
gate propagation to child Graphs, and each child Graph can independently gate propagation to its own
resources — all from the same mechanism, no special-casing per layer. See KREP-006 for the broader
motivation and design discussion.

### Nested Graphs

When a node's template is itself a Graph, you get nested composition: a parent Graph that stamps a
child Graph as a real cluster object. The child Graph is persisted, reconciled independently, and
carries its own conditions and revision history — providing full debuggability via `kubectl get
graph`. You don't strictly need `forEach` for this — a single template node can create one child
Graph — but the combination of `forEach` + template:{kind: Graph} is the powerful pattern. It stamps
one child Graph per item in the collection. This is how an RGD-equivalent is expressed — a per-RGD
Graph stamps a per-instance Graph, each with its own scope, its own revisions, and its own lifecycle.

#### Deferral Boundaries

Child Graph CEL expressions live as literal strings inside the parent's template. Deferral is not a
special syntax or preprocessing step — it falls out of CEL string semantics. To produce a literal
`${expr}` in the child's spec, the parent writes a CEL string expression whose value is that
literal:

- `${...}` — evaluated by the current Graph
- `${'${...}'}` — a CEL string literal; the parent evaluates it to produce the text `${...}`, which
  the child Graph then evaluates at its own scope

```yaml
# Parent Graph (L0) evaluates this — bakes the RGD name into the child spec:
name: ${rgd.metadata.name}

# Parent produces literal "${rgd.spec.schema.group}" — child Graph (L1) evaluates it:
group: ${'${rgd.spec.schema.group}'}
```

This composes to arbitrary depth. `${'${"${...}"}'}` defers two levels — each layer evaluates one
string literal, peeling off one level of quoting.

#### Recursive Compilation

When the child template is a Graph, the compiler extracts the child spec, resolves the parent-level
string literals to recover the child's expressions, and runs the full compilation pipeline on it. A
typo in a child expression, a type mismatch in a child template, or a cycle in a child's dependency
graph are all reported on the parent at compile time — not deferred until the child CR is created.

## Expressing RGD as Graph

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
`NotCompiled`. A single alarm on `Ready` covers both compilation failures and runtime issues — there
is no need to monitor Compiled separately.

| Ready Reason | Status  | Meaning                                    |
| ------------ | ------- | ------------------------------------------ |
| Ready        | True    | All nodes applied and ready                |
| NotReady     | Unknown | readyWhen not yet met                      |
| Pending      | Unknown | Waiting for upstream data                  |
| NotCompiled  | False   | Spec is invalid (rollup of Compiled=False) |
| Conflict     | False   | SSA field ownership contested              |
| Error        | False   | Client request failed (4xx)                |
| SystemError  | False   | Infrastructure failure (5xx)               |

User-defined status does not live on the Graph object. It lives on custom resources and is written
via `patch:` nodes. The Graph's status contains only controller-managed fields — system conditions
like `Compiled` never appear on user resources.

## Relationship to Existing KREPs

| KREP                                       | Relationship                                                                                                                                                                                                                                                                                                                                                                               |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| KREP-001 (Status Conditions)               | Graph changes where conditions live. System conditions (`Compiled`, `Ready`) exist on Graph objects — never on user resources. Users define their own status via `patch:` nodes. This separates system health (observable on Graph objects) from user-facing status (controlled by the graph author).                                                                                      |
| KREP-002 (Collections)                     | Adopted unchanged. Graph extends it: when a forEach node's template is a Graph, you get recursive composition.                                                                                                                                                                                                                                                                             |
| KREP-003 (Decorators)                      | A Decorator is naturally a Graph with `watch:` + `forEach`. No special runtime support needed. Graph also resolves the Singleton problem KREP-003 identified — a Graph needs no schema or CRD to self-instantiate.                                                                                                                                                                         |
| KREP-006 (Propagation Control)             | Graph adopts KREP-006's lifecycle signals (`.ready()`, `.updated()`) and core `propagateWhen` gating semantics. KREP-006 provides the broader motivation (rate controls, reactive controls, manual controls), rollout strategies, and collection defaults. Because Graph is recursive, propagateWhen composes at every level of nesting — an RGD gets propagation control over instances, and each instance gets propagation control over resources, from a single mechanism. |
| KREP-008 (includeWhen Resource References) | Graph implements `includeWhen` as a first-class modifier — when false, the node is excluded and becomes a prune candidate. This is distinct from `propagateWhen` (which freezes in place). In RGD, KREP-008 added complexity to make includeWhen reference upstream resources; in Graph, that works naturally because all modifiers participate in dependency inference.                   |
| KREP-011 (Variables)                       | `def:` is Graph's implementation. Same semantics.                                                                                                                                                                                                                                                                                                                                          |
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
  Kind demonstrates how a higher-level abstraction composes Graph primitives; the RGD compatibility
  layer in the prototype is built on top of Kind.
- **Propagation control** — rate-limited rollouts, time-based gates, reactive controls (KREP-006
  covers the design; the prototype validates it composes with nested Graphs)
- **Prometheus metric emission** (`metric:`) — emit gauges driven by CEL expressions
- **Finalizer coordination** (`finalizes:`) — cross-resource cleanup ordering; finalizer nodes
  materialize only when their target becomes a prune candidate
- **Time-based scheduling** — `time.now()` evaluated once per reconcile pass; the system solves for
  the exact requeue deadline rather than polling.
- **SSA ownership protocol** — field-level ownership tracking via server-side apply field managers,
  with a per-node `lifecycle.apply: Force` flag for contested ownership.
- **Reconciliation algorithm** — wavefront evaluation with topological ordering and prune in reverse
  order
- **CEL lifecycle functions** — `.dependencies()` returns a node's upstream dependency list;
  `.condition()` encapsulates the Kubernetes status condition pattern with `lastTransitionTime`
  preservation and `observedGeneration` tracking
- **Cryptographic CEL functions** — `rsa.generateKey`, `ecdsa.generateKey`, `ed25519.generateKey`,
  and `x509.createCertificateRequest` enable TLS bootstrapping inline in a Graph, eliminating the
  need for cert-manager in simple certificate use cases

These features compose naturally with Graph but are separate design concerns, proposable
incrementally. The key finding from Krocodile is that Graph's recursive structure means each feature
added at one level automatically applies at every level of nesting. propagateWhen at L1 controls
instance-level rollouts; the same mechanism at L2 controls per-resource rollouts. Graph Revisions at
L1 track RGD spec changes; the same mechanism at L2 tracks instance changes independently. Nothing
is special-cased per layer.
