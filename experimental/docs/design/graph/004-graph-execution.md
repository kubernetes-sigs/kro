# Graph Execution

How the controller winds up resources, unwinds them, and handles dynamic collections. The execution
model spans both Graph (the runtime unit) and Revision (the compilation boundary).

## Plan States

Each node in the DAG lands in exactly one state per reconcile.

| State    | Meaning                          | Propagation |
| -------- | -------------------------------- | ----------- |
| Ready    | Applied, readyWhen satisfied     | Unblocked   |
| NotReady | Applied, readyWhen not satisfied | Unblocked   |
| Pending  | CEL expression cannot resolve    | Blocked     |
| Excluded | includeWhen false                | Blocked     |
| Error    | Fatal error                      | Blocked     |

Blocked means dependents cannot evaluate — the dependency's data is not in scope. The DAG walk
naturally skips unevaluable nodes; no explicit propagation mechanism is needed. If A is Pending, B
depends on A, B's CEL fails because A isn't in scope — B is also Pending. If A is Excluded, B can't
resolve A — B is also blocked. Independent branches are unaffected.

Ready and NotReady are transient during normal operation — a resource moves from NotReady to Ready
when its readyWhen conditions pass. A resource in NotReady has been applied and its data is in scope
— dependents proceed with the current data. readyWhen is a health signal (it feeds the Graph's
aggregated status) not an execution gate.

propagateWhen gates data flow during transitions. When a dependency has propagateWhen and the
condition is unsatisfied, dependents skip re-evaluation and retain their last-applied state via the
template hash. When propagateWhen passes, dependents re-evaluate against the now-stable data. A
resource without propagateWhen propagates immediately.

Pending is transient — it resolves when upstream data becomes available. Excluded is permanent for
the duration of that reconcile but may change on the next reconcile if includeWhen inputs change.
Error means the reconcile encountered a non-retryable failure — an API server error, a malformed
template, or a type mismatch during CEL evaluation. A node in Error is re-evaluated on the next
reconcile; the error clears if the underlying cause is resolved (e.g., the API server recovers, the
Graph spec is fixed).

## Resource Tracking

The `internal.kro.run/applied-resources` annotation on the Graph object tracks every resource the
Graph has applied to. The value is a semicolon-delimited list of resource keys:

```
group/version/kind/namespace/name
```

Every reconcile produces the complete set of intended resource keys as a side effect of the DAG
walk. The annotation is write-eliminated: it is only updated when the sorted key set changes,
preventing a resourceVersion bump and spurious re-reconciliation. This is the cleanup index — on
prune or teardown, the controller iterates this list.

## Execution Triggers

Three events trigger an execution cycle:

- **Instance creation** — a new Graph object appears. The controller runs a full wind: compile, walk
  the DAG, apply all resources.
- **Revision activation** — the Graph spec changed, a new Revision was compiled and activated. The
  controller runs a rollforward: a partial unwind of what changed followed by a wind of the new
  state.
- **Schema drift** — a CRD referenced by the Graph changed in the cluster. The compilation layer
  detects the mismatch against the active Revision's recorded schema versions, produces a new
  Revision, and triggers a rollforward.

In all cases, the reconcile loop is the same algorithm — a topological walk that applies the
intended state and prunes what no longer belongs. The trigger determines what changed; the walk
determines what to do about it.

## Wind

The creation lifecycle. The controller walks the DAG in topological order — dependencies before
dependents — evaluating CEL expressions and applying resources.

### Parallel Topological Walk

Nodes partition into levels by their maximum distance from any root.

```
Level 0:  [A]  [B]        <- no dependencies, process in parallel
Level 1:  [C]  [D]        <- depend only on level 0 nodes
Level 2:  [E]             <- depends on level 1 nodes
```

All nodes at a level can be processed concurrently. Their dependencies are at lower levels and are
already resolved. The algorithm is correct whether nodes within a level run sequentially or in
parallel — parallelism is a throughput optimization, not a correctness property.

For each node, the walk:

1. Skips if blocked — a dependency's data is not in scope.
2. Skips if a dependency has propagateWhen unsatisfied — retains last-applied state.
3. Evaluates `includeWhen` — if false, sets Excluded.
4. Dispatches by resource type.
5. Evaluates `readyWhen` — if not satisfied, sets NotReady. The resource's data is in scope
   regardless; dependents proceed (unless gated by propagateWhen).

### Template Apply

Evaluate the template's CEL expressions against the current scope, then apply. The API server
response (including server-assigned fields like UID, resourceVersion, and status) enters scope under
the resource's `id`. Downstream nodes can reference these fields.

A template hash (FNV-64a of the evaluated output) is stored as an annotation on the resource. On
subsequent reconciles, if the hash matches, the apply is skipped — the desired state hasn't changed.

Watch templates (identity fields only) are read via GET, not applied. They enter scope the same way
but are not tracked for cleanup. Contribute templates (subset of fields) are applied and tracked —
the Graph manages exactly the fields the template specifies.

### forEach Expansion

Evaluate the collection expression, iterate the result, and stamp the template once per item with
the iterator variable in scope.

```yaml
- id: copies
  forEach:
    src: ${sources}
  template:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: ${src.metadata.name}-copy
```

Each iteration evaluates the template in an isolated scope where `src` is bound to the current item.
The results are collected into an array and entered into scope under the resource's `id`. Downstream
nodes see the full collection.

Resource identity is the evaluated resource name — the GVK, namespace, and name that the template
produces for each item. Names are deterministic when the collection expression produces a
deterministic set and the naming expression maps each item to a unique, stable name.

If the collection is nondeterministic (different items on each reconcile) or the naming expression
produces colliding names, the set-diff churns: every reconcile produces different keys, the diff
prunes the old and creates the new, and steady-state never converges. The pruning mechanism still
ensures cleanup correctness — no orphans accumulate — but the Graph never reaches Ready. This is a
liveness problem, not a safety problem.

### Contribute Templates

A contribute template targets an existing resource and specifies a subset of its fields. The
controller splits the apply into up to two operations based on which fields the template contains:

1. Metadata/spec fields via the main resource. Skipped if the template contains only `.status` and
   identifying metadata.
2. Status fields via the status subresource. Skipped if the template contains no `.status` fields.

The target resource reference is added to the `applied-resources` annotation like any other managed
resource.

## Steady-State

Once all resources are applied and healthy, the reconcile loop makes zero API calls per managed
resource. The only call is the initial GET of the Graph object that triggered the reconcile.

- Template hashing: hash matches -> skip apply.
- Metadata informers: resourceVersion matches -> skip GET.
- Annotation: key set unchanged -> skip write.
- Status: unchanged -> skip write.

### forEach Scale Changes

forEach collections are reactive. When the upstream collection changes — items added or removed —
the next reconcile produces a different set of resource keys.

**Scale up**: New items in the collection -> new keys in the current set -> new resources applied ->
annotation updated. Existing resources are untouched (template hash matches).

**Scale down**: Items removed from the collection -> their keys absent from the current set -> the
set diff (`previousKeys - currentKeys`) identifies prune candidates. Each candidate is verified: GET
the resource, check for the template-hash annotation (ownership proof), then DELETE.

**Reordering**: If collection order changes but the items are the same, the evaluated resource names
do not change. No prune/create churn. Identity is name-based, not index-based.

**Name expression changes**: If the template's naming expression changes, all previous names
disappear from the current set and all new names appear. The set-diff correctly prunes old resources
and creates new ones.

**Pending blocks pruning**: If the collection expression cannot be evaluated, the intended set is
unknown. The controller skips pruning entirely. Stale resources persist until the next successful
reconcile. This is a latency property, not a correctness property.

**Crash recovery**: If the controller crashes between applying new resources and updating the
annotation, the next reconcile re-walks the DAG, produces the correct current set, and prunes the
diff against the stale annotation. The window where stale resources exist is bounded by the
reconcile interval.

### includeWhen Toggles

When an includeWhen condition flips from true to false, the resource is absent from the current key
set. Its dependents also can't evaluate (the excluded resource's data is not in scope) and are
absent too. The set-diff mechanism prunes them. When the condition flips back, the resources
reappear in the current set and are recreated. The pruning mechanism is uniform: forEach scale-down,
includeWhen toggles, and revision transitions all use the same annotation set-diff.

### Absence Classification

When a resource is absent from the current key set, the controller classifies the absence before
pruning.

**Definitive absence** — the resource was excluded by includeWhen, or its dependency chain includes
an excluded, errored, or conflicted node. The resource is absent because the graph intentionally
doesn't include it this reconcile. Safe to prune.

**Uncertain absence** — a dependency's data is not yet available (Pending). The resource might
appear in the key set once the data resolves. Not safe to prune.

The classification propagates through the DAG naturally: if node B is unevaluable because node A is
excluded, B's absence is definitive regardless of B's own includeWhen. The DAG walk already knows
each node's state — the prune decision follows from the blocking cause, not from a separate
propagation mechanism.

### propagateWhen and Transition Gating

During dependency transitions (e.g., rolling updates), dependents would otherwise re-evaluate
against intermediate states — `availableReplicas` dropping temporarily, status conditions flipping.
propagateWhen on a resource holds dependent re-evaluation until the gate passes. Dependents retain
their last-applied state (the template hash is unchanged, so no write occurs). When propagateWhen
becomes true, dependents re-evaluate against the now-stable data and apply if the evaluated output
differs.

Template hashing absorbs the common case even without propagateWhen — if intermediate values don't
change the dependent's evaluated output, no write happens regardless. propagateWhen is for cases
where intermediate values do alter the output and the churn is undesirable.

### Contribute Template Updates

When a contribute template's evaluated output changes (upstream dependency status changed, new data
available), the template hash differs from the cached value and the template is re-applied with the
new field values. If the output hasn't changed, the write is skipped.

## Unwind

When a Graph is deleted, the controller runs a full cleanup before removing its finalizer.

### Reverse Topological Deletion

Release and conditionally delete managed resources in reverse topological order — dependents before
their dependencies. Nodes partition into reverse levels:

```
Level 0:  [E]             <- leaf nodes (no dependents), release first
Level 1:  [C]  [D]        <- depend on E, release after E is gone
Level 2:  [A]  [B]        <- roots, release last
```

At each level, all nodes can be processed in parallel. For each managed
resource, the controller either deletes it (Owns) or releases its fields
via a skeleton SSA apply (Contribute).

After processing a level, verify all resources at that level are gone or released — one GET per
resource. If any persist — child Graphs with their own finalizers, resources with external
finalizers — requeue. The next reconcile picks up where it left off.

**Reconstructing the DAG for deletion**: The active Revision's compiled DAG provides the ordering.
If no Revision is available (compilation never succeeded), the `applied-resources` annotation
provides the set of resources to clean up. Without a DAG, deletion falls back to a flat iteration —
correct for resources with no inter-dependencies, which is the likely case when compilation failed.

### Finalizer Removal

Once all managed resources are processed, remove the finalizer. The API server completes the
deletion.

## Revision Transitions

A revision transition is a partial unwind of what changed followed by a wind of the new state. The
controller does not distinguish between "initial creation" and "revision rollforward" — the same
reconcile algorithm handles both. The annotation set-diff produces the correct operations regardless
of how many resources changed.

### Detection and Activation

The Graph spec changes -> new generation. The compilation layer produces a new Revision with
compiled CEL programs, a dependency graph, and recorded schema versions of all referenced CRDs. The
new Revision is marked Active; the old Revision is marked Active=False. The controller begins
reconciling against the new Revision.

### Rollforward

The reconcile against the new Revision produces a new set of resource keys. The set-diff against the
previous annotation:

| Case      | Condition                   | Action                   |
| --------- | --------------------------- | ------------------------ |
| Unchanged | In both, template-hash same | No-op                    |
| Updated   | In both, template-hash diff | SSA apply                |
| Created   | In new set only             | SSA apply (new resource) |
| Removed   | In old set only             | Prune (delete)           |

Template hashing prevents unnecessary writes. A resource in both the old and new Revision with
identical evaluated output is a no-op regardless of the Revision change.

### Schema Drift

A referenced CRD changing in the cluster invalidates the active Revision's compiled programs. The
Revision records schema versions of all referenced CRDs at compilation time. The compilation layer
watches these CRDs and compares current versions against the Revision's recorded dependencies. A
mismatch triggers recompilation — a new Revision — and instances roll forward via the standard
transition mechanism.

This is structural invalidation (Revision is stale -> produce new Revision), not cache invalidation.
The staleness is detectable by inspection, not dependent on TTLs or cache eviction heuristics.

## Why Not

**Index-based forEach identity.** Mapping collection items to resources by their position in the
array means reordering the input list deletes and recreates every resource. Name-based identity is
stable under reordering because the evaluated resource name depends on the item's content, not its
position.

**Separate discovery for forEach orphans.** An earlier model considered using label selectors to
discover forEach-created resources for pruning. The annotation-based set-diff is simpler — it
compares the exact set of resources the controller intended, not a selector that might match
unrelated objects.

**Flat deletion ordering.** Deleting all managed resources in a single unordered pass ignores
dependency relationships. If resource B depends on resource A, deleting A first may cause B's
controller to error or attempt to recreate A. Reverse topological ordering ensures dependents are
gone before their dependencies.

**Continuous drift restoration.** Re-applying every resource on every reconcile to restore non-SSA
edits is an N-write steady-state tax for an event that rarely happens. Content-addressed apply skips
the write when the template hash matches. Drift from non-SSA edits persists until the template
output changes, at which point the apply restores the controller's desired state. This is the
pod-template-hash tradeoff.
