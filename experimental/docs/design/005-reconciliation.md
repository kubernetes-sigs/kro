# Graph Reconciliation

Reconciliation operates on a [compiled Graph](004-compilation.md) — per-node compiled programs, field
path dependencies defining each node's input surface, and a DAG. When the
[revision](002-revisions.md) changes, the controller diffs it against the previous — nodes that
differ are triggered, removed nodes become prune candidates.

## Trigger

A Graph reconciles when its resources or the Graph itself change (detected via watch), and when
CRDs change (a referenced CRD's `metadata.generation` advances; the compiled artifact is stale on
the next reconcile; see [004-compilation](004-compilation.md#algorithm)).

Deterministic errors (4xx) are not retried — same inputs produce the same failure. They resolve via
new inputs. Transient errors (5xx) retry with exponential backoff.

## Reconcile

Reconcile is two walks — propagation (forward) and prune (reverse). Propagation walks the forward
DAG: evaluates nodes in dependency order, publishes results to scope. A node evaluates after its hard
dependencies have been processed. Prune walks the reverse DAG: removes resources absent from the
desired set.

Each Graph converges one revision at a time. When a new revision is compiled, in-progress evaluation
of the previous revision is abandoned — partially applied resources either match the new revision's
templates (kept) or don't (pruned). Multiple Graphs converge independently, each with their own
scope, revision, and watches.

### Scope

Nodes communicate through a scope — the graph's resolved data keyed by node ID. After a node is
resolved, its output is published to the scope. Scope is the single source of data for template
evaluation, readyWhen, propagateWhen, and includeWhen — if it's not in scope, the expression can't
see it. Workers receive read-only views of the scope containing their dependencies' outputs. Hard
dependencies are always present in the view (the node waited for them). Soft dependencies are
optional — `optional.of(object)` when available at dispatch time, `optional.none()` otherwise.

### Node States

Each node's evaluation resolves to exactly one state:

| State       | Meaning                        | Dependents | Resolution                          |
| ----------- | ------------------------------ | ---------- | ----------------------------------- |
| Ready       | Applied, readyWhen satisfied   | Proceed    | —                                   |
| NotReady    | Applied, readyWhen unsatisfied | Proceed    | Converges via watch                 |
| Pending     | Data not yet available         | Pending    | Upstream resolves                   |
| Excluded    | includeWhen false              | Excluded   | includeWhen inputs change           |
| Blocked     | Dependency in error state      | Blocked    | Dependency resolves                 |
| Conflict    | Field ownership contested      | Blocked    | Propagation or revision             |
| Error       | Client request failed (4xx)    | Blocked    | Propagation or revision             |
| SystemError | Server/infra failure (5xx)     | Blocked    | Exponential backoff                 |

Ready and NotReady are both "applied and in scope." readyWhen is a health signal — it does not gate
dependents. Errored nodes (Conflict, Error, SystemError) do not publish to scope — dependents become
Blocked. Pending and Blocked both represent uncertain absence — previous applied keys are retained,
not safe to prune. Excluded propagates as Excluded (definitive absence — safe to prune).

`def:` nodes can be Ready, NotReady (readyWhen unsatisfied), Pending (upstream dependency unresolved
— the CEL expression references scope data that is not yet available), or Error (CEL evaluation
failure). They do not produce Pending from their own execution (no API calls), but inherit it from
unresolved upstream dependencies. They cannot be Conflict (no SSA) or SystemError (no API calls).

The Graph's Ready condition (see [001-graph](001-graph.md#conditions)) rolls up node states into a
single signal. When multiple failure states coexist, precedence is SystemError > Error > Conflict >
Blocked > Pending > NotReady. SystemError surfaces first because it signals degraded infrastructure
— deterministic errors may be artifacts of system instability, not real spec problems.

### Propagation

The forward walk evaluates nodes in dependency order — a node evaluates after its hard dependencies
have been processed.

At each node:

1. **Dependencies**
   - Soft dependencies are always in scope as optional values. If any hard dependency is not in
     scope, the consumer cannot evaluate. The consumer inherits a state from the unavailable
     dependency. Precedence: Excluded > Blocked > Pending.

2. **includeWhen**
   - includeWhen == false → Excluded. Structural decisions (should this node exist?) take
     precedence over temporal decisions (is it safe to re-evaluate?).

3. **propagateWhen**
   - The node's propagateWhen unsatisfied → skip. The node's output is not published to scope for
     this evaluation. If never evaluated, the node remains Pending. Dependents of a skipped node
     are also skipped — no mixed-generation evaluation.
   - Takes precedence even on spec changes

4. **Resolve**
   - `ref:` — GET the named object. Data enters scope. Pending if absent.
   - `watch:` — list matching objects by label selector. List enters scope (supports `.filter()`,
     `.map()`, etc.).
   - forEach parent — evaluate collection, determine children, dispatch children.
   - `def:` — resolve all values against the current scope. No API calls.
   - `template:` — evaluate desired state, SSA apply. 409 → Conflict.
   - `patch:` — evaluate desired state, SSA apply to existing resource. Pending if target absent.
     409 → Conflict. Auto-splits status subresource.

   When a template targets both the main resource and the status subresource, the controller splits
   the apply into two operations.

5. **Result**
   - Evaluate readyWhen → Ready or NotReady. Publish to scope.

### Prune

After propagation determines the desired set, prune removes resources that should no longer exist.

The applied set is derived from [identity labels](003-ownership.md#identity-labels) — all resources
where the Graph's identity label exists. Resources written by both `template:` and `patch:` nodes
are in the applied set; the label value (`template` or `patch`) determines the prune action.

Prune candidates are the set difference: resources in the applied set minus the current reconcile's
output set. Revision transitions, includeWhen toggles, and forEach scale-down all produce prune
candidates through this diff. A resource is prunable if its absence is definitive (Excluded, removed
from revision). Uncertain absence (Pending, Blocked, Error, SystemError) blocks pruning — the
resource might reappear once the blocker resolves. Conflict is excluded from the prune gate: a 409
is positive evidence that the resource exists. Prune is the recovery path for conflicts during
revision transitions — the old revision's resource is removed, the new creates it fresh without
contested field ownership.

Prune walks the reverse DAG — the same algorithm as propagation, edges pointing from dependent to
dependency. The walk starts at leaf nodes (no dependents in the forward DAG = no dependencies in the
reverse DAG). `template:` → delete. `patch:` → [release apply](003-ownership.md#release-apply)
(omit managed fields, relinquishing ownership).
`ref:`/`watch:`/`def:` → no action. If the DAG is unavailable, prune is blocked — never degrade to
unordered deletion. If another node declares `finalizes` targeting a prune candidate, finalization
runs first.

Reverse dependency ordering comes from the most recent revision that defined the resource.
Superseded revisions must be retained until their unique resources are pruned — they carry the
ordering and finalization metadata for those resources. The old revision's `finalizes` declarations
govern the prune of its resources — if a new revision changes or drops `finalizes`, the old
revision's metadata still applies to resources being pruned from it.

#### Teardown

When a Graph is deleted — by its owner, by GC, or directly — every node becomes a prune
candidate — the prune algorithm above runs in full. Ordering comes from the active revision's DAG
(distinct from reconcile-time prune, where ordering comes from the superseded revision that defined
the pruned resources). If the revision was deleted (ownerReference cascade race), the controller
regenerates the DAG from spec. Teardown is blocked until ordering is available — never degrade to
unordered deletion. If nodes persist (finalizers), requeue. Once all nodes are pruned, remove the
Graph's finalizer.

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

| Condition                                                                           | Behavior                                |
| ----------------------------------------------------------------------------------- | --------------------------------------- |
| Target absent (creation failed, deleted externally)                                 | Skip finalization, proceed with cleanup |
| Finalizer can't be created (dependency failure, admission, quota, invalid template) | Block target deletion                   |
| Finalizer created but never reaches readyWhen                                       | Block target deletion                   |

Finalizer nodes use normal node states — Pending (can't create), Error (creation failed), NotReady
(readyWhen unsatisfied). These roll up through the standard Graph Ready condition. When teardown is
blocked, the condition message identifies the blocking finalizer node. To unblock: update the Graph
spec to remove or fix the finalizer resource. The revision transition prunes the orphaned finalizer
resource and deletes the target without finalization.

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

    <parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/type

For example — Deployment `frontend` in namespace `default`, parent `deploys`, graph `mygraph` in
namespace `default`:

    deploys.frontend.default.deployment.apps.mygraph.default.internal.kro.run/type

This is the same label key structure as any node — the parent ID is the first label, followed by the
resource key components as additional DNS labels before the graph identity. A non-forEach node
`config` produces `config.mygraph.default.internal.kro.run/type`. The label prefix is a DNS
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
- **Error/Conflict/SystemError** — any child in an error state. Error states take precedence over
  Pending — a child that attempted apply and got a Conflict is in Conflict state, not Pending.
  Deterministic errors (Error) take precedence over transient errors (SystemError, Conflict) — if
   any child's failure is deterministic, retrying cannot resolve the parent. Per-child detail
   (child resource GVK, namespace, name, and state) surfaces in the Graph's Ready condition message.

### Propagation Control

When a forEach node declares propagateWhen, it gates each child's evaluation. ForEach children are
normally independent — no dependency edges between them. Without propagateWhen, children can be
evaluated in any order (or concurrently — see
[007-optimizations](007-optimizations.md#parallel-evaluation)). But when propagateWhen references
the parent collection, each child's gate depends on sibling state. This is a lateral dependency the
DAG does not model. The forEach loop handles it: the parent iterates children sequentially,
evaluating propagateWhen and updating the parent aggregate after each dispatch.

Each child has one of four states relative to the latest generation:

| `updated()` | `ready()` | State        | Meaning                                       |
| ----------- | --------- | ------------ | --------------------------------------------- |
| true        | true      | **Current**  | On latest version and healthy                 |
| true        | false     | **Updating** | On latest version, not yet converged          |
| false       | true      | **Pending**  | Healthy on old version, good update candidate |
| false       | false     | **Stuck**    | Broken before rollout, bad update candidate   |

**Dispatch loop.** The parent iterates all children in order. For each child:

1. propagateWhen satisfied → dispatch, update parent aggregate.
2. propagateWhen unsatisfied → skip, retain previous state.

The loop always completes. Children whose propagateWhen is unsatisfied are skipped — the loop
continues to the next child.

**Ordering.** Ready before NotReady, before error states. Within a readiness class, random. This
avoids baking in a policy for which children go first — the propagateWhen expression controls what
happens, ordering just determines who is evaluated next.

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

## Why Not

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

**Explicit sort-by for forEach rollout ordering.** User-specified sort expressions for controlling
which children are dispatched first during rollouts. Ready-first with random tiebreaker avoids
baking in a policy. Different use cases want different orderings (oldest-first for patching,
newest-unhealthy-first for bug fixes). Random within readiness class is neutral — the propagateWhen
expression controls the rollout, not the ordering.

**Source-side propagateWhen.** propagateWhen declared on the source node, controlling outflow to all
dependents. Conflates the health signal (which belongs in readyWhen) with the gating decision (which
belongs at the consumer). Each consumer has different input tolerance — a blanket gate on the source
removes that flexibility. readyWhen produces the signal, propagateWhen on the consumer consumes it.

**Concurrent propagateWhen evaluation for forEach children.** The propagateWhen expression
references the parent collection, which includes sibling state. Concurrent evaluation against a
static snapshot produces incorrect budget enforcement — all children see the same state and all
dispatch. Sequential evaluation with aggregate updates after each dispatch is required.
