# KREP-024: Graph

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
composition rather than imperative Go code. This implementation passes 23 of 36 upstream kro
compatibility test files, with remaining gaps due to prototype immaturity rather than structural
limitations (see [examples/graph/rgd.yaml](../../../examples/graph/rgd.yaml)).

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

**Proposed:** The Graph Kind — node types (`template`, `patch`, `ref`, `watch`, `def`), dependency
inference from CEL expressions, the evaluation model (nodes evaluate when dependencies are in scope,
not when they are ready), status conditions (`Compiled`, `Ready`), and nested composition. These are
new primitives that do not exist in KRO today.

**Inherited unchanged from RGD:** `includeWhen`, `forEach`, `readyWhen`, and CEL expression syntax.
These mechanisms carry forward with the same semantics and are not redefined by this KREP.

**Defers to KREP-006:** `propagateWhen` gating semantics, `.ready()` and `.updated()` lifecycle
signals, collection-level rollout strategies, and budget syntax. Graph supports all of these; their
API and behavior are defined by KREP-006.

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

Graph has five node types. Each is declared by a keyword on the node object. We choose explicit
keywords over fewer types with strategy flags so that a reader knows what a node does — and what
type it produces in scope — at a glance from the top-level keyword.

#### `template:`

Creates and owns a Kubernetes resource via server-side apply. On deletion of the Graph, the resource
is deleted.

#### `patch:`

Contributes fields to a resource you don't own. The resource must already exist. On prune, your
fields are released — the resource itself is not deleted.

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

Reads a single named resource into scope as an object. No write, no ownership, no cleanup.

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

Reads all resources matching a label selector into scope as a list. No write, no ownership, no
cleanup.

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
  watch:
    apiVersion: ${crd.spec.group}/${crd.spec.versions[0].name}
    kind: ${crd.spec.names.kind}
    selector:
      matchLabels: {}
```

### Node Modifiers

`includeWhen` and `forEach` are node-level modifiers — they can be applied to any node type.
`includeWhen` conditionally includes a node — when false, the node becomes a prune candidate.
`forEach` stamps a node once per item in a collection. Both retain the same semantics as in today's
RGD and are not redefined here — see KREP-008 and KREP-002 respectively.

| Modifier        | Question it answers       | When false                           | Defined in           |
| --------------- | ------------------------- | ------------------------------------ | -------------------- |
| `includeWhen`   | Should this node exist?   | Prune — resource deleted             | KREP-008             |
| `propagateWhen` | May this node mutate now? | Freeze — last-applied state persists | KREP-006             |
| `readyWhen`     | Is this node healthy?     | Signal only — Graph not Ready        | (inherited from RGD) |

### Dependencies

Dependencies are inferred from CEL expressions. If node B's template contains `${A.field}`, B
depends on A. The compiler builds a DAG from these references and computes topological order. Cycles
are compile-time errors.

Each node's observed state (from a GET before apply) enters scope under its `id`. Downstream nodes
reference these scope entries via CEL.

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
`.orValue()` provides an explicit fallback when absence isn't acceptable.

**Why soft dependencies exist:** Status writeback creates a dependency cycle: the `patch:` node
references all managed resources, but can't hard-depend on all of them without blocking itself.
Optional chaining breaks the cycle — the status node runs on every pass, filling in whatever is
available.

#### Evaluation Model

A node evaluates as soon as its hard dependencies are in scope — meaning they have been applied and
their observed state is available from the cluster. Nodes do not wait for dependencies to pass
`readyWhen`.

Consider a Deployment and a Service: the Service's selector references the Deployment's labels.
Under ready-gating, the Service cannot be created until the Deployment's replicas are available —
minutes of unnecessary waiting. Under scope-gating, the Service is created as soon as the Deployment
exists and its labels are observable (seconds). The same pattern appears broadly: an EC2
NetworkInterface can attach to an Instance as soon as the Instance ID is known, without waiting for
the Instance to pass health checks. Ready-gating forces sequential convergence; scope-gating lets
the graph flow as fast as data allows, with `propagateWhen` available for cases where explicit
health gates are needed.

This is what enables status writeback in the RGD-as-Graph implementation: a `patch:` node can
reference all managed resources and run on every reconcile pass, progressively filling in available
fields without deadlocking on readiness.

Within a single Graph, nodes evaluate serially in topological order. Across Graphs, reconciliation
is fully parallel: each Graph object is an independent work item in the controller's queue.
`propagateWhen` (KREP-006) provides explicit gating when authors need to sequence mutations on
health or other conditions. `readyWhen` determines the Graph's overall Ready condition and powers
the `.ready()` lifecycle signal (also KREP-006). Both compose with Graph's recursive structure — see
KREP-006 for semantics, strategies, and collection-level defaults.

### Nested Graphs

A `template:` node can contain any Kind — including Graph itself. Nesting isn't an opt-in feature;
it emerges from the Kubernetes API and Graph semantics.

When a node's template is a Graph, you get nested composition: a parent Graph that stamps a child
Graph as a real cluster object. The child is persisted, reconciled independently, and carries its
own conditions and revision history — providing full debuggability via `kubectl get graph`. The
combination of `forEach` + template:{kind: Graph} stamps one child Graph per item in a collection.
This is how an RGD-equivalent is expressed — a per-RGD Graph stamps a per-instance Graph, each with
its own scope, its own revisions, and its own lifecycle.

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

This composes to arbitrary depth. Each layer evaluates one string literal, peeling off one level of
quoting.

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

A Graph has two conditions:

**Compiled** — is the spec valid? Set once when the spec is processed. True on success; False on
expression errors, dependency cycles, or malformed node declarations. Permanent until the spec
changes.

**Ready** — has the graph converged? True means all nodes are applied and their readyWhen conditions
pass. Unknown means the controller is still making progress. False means something requires human
intervention.

If compilation fails, Ready is False with reason NotCompiled. A single alarm on Ready covers both
compilation failures and runtime issues.

| Ready Reason | Status  | Meaning                       |
| ------------ | ------- | ----------------------------- |
| Ready        | True    | All nodes applied and ready   |
| NotReady     | Unknown | readyWhen not yet met         |
| Pending      | Unknown | Waiting for upstream data     |
| NotCompiled  | False   | Spec is invalid               |
| Conflict     | False   | SSA field ownership contested |
| Error        | False   | Client request failed (4xx)   |
| SystemError  | False   | Infrastructure failure (5xx)  |

User-defined status does not live on the Graph object. It lives on custom resources and is written
via `patch:` nodes. The Graph's status contains only controller-managed conditions.

## Security Posture

Graph does not introduce new privilege boundaries. A Graph author operates at the same privilege
level as today's RGD author: the "infrastructure author" persona who defines what resources exist
and how they relate. Graph authors have full control over everything KRO can do within the
permissions granted to the KRO controller's service account.

KRO's dual-persona model is preserved at higher layers. Authors (broad permissions) write
RGDs/Graphs; consumers (narrow permissions scoped to the CRD the author created) interact only with
the Kinds those Graphs implement, never with Graph objects directly. Future work on credential
scoping (short-lived tokens, caller credentials, per-Graph service accounts) applies uniformly to
all KRO primitives and is out of scope for this KREP.

## Relationship to Existing KREPs

| KREP                            | Relationship                                                                                                                                                             |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| KREP-001 (Status Conditions)    | System conditions (`Compiled`, `Ready`) exist on Graph objects — never on user resources. Users define their own status via `patch:` nodes.                              |
| KREP-002 (Collections)          | Adopted unchanged. Graph extends it: when a forEach node's template is a Graph, you get recursive composition.                                                           |
| KREP-003 (Decorators)           | A Decorator is naturally a Graph with `watch:` + `forEach`. No special runtime support needed.                                                                           |
| KREP-006 (Propagation Control)  | Graph adopts KREP-006's lifecycle signals and `propagateWhen` gating. Because Graph is recursive, propagation composes at every nesting level. KREP-006 defines the API. |
| KREP-008 (includeWhen)          | Graph implements `includeWhen` as a first-class modifier. Dependency inference works naturally — no special handling needed.                                             |
| KREP-011 (Variables)            | `def:` is Graph's implementation. Same semantics.                                                                                                                        |
| KREP-013 (Graph Revisions)      | Applies unchanged. Each nested Graph gets independent revisions.                                                                                                         |
| KREP-014 (Resource Lifecycles)  | Graph's node types implicitly define lifecycle: `template:` = delete-on-prune, `patch:` = release-fields-on-prune. Per-node lifecycle policies are a natural follow-on.  |
| KREP-018 (Partial Dependencies) | Soft dependencies (`?.`) address the taken-branch problem — the untaken branch uses optional chaining rather than creating a hard gate.                                  |
| KREP-019 (Deferred Fields)      | Graph's `?.` optional chaining is the implementation of this KREP.                                                                                                       |

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
