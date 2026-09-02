# KREP-024: Graph

> **Implementation status (as of v1alpha1):** This proposal document has been partially reconciled with the shipped implementation in `pkg/graphengine/` and `pkg/controller/graph/`. Key architectural differences from earlier drafts — including unified `ref:` for collections (superseding `watch:`), explicit `graph:` subgraph nodes, the raw-manifest `patch:` API (a `Patch *runtime.RawExtension` field authored exactly like `template:` — no `PatchSpec` wrapper, no `subresource`/`body` fields, no `forEach`; the target endpoint is derived from field presence, see `api/v1alpha1/graph_types.go:208`), collection expansion caps (`MaxCollectionSize`), and the `Accepted`/`ResourcesConverged`/`Ready` condition model — are reflected below. Note also that RGD is no longer a sibling engine: the RGD controller now unconditionally routes through the graph engine (it issues `GraphRevision` objects and serves compiled graphs through the shared runtime; see `pkg/controller/resourcegraphdefinition/controller_reconcile.go`). Features marked as "Planned" or "Not yet implemented" (such as KREP-006 `propagateWhen` and lifecycle signals) remain future work. Remaining design claims may still drift.

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

> **These examples are illustrative.** They are shipped to demonstrate patterns, not as guaranteed
> ready-to-run manifests, and not all of them necessarily compile or run as-is pending follow-up.
> For instance, the RGD example generates a child Graph whose spec can be invalid even while the
> outer Graph is valid (nested Graphs reconcile independently — see Nested Graphs), and the CoreDNS
> example embeds inline Corefile `health { ... }` / `forward { ... }` blocks that a given pinned
> CoreDNS image may reject. Treat them as starting points to adapt, not as tested deployments.
>
> **Prerequisite — an applier ServiceAccount and its RBAC.** A standalone Graph applies its
> resources under an **impersonated ServiceAccount** in the Graph's own namespace, not the kro
> controller identity (`pkg/controller/graph/impersonation.go` — `serviceAccountUsername`). By
> default that is the namespace's `default` ServiceAccount; override it with
> `spec.serviceAccountName`. That ServiceAccount must hold the RBAC (Roles/ClusterRoles +
> bindings) for every resource type the Graph creates, patches, reads, or prunes — the linked
> examples do **not** ship that ServiceAccount or its RBAC, so you must provision it (and grant the
> kro controller the `impersonate` verb) before a Graph will apply. See
> [Security Posture](#security-posture) for the full model.

### What this KREP covers

**Proposed:** The Graph Kind — node types (`template`, `patch`, `ref`, `graph`, `def`), dependency
inference from CEL expressions, the evaluation model (nodes evaluate when hard dependencies are in scope,
not when they are ready), status conditions (`Accepted`, `ResourcesConverged`, `Ready`), and nested composition. These are
new primitives that do not exist in KRO today.

**Inherited unchanged from RGD:** `includeWhen`, `forEach`, `readyWhen`, and CEL expression syntax.
These mechanisms carry forward with the same semantics and are not redefined by this KREP.

**Defers to KREP-006 (Planned / Not yet implemented):** `propagateWhen` gating semantics, `.ready()` and `.updated()` lifecycle
signals, collection-level rollout strategies, and budget syntax. These features are deferred to KREP-006 and are not yet implemented in the engine.

**Relationship to RGD:** Graph and RGD are no longer sibling engines. As shipped, the RGD controller
has been migrated onto the graph engine unconditionally: every RGD compiles to `GraphRevision`
objects and its instances are reconciled through the shared graph runtime
(`pkg/controller/resourcegraphdefinition/controller_reconcile.go` — the reconcile loop issues a
`GraphRevision` via `createGraphRevision` and serves the compiled graph). RGD remains a
user-facing Kind with unchanged external behavior, but internally it _is_ a graph. Graph is still
additive at the API surface — it does not replace or deprecate the RGD Kind — but the two share one
engine rather than two.

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

`spec.serviceAccountName` (optional) selects which ServiceAccount in the Graph's own namespace kro
impersonates when applying the Graph's resources; when unset, the namespace's `default`
ServiceAccount is impersonated. See [Security Posture](#security-posture) for the full model.

### Node Types

Graph has five node types: `template`, `ref`, `def`, `graph`, and `patch`. Each is declared by a keyword on the node object. We choose explicit
keywords over fewer types with strategy flags so that a reader knows what a node does — and what
type it produces in scope — at a glance from the top-level keyword. Exactly one type keyword must be set per node.

#### `template:`

Creates and owns a Kubernetes resource via server-side apply. On the RGD/instance path the shared
field manager `kro.run/applyset` is used with force ownership; on the standalone Graph path a
per-Graph field manager is used with conflict detection (see Application & Field Management below).
On deletion of the Graph, the resource is deleted.

#### `patch:`

Contributes fields to a resource you don't own. The resource must already exist. `patch:` is authored
exactly like `template:` — a raw partial manifest, with no wrapper around the contributed fields. The
node applies that manifest via server-side apply under a dedicated per-node field manager
(`kro-graphengine.patch.<graph>.<node>`). On prune, your contributed fields are released (relinquished by
applying an empty object under that manager) — the target resource itself is not deleted.

A patch node's manifest declares `apiVersion`, `kind`, `metadata` (`name` required, `namespace`
optional — identity only, not a contribution), and the contributed fields at the top level, same as
`template:`. There is no explicit `subresource:` field; the target endpoint is **derived from field
presence**:

- A top-level `status:` field targets the status subresource.
- `spec:`, `data:`, or other top-level fields, or `metadata.labels`/`metadata.annotations`, target the
  main resource.
- A single patch node may not mix `status:` with main-resource fields — they target different API
  endpoints. If a resource needs both, split into two patch nodes.
- The scale subresource is not supported.

Note that `forEach` is not supported on patch nodes.

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

- `forEach`: Supported on `template`, `def`, and `patch` nodes. On `template`/`def` it stamps one instance per element (or cartesian product across multiple dimensions); on `patch` it fans the same contribution out across every rendered target (each must resolve to a distinct name). Explicitly rejected on `graph` and `ref` nodes.
- `includeWhen`: Supported on `template`, `ref`, `def`, and `patch` nodes. When false, the node is skipped (and template resources are pruned). The skip is contagious — nodes depending on a skipped node are skipped too, rather than erroring on the missing reference. Rejected on `graph` nodes.
- `readyWhen`: Supported on `template`, `ref`, `def`, and `patch` nodes. Evaluated against scope to determine whether the node is ready. Rejected on `graph` nodes.
- `propagateWhen`: Planned / Not yet implemented (deferred to KREP-006).

| Modifier        | Supported Node Types             | Question it answers       | When false                           | Status / Defined in  |
| --------------- | -------------------------------- | ------------------------- | ------------------------------------ | -------------------- |
| `includeWhen`   | `template`, `ref`, `def`, `patch`| Should this node exist?   | Prune — resource deleted / skipped   | Shipped (KREP-008)   |
| `readyWhen`     | `template`, `ref`, `def`, `patch`| Is this node healthy?     | Signal only — Graph not Ready        | Shipped (from RGD)   |
| `forEach`       | `template`, `def`, `patch`       | How many instances?       | N/A (expands node)                   | Shipped (KREP-002)   |
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
   - `template:` nodes apply their desired manifest via Server-Side Apply (SSA). On the RGD/instance path the shared field manager `kro.run/applyset` is used with force ownership. On the standalone Graph path a **per-Graph** field manager `kro-graphengine.tmpl.<graphSegment>` (e.g. `kro-graphengine.tmpl.d2ba416cfd76`, where `<graphSegment>` is derived from the Graph UID) is used **without** force so the API server reports a field-level conflict. Note the manager is keyed on the Graph UID **only**, not the node: every node of one Graph shares a single template manager, so ownership stays stable across a node rename (SSA narrows the manager's field set instead of orphaning the retired node's fields). The peer-vs-drift decision reads the 409's own conflict causes: a field owned by a _peer Graph's_ template manager is never stolen (the node is held soft not-ready), while external drift (a human or another controller) is reclaimed with force. Two nodes of the same Graph can never legitimately co-own one object — the identity-claim guard rejects that before any write — so there is no same-Graph self-conflict case.
   - `patch:` nodes apply contributed fields under a dedicated per-node field manager (`kro-graphengine.patch.<graph>.<node>`). Force behavior **differs by target endpoint** (`pkg/graphengine/executor/simple.go:1495` `contributeApply`):
     - A **status-subresource** patch (a top-level `status:` field) always applies **with** force ownership (`client.ForceOwnership`, `simple.go:1497`). Status writeback must reclaim status fields from a legacy `Update`-manager takeover (the pre-SSA controller wrote status via `Update`, which leaves fields owned by the `before-first-apply`/manager-update identity); without force the first status apply would 409 forever. SSA scopes the force to only the fields this manager sets.
     - A **main-resource** patch (`spec:`, `data:`, `metadata.labels`/`annotations`, etc.) applies **without** force (`simple.go:1499`), so the API server reports a field-level 409 rather than silently stealing a field owned by a human, another controller, or a peer Graph — the caller surfaces that as soft not-ready. The one exception is a conflict with this Graph's own stale patch identity (a re-keyed node, or a legacy pre-segment manager), which is force-reclaimed since unforced it would deadlock forever.
   - The ownership implication: a status patch will _take over_ status fields another manager currently owns, while a main-resource patch is cooperative and will not. On delete or prune, releasing a contribution applies an empty object under that manager to relinquish field ownership without deleting the target object.
4. **Reconciliation Parallelism:** Within a single Graph, nodes evaluate serially in topological order; collection instances evaluate in bounded parallel (default ApplyConcurrency=20). Across Graphs, reconciliation is fully parallel via controller-runtime's work queue.

_(Note: `propagateWhen` gating and `.ready()` lifecycle signals are deferred to KREP-006 and not yet implemented in the engine.)_

### Nested Graphs

Graph supports two forms of nesting:

1. **Inline Subgraphs (`graph:` node):** An explicit `graph:` node embeds a child `GraphSpec` inline. The compiler compiles this into a child `SubProgram` frame with lexical scoping. Ancestor node references are captured as dependencies of the subgraph node, while expressions inside the subgraph cannot mix frames.
2. **Stamping Graph Custom Resources (`template:` node with `kind: Graph`):** A parent Graph can stamp child `Graph` custom resources into the cluster (for example, combined with `forEach`). The child Graph is applied as an independent Kubernetes object and reconciled **asynchronously** by the Graph controller — the parent's apply of the child object completes as soon as the object exists, and the child then compiles and converges on its own reconcile.

> **Revision history is not implemented for standalone/nested Graphs.** Only the RGD controller
> issues `GraphRevision` objects (via `createGraphRevision` in
> `pkg/controller/resourcegraphdefinition/`). The standalone Graph controller
> (`pkg/controller/graph/`) keeps **only an in-memory compiled-program cache** — a
> `registry.Registry` keyed by `(namespace, name)` and a spec hash
> (`pkg/controller/graph/controller.go:74`, `:268`) — and creates no `GraphRevision` objects. So
> the KREP-013 claim that "each nested Graph gets independent revisions" is a not-yet-built
> aspiration: nested and standalone Graphs have no persisted revision history, only the in-memory
> program cache.

> **Asynchronous nesting has no automatic parent-waits-for-child gating.** Because a stamped child
> reconciles on its own loop, a parent's `ResourcesConverged`/`Ready` reflects only the parent's own
> nodes — the parent can report `Ready` while a stamped child is still invalid, compiling, or
> unready. There is no cross-Graph readiness edge in `pkg/controller/graph/`. If a parent's readiness
> must depend on a child's, that has to be wired explicitly: the parent must `ref:` the stamped
> child Graph object and put its `Ready` condition into a `readyWhen` expression. Nothing gates the
> parent on the child automatically.

#### Deferral Boundaries for Stamped Graphs

When stamping child Graph resources via a `template:` node, child CEL expressions live as literal strings inside the parent's template. The shipped scanner (`pkg/graph/parser/cel.go` — `extractExpressions`) tracks single- and double-quoted string literals and only permits a nested `${` when it is **immediately preceded by a quote character** (`'` or `"`); a bare nested `${...}` is rejected (`ErrNestedExpression`), and a `${` that never closes before end-of-input is rejected (`ErrUnterminatedExpression`). The supported deferral forms follow directly from that scanner:

- `${...}` — evaluated by the current (parent) Graph.
- `${"${...}"}` (or `${'${...}'}`) — the inner `${` sits inside a quoted string literal, so the parent parses the whole thing as **one** expression: it evaluates the CEL string literal `"${...}"` to produce the literal text `${...}`, which the child Graph then evaluates at its own scope. This is the two-level form.
- Three levels nest the same way, one quoted layer per level: `${"${'${...}'}"}`. Each layer's `${` is guarded by the quote that opens the literal it lives in; the parent peels the outermost quoted layer, the child peels the next, and so on.

Because the guard is purely "a nested `${` must be preceded by a quote," every additional level of deferral is exactly one more layer of string quoting. A bare `${outer(${inner})}` (no quotes around the inner `${`) does not parse.

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

Unlike cluster-scoped `ResourceGraphDefinition`, `Graph` is a namespaced, user-creatable kind whose
executor performs writes (including cross-namespace writes, cluster-scoped RBAC creation, foreign
Secret reads, and prune). To keep that power from defaulting to the kro controller's own broad
identity, a Graph's resources are applied **under an impersonated ServiceAccount**, not the
controller service account.

**Impersonation model.** The Graph controller applies each Graph's resources while impersonating a
ServiceAccount resolved in the Graph's **own namespace**
(`system:serviceaccount:<graph-namespace>:<name>`):

- `spec.serviceAccountName`, when set, selects which ServiceAccount in the Graph's namespace to
  impersonate.
- When unset, kro impersonates that namespace's `default` ServiceAccount, confining resource access
  to the namespace by default.
- The ServiceAccount is **always** resolved in the Graph's own namespace, so a Graph can never
  escalate beyond the RBAC granted to a ServiceAccount a caller in that namespace could already use.
  A Graph author cannot name a ServiceAccount in another namespace.

**Trust model: equivalent to `create pod`.** The consequence is that permission to create or update a
`Graph` in a namespace is permission to act as **any ServiceAccount in that namespace kro is allowed to
impersonate** (delete included — teardown runs under the same identity). This is deliberately the same
trust boundary Kubernetes already has for Pods: anyone who can create a Pod (directly or via a
Deployment/Job) can set `spec.serviceAccountName` to any SA in the namespace and run as it. `Graph`
is therefore no more privileged than existing `create pod` access — the namespace is the boundary.
Operators narrow it further with kro's **own** RBAC: granting the `impersonate` verb per-namespace and
with `resourceNames` restricts which SAs a Graph can ever use (a Graph naming a non-impersonable SA
fails to apply rather than escalating). This parity, and the recommendation to treat Graph mutation as
a privileged grant, is documented for users on the kro website's Access Control page
(`website/docs/docs/advanced/01-access-control.md`).

Mechanically, the controller derives a per-identity controller-runtime client from the manager's REST
config with `rest.ImpersonationConfig` set to the resolved username, cached one entry per distinct
ServiceAccount. For this to take effect the **kro controller ServiceAccount must be granted the
`impersonate` verb on `serviceaccounts`** (and, transitively, the ability to act as those accounts).
Where impersonation is not wired (e.g. unit tests), the executor falls back to the controller
identity.

**Controller self-impersonation guard.** The one identity the namespace confinement does _not_
naturally protect against is the kro controller's **own** ServiceAccount: a Graph created in the
controller's namespace could name the controller SA (or its `default` could resolve to it) and thereby
apply resources under the controller's own broad identity — turning `create graphs` in that namespace
into an escalation to the controller's privileges. The controller therefore knows its own identity
(via `--controller-namespace`/`--controller-service-account`, wired from the downward API and the
ServiceAccount name in the Helm chart) and **refuses** any Graph whose resolved impersonation username
equals it, marking the Graph `Accepted=False` (reason `InvalidGraph`) before compile or apply. This
guard covers only the controller's _own_ SA; any _other_ privileged ServiceAccount reachable in a
namespace remains the operator's responsibility to scope via RBAC (and, if desired, by restricting the
controller's `impersonate` grant with `resourceNames`).

Because `Graph` is a privileged, user-creatable kind, it must **not** be aggregated into the built-in
Kubernetes user roles (`edit`, `admin`, `view`). Access to create or manage `Graph` resources must be
explicitly granted via separate, dedicated roles and is gated behind the `GraphKind` feature gate
(see the Helm `user-cluster-role.yaml` configuration).

This inverts the earlier posture: a Graph's blast radius is now bounded by a namespace ServiceAccount
by default rather than the controller identity. Remaining follow-ups are finer-grained credential
scoping — e.g. short-lived/bound tokens and caller-credential propagation — across KRO primitives.

## Relationship to Existing KREPs

| KREP                            | Relationship                                                                                                                                                             |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| KREP-001 (Status Conditions)    | System conditions (`Accepted`, `ResourcesConverged`, `Ready`) exist on Graph objects — never on user resources. Users define their own status via `patch:` nodes.       |
| KREP-002 (Collections)          | Adopted with safety limits (`MaxCollectionSize = 1000`, `MaxCollectionDimensions = 10`). Supported on `template` and `def` nodes.                                      |
| KREP-003 (Decorators)           | A Decorator is naturally a Graph with `ref:` (selector) + `forEach`. No special runtime support needed.                                                                  |
| KREP-006 (Propagation Control)  | Planned / Not yet implemented: `propagateWhen` gating and lifecycle signals (`.ready()`, `.updated()`) are deferred to KREP-006 and not yet implemented in the engine. |
| KREP-008 (includeWhen)          | Graph implements `includeWhen` as a first-class modifier across `template`, `ref`, `def`, and `patch` nodes. Dependency inference works naturally.                      |
| KREP-011 (Variables)            | `def:` is Graph's implementation. Same semantics.                                                                                                                        |
| KREP-013 (Graph Revisions)      | Applies to RGD only. The RGD controller issues `GraphRevision` objects; **standalone/nested Graphs have no persisted revision history** — only an in-memory compiled-program cache (see Nested Graphs). Independent per-nested-Graph revisions are a not-yet-built aspiration. |
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
