# Graph Reconciliation

How the controller reconciles a Graph. The DAG is the dependency structure between nodes. Watches
bring external state in. Performance is structural — work is proportional to change, not to DAG
size. Changes propagate forward through the DAG and stop when they stop mattering.

## Trigger

A Graph reconciles on:

- **Changes** — detected via watch. This includes resources referenced by nodes and the Graph
  itself. When the Graph spec changes, all nodes are triggered — the DAG structure may have changed.
- **Resync** — per-node, jittered; corrects configuration drift (see [Resync](#resync)). On startup,
  all resync timers are reset.

Zero triggers → no work; the controller schedules the next reconcile at the earliest resync.

Deterministic errors (4xx) are not retried — same inputs produce the same failure. They resolve via
changes or resync. Transient errors (5xx) retry with exponential backoff [1s, resyncInterval].

## Reconcile

Reconcile is two walks of the same algorithm —
[Kahn's](https://en.wikipedia.org/wiki/Topological_sorting#Kahn's_algorithm) topological sort over
the DAG. Propagation walks forward: evaluates triggered nodes, publishes results to scope. Prune
walks in reverse: removes resources absent from the desired set. Independent nodes in each walk's
frontier process concurrently. Work concentrates at triggered nodes — the rest of the graph resolves
in O(1) per node.

### DAG

The Graph spec compiles into a DAG — one node per resource declaration, edges inferred from
expression references. Node types (Own, Watch, WatchKind, Contribute, Definition) are defined in
[001-graph](001-graph.md) and [003-ownership](003-ownership.md). There is always exactly one target
DAG, produced from the latest revision (see [002-revisions](002-revisions.md)). When the spec
changes, a new revision is compiled and all nodes enter the frontier. There is no concurrent
convergence across revisions within a single Graph — in-progress evaluation of the previous revision
is abandoned. Partially applied resources either match the new revision's templates (kept) or don't
(pruned). The Graph boundary is where convergence independence is introduced: separate Graphs
reconcile independently, each with their own scope, revision, and watches.

### Scope

Nodes communicate through a scope — the graph's resolved data keyed by node ID. After a node is
resolved, its output is published to the scope. Scope is the single source of data for template
evaluation, readyWhen, propagateWhen, and includeWhen — if it's not in scope, the expression can't
see it. Workers receive read-only views of the scope containing only their dependencies' outputs.

### Node States

Each node's evaluation resolves to exactly one state:

| State       | Meaning                        | Dependents | Resolution                          |
| ----------- | ------------------------------ | ---------- | ----------------------------------- |
| Ready       | Applied, readyWhen satisfied   | Proceed    | —                                   |
| NotReady    | Applied, readyWhen unsatisfied | Proceed    | Converges via watch                 |
| Pending     | Data not yet available         | Pending    | Upstream resolves                   |
| Excluded    | includeWhen false              | Excluded   | includeWhen inputs change           |
| Blocked     | Dependency in error state      | Blocked    | Dependency resolves                 |
| Conflict    | Field ownership contested      | Blocked    | Propagation, revision, or resync    |
| Error       | Client request failed (4xx)    | Blocked    | Propagation, revision, or resync    |
| SystemError | Server/infra failure (5xx)     | Blocked    | Exponential backoff, caps at resync |

Ready and NotReady are both "applied and in scope." readyWhen is a health signal — it does not gate
dependents. Pending and Blocked both represent uncertain absence — previous applied keys are
retained, not safe to prune. Excluded propagates as Excluded (definitive absence — safe to prune).

Definition nodes can be Ready, NotReady (readyWhen unsatisfied), or Error (CEL evaluation failure).
They cannot be Pending (no resource to wait for), Conflict (no SSA), or SystemError (no API calls).

### Propagation

The forward walk extends Kahn's with change detection — a frontier of nodes eligible for processing,
seeded by level-0 nodes (no in-graph dependencies) and any node with an external trigger (watch
event or resync). Independent nodes in the frontier evaluate concurrently. Nodes that don't need
evaluation **skip** — they retain their previous state and scope entry.

A node enters the frontier when all its dependencies have been processed AND either:
- a dependency's output changed (propagation trigger), or
- the node has its own external trigger (watch event or resync)

After processing, a node's output-hash determines whether dependents receive a propagation trigger.
Unchanged output → dependents without their own triggers don't enter the frontier. This narrows the
walk to the affected subgraph. SSA is idempotent — no-diff applies don't bump resourceVersion, so
externally triggered nodes that re-evaluate to the same state cause no churn.

At each frontier node:

1. **Dependencies**
   - any dep Excluded → Excluded
   - any dep Blocked/Error/Conflict/SystemError → Blocked
   - any dep Pending → Pending
   - Precedence: Excluded > Blocked > Pending. Excluded is definitive — the dependency is
     intentionally absent, so the node cannot evaluate regardless of other dependencies' states.

2. **Propagation allowed**
   - any dep's propagateWhen unsatisfied → skip. Previous evaluation and state retained. If never
     evaluated, the node remains Pending — its output is genuinely unavailable, not stale.
   - Takes precedence even on spec changes where all nodes enter the frontier

3. **Inputs changed**
   - input-hash match + resourceVersion unchanged → skip
   - input-hash match + resourceVersion changed → GET live object, re-evaluate readyWhen, check
     output-hash. Template not re-evaluated.
   - Watch/WatchKind with external trigger → continue (output depends on cluster state)
   - Watch/WatchKind without external trigger → skip
   - Resync → continue

4. **includeWhen**
   - includeWhen == false → Excluded
   - Evaluated after input-hash because it depends on the same inputs — unchanged inputs → unchanged
     inclusion

5. **Resolve**
   - Watch: GET full object. Data enters scope. Pending if absent.
   - WatchKind: list matching objects by label selector. List enters scope (supports `.filter()`,
     `.map()`, etc.). When a single resource changes, update the cached list incrementally rather
     than re-listing — O(1) per event, not O(matching).
   - forEach parent: evaluate collection, determine children, dispatch changed children (see
     [forEach](#foreach)).
   - Definition: resolve all values in the template against the current scope. No API calls.
   - Own: evaluate template, hash desired state (apply-hash), compare against previous. Match → omit
     write. Resync bypasses — apply unconditionally. Differs → SSA apply. 409 → Conflict.
   - Contribute: same as Own. 409 → Conflict. Auto-splits status subresource.

   The apply-hash within Resolve is the third hash layer — it skips the SSA write when the desired
   state is unchanged. When a template targets both the main resource and the status subresource,
   the controller splits the apply into two operations.

6. **Result changed**
   - Evaluate readyWhen → Ready or NotReady. Publish to scope.
   - output-hash == previous → dependents don't enter the frontier
   - output-hash != previous → dependents enter the frontier (propagation trigger)

Three hashes at progressively deeper layers: input-hash (step 3) skips template evaluation,
apply-hash (step 5) skips the write, output-hash (step 6) determines propagation.

### Prune

After propagation determines the desired set, prune removes resources that should no longer exist.

The applied set is a live view derived from identity labels in the controller's informer stores —
all resources where the Graph's identity label exists. Not persisted. Hydrated on startup from
informer list and kept current by watch events. Both Own and Contribute targets are in the applied
set; the label value (`own` or `contribute`) determines the prune action.

Prune candidates are the set difference: resources in the applied set minus the current reconcile's
output set. Revision transitions, includeWhen toggles, and forEach scale-down all produce prune
candidates through this diff. A resource is prunable if its absence is definitive (Excluded, removed
from revision). Uncertain absence (Pending, Blocked, Error, SystemError) blocks pruning — the
resource might reappear once the blocker resolves. Conflict is excluded from the prune gate: a 409
is positive evidence that the resource exists. Prune is the recovery path for conflicts during
revision transitions — the old revision's resource is removed, the new creates it fresh without
contested field ownership.

Prune walks the DAG in reverse — Kahn's with reversed edges. The frontier is seeded by leaf nodes
(no in-graph dependents); as each node is removed, its dependencies enter the frontier when all
their dependents have been processed. Own → delete. Contribute → release fields via skeleton SSA
apply (omit managed fields, relinquishing ownership; see [003-ownership](003-ownership.md)).
Watch/WatchKind → no action. Independent nodes in the frontier can be removed concurrently. If the
DAG is unavailable, prune is blocked — never degrade to unordered deletion. If another node declares
`finalizes` targeting a prune candidate, finalization runs first (see
[Finalization](#finalization)).

Reverse dependency ordering comes from the most recent revision that defined the resource.
Superseded revisions must be retained until their unique resources are pruned — they carry the
ordering and finalization metadata for those resources. The old revision's `finalizes` declarations
govern the prune of its resources — if a new revision changes or drops `finalizes`, the old
revision's metadata still applies to resources being pruned from it.

#### Teardown

When a Graph is deleted, every node becomes a prune candidate — the prune algorithm above runs in
full. Ordering comes from the active revision's DAG (distinct from reconcile-time prune, where
ordering comes from the superseded revision that defined the pruned resources). If the revision was
deleted (ownerReference cascade race), the controller regenerates the DAG from spec. Teardown is
blocked until ordering is available — never degrade to unordered deletion. If resources persist
(finalizers), requeue. Once all managed resources are pruned, remove the Graph's finalizer.

#### Finalization

When another node declares `finalizes` targeting a prune candidate's resource, deletion is gated on
the finalizer resource completing. `finalizes` introduces two behaviors that do not emerge from the
DAG:

- **Creates during prune** — the finalizer resource does not exist during normal operation. It
  materializes when the target becomes a prune candidate.
- **Inverts deletion ordering** — normally, dependents are deleted before dependencies. `finalizes`
  inverts this for the target/finalizer pair: the target is deleted before the finalizer resource.

The sequence within a prune walk:

1. The prune walk encounters the target. The controller creates the finalizer resource — the target
   is still fully operational, no `metadata.deletionTimestamp`. This matters: setting
   `deletionTimestamp` can trigger the target's own controller to start destroying underlying
   infrastructure before the finalizer resource has a chance to act. The finalizer resource's key is
   added to the applied set.
2. The finalizer resource reaches readyWhen. If multiple finalizer nodes target the same resource,
   dependencies among them determine ordering — all must be Ready before proceeding.
3. The controller issues DELETE on the target.
4. The prune walk continues. The finalizer resources are in the applied set but not in the desired
   state — they are prune candidates. The walk picks them up and deletes them in reverse dependency
   order.

Finalization state is fully recoverable from spec, applied set, and cluster state — no state machine
needed. On crash, the next reconcile re-derives position: the applied set identifies which finalizer
resources were created, the cluster reveals whether they exist and satisfy readyWhen, and the spec
provides the `finalizes` relationships. SSA idempotency covers re-creation.

Prune ordering must account for finalizer resource dependencies beyond the target — resources
referenced by an in-flight finalizer are deferred until finalization completes, even if they're in a
different branch of the normal DAG.

Side effects from completed finalizer resources are not rolled back on partial failure. If one
finalizer reaches Ready but a sibling fails, the completed finalizer's effects persist. Finalization
is not transactional.

| Condition                                                                           | Behavior                                | Status             |
| ----------------------------------------------------------------------------------- | --------------------------------------- | ------------------ |
| Target absent (creation failed, deleted externally)                                 | Skip finalization, proceed with cleanup | `FinalizerSkipped` |
| Finalizer can't be created (dependency failure, admission, quota, invalid template) | Block target deletion                   | `TeardownBlocked`  |
| Finalizer created but never reaches readyWhen                                       | Block target deletion                   | `TeardownBlocked`  |

`TeardownBlocked` is not a skip — the target has data the user intended to finalize. The condition
message distinguishes creation failure from readyWhen failure. To unblock: update the Graph spec to
remove or fix the finalizer resource. The revision transition prunes the orphaned finalizer resource
and deletes the target without finalization.

## forEach

A forEach node is a parent that expands into child nodes — one per item in a collection. The parent
is a logical node (no managed resource). Children are nodes — all existing per-node machinery
applies.

```
                                forEach parent
                                ┌──────────┐
    ${apps} ────────────────────▶│ deploys  │  (logical — no managed resource)
                                │ (parent) │
                                └────┬─────┘
                  ┌──────────────────┼──────────────────┐
                  ▼                  ▼                  ▼
     ┌──────────────────┐ ┌──────────────────┐ ┌──────────────────┐
     │    frontend      │ │    backend       │ │    worker        │
     │     (child)      │ │     (child)      │ │     (child)      │
     └──────────────────┘ └──────────────────┘ └──────────────────┘
```

### Child Identity

A child's identity is scoped to its parent and encodes the full resource key as DNS subdomain labels
within the label key:

    <parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/reference

For example — Deployment `frontend` in namespace `default`, parent `deploys`, graph `mygraph` in
namespace `default`:

    deploys.frontend.default.deployment.apps.mygraph.default.internal.kro.run/reference

This is the same label key structure as any node — the parent ID is the first label, followed by the
resource key components as additional DNS labels before the graph identity. A non-forEach node
`config` produces `config.mygraph.default.internal.kro.run/reference`. The label prefix is a DNS
subdomain (253-character limit); graph names and non-forEach node IDs are single DNS labels, forEach
children extend the prefix with additional labels. Uniqueness is across the full resource key (GVK +
namespace + name). If the rendered key changes, that's a new child — the old one is a prune
candidate. Resource keys must be unique across children of the same parent — validated at expansion
time.

### Parent Expansion

The parent evaluates the collection and determines which children exist. A logical node that expands
into children at walk time.

1. **Evaluate collection** — CEL expression produces a list. An empty collection produces zero
   children — the parent enters scope as `[]` and is Ready (readyWhen is per-child; zero children
   means vacuously satisfied).
2. **Render identity** — per item, bind the iterator variable and resolve the identity fields:
   `apiVersion`, `kind`, `metadata.name`, and `metadata.namespace`. Any of these may be CEL
   expressions. If any identity expression cannot evaluate — upstream dependency Pending, CEL type
   error, nil dereference — the parent is Pending (upstream not ready) or Error (expression
   failure). Expansion does not proceed and existing children persist. Partial expansion is never
   attempted.
3. **Dispatch children** — each child evaluates its template like any node, with the iterator
   variable bound to the collection item. readyWhen expressions are evaluated per-child — within
   readyWhen, `${deploys}` binds to the individual child's managed resource. In all other contexts
   (downstream templates, scope), `${deploys}` is the parent's aggregated list.
4. **Aggregate** — the parent collects child scope entries into a list and enters scope. The parent
   does not wait for readyWhen — downstream nodes proceed as soon as child data is in scope.

### Parent State

The parent's state is derived from its children:

- **Pending** — any child has not yet been dispatched or is awaiting its first result
- **Ready** — all children Ready
- **NotReady** — any child NotReady, none in error states

The parent's propagateWhen is satisfied when all children's propagateWhen are satisfied.

- **Error/Conflict/SystemError** — any child in an error state. Error states take precedence over
  Pending — a child that attempted apply and got a Conflict is in Conflict state, not Pending.
  Deterministic errors (Error) take precedence over transient errors (SystemError, Conflict) — if
  any child's failure is deterministic, retrying cannot resolve the parent. Per-child detail
  surfaces in Graph status.

### Collection Ordering

The list downstream nodes receive matches expansion order — deterministic given the same input
collection. Observed state is aligned to expansion order by resource key during aggregation. If the
input reorders, the downstream list reorders — but each child's identity (resource key) is
unchanged. Index-sensitive downstream CEL (`${deploys[0]}`) is fragile unless the collection is
explicitly sorted.

### Prune (Scale-Down)

A removed collection item means a child that existed last cycle is absent this cycle. The child's
managed resource is in the previous applied set but not the current one — standard prune candidate.
Prune in reverse dependency order. If the forEach node itself is removed from the spec (revision
transition), children are pruned before the parent is removed from the DAG. No forEach-specific
scale-down logic — the applied set diff handles it.

If the collection expression cannot evaluate (upstream is Pending), the intended child set is
unknown — prune is blocked and existing children persist until the next successful evaluation.

## Optimizations

### Hash Mechanics

The input-hash and output-hash are ephemeral — recomputed on cold start. The apply-hash is persisted
as an annotation (`internal.kro.run/template-hash`); recomputing it requires re-applying via SSA, so
without it cold start produces an N-write burst. Resync bypasses the apply-hash (apply
unconditionally) but the output-hash still applies — if corrected output matches previous output,
dependents are not added to the frontier.

Input-hash and output-hash share the same path extraction. At graph compilation, the controller
walks each expression's AST to extract reference chains — sequences of select operations rooted at a
scope variable. `${deploy.status.availableReplicas}` yields `(deploy, status.availableReplicas)`.
`${a.spec.x + b.data.y}` yields `(a, spec.x)` and `(b, data.y)`. When a chain contains a dynamic
operation (function call, index, comprehension), the path terminates at the last static select and
the value at that prefix is hashed in full:
`${deploy.status.conditions.filter(c, c.type == 'Available')[0].status}` yields
`(deploy, status.conditions)`. Absent paths hash to a sentinel — absent to present is a change, not
a skip.

### Resync

Each node has an in-memory resync timer with a jittered interval (default 30 minutes) — a
consistency floor bounding how long any divergence can persist. Watches can miss divergence
(disconnects, cache staleness); hash-matched skips don't detect server-side drift (external edits,
admission webhooks). On expiry, the node is triggered and applies unconditionally.

SSA is idempotent; apply corrects drift as a side effect and resets the timer. A skipped node does
not reset its timer — frequent reconciles from other nodes' events don't perpetually push it
forward. Jitter decorrelates timers across nodes. On restart, timers start fresh — bounded burst
(10k nodes over 30 minutes ≈ 5.5 applies/sec). Resync timer state is in controller metrics, not on
managed resources.

### Storage Model

Each managed resource carries two labels per Graph-node pair. The identity is encoded in the label
prefix using DNS subdomain structure. The controller uses metadata-only informers — spec and status
are not fetched. Full object reads happen only during Resolve. When an evaluated node needs data
from a skipped dependency, the full object is read from the API server on demand. If absent (deleted
externally, not yet created), the node is Pending.

| Label key                                         | Value                       | Purpose                              |
| ------------------------------------------------- | --------------------------- | ------------------------------------ |
| `<node>.<graph>.<ns>.internal.kro.run/reference`  | `own` or `contribute`       | Identity, selection, prune reference |
| `<node>.<graph>.<ns>.internal.kro.run/generation` | `graph.metadata.generation` | Observational                        |

Each Graph gets its own label keys — multiple Graphs targeting the same resource coexist without
collision. Label count scales linearly with managing Graphs (2N labels for N Graphs). The identity
label enables selection (see [Prune](#prune)) and routes watch events to the correct
node via the node ID encoded in the label key.

The controller's operational inputs are the informer store and the DAG. Revision status is a
write-only observation surface — not an operational input.

## Why Not

**Periodic full-graph resync.** Informer resyncs trigger all nodes simultaneously — correlated,
expensive. Per-node resync with jitter amortizes the cost across reconciles. Simpler scheduling, but
more moving parts than a single resync interval.

**Bespoke object caching for partial evaluation.** Caching full objects per node between reconciles
enables skipping nodes entirely, but introduces coherence obligations — the cache can go stale, and
the controller is responsible for invalidation. Dirty propagation through the full graph achieves
the same performance (work proportional to change) without maintaining object state. Informer stores
provide the read path using standard Kubernetes watch machinery.

**Explicit subgraph scoping per trigger.** Maintaining a walk scope per trigger — tracking which
subgraph to visit, restoring previous state for out-of-scope nodes — achieves the same result as the
output-hash frontier pruning but with more bookkeeping. The output-hash achieves scoping as a side
effect of change detection: nodes whose inputs didn't change aren't visited because no dependency's
output changed. Same result, no explicit scope tracking.

**Continuous drift correction.** Apply unconditionally on every reconcile. Catches drift immediately
but imposes an N-write steady-state tax. Per-node resync with jitter corrects drift within the
interval at near-zero steady-state cost.

**forEach as a single node with internal iteration.** The forEach handler reimplements scheduling,
state tracking, readyWhen, and change detection inside a mini-coordinator — a parallel system
alongside the DAG walk. Parent-child makes children real nodes. One system.

**forEach children as dynamic siblings.** Children appear as peers in the DAG rather than under a
parent. No natural aggregation point for downstream consumers — the parent-child relationship has to
be reinvented as a convention. Aggregation is structural, not conventional.

**Index-based forEach identity.** Child identity derived from position in the collection. If the
collection reorders (API server returns namespaces in a different order), children churn — deletes
and recreates for what should be no-ops. Resource-key identity is stable under reordering.

**Content-hash forEach identity.** Child identity derived from a hash of the collection item. Any
field mutation changes the hash — a label change on a namespace produces a new child identity,
triggering delete and recreate. Resource-key identity changes only when the template's identity
fields (namespace, name) change.

**forEach as a subgraph stamper.** forEach expands into a subgraph of multiple resources per item.
Sibling forEach blocks and nested Graphs already compose for multi-resource-per-item cases — one
template per forEach keeps the primitive simple.
