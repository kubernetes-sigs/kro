# Graph Execution

How the controller walks the DAG. Wind creates resources in dependency order. Unwind removes them
in reverse. Revision transitions, forEach changes, and Graph deletion all reduce to the same
walk — the DAG is the single execution primitive.

## Data Flow

A Graph is a reactive data flow. Watches bring external state in. Templates push state out. The DAG
is the flow between them — CEL expressions on each node transform upstream data into downstream
desired state.

```
Cluster ── watch ──▶ [scope] ── CEL ──▶ [template] ── SSA ──▶ Cluster
```

A change to any watched resource triggers a reconcile. The reconcile walks the DAG, re-evaluates
expressions against current scope, and applies resources whose evaluated output changed. Template
hashing gates every write — if the evaluated output matches the hash on the resource, the apply is
skipped. In steady state, nothing changed and no writes occur.

## Plan States

Each node in the DAG lands in exactly one state per reconcile.

| State    | Meaning                        | Dependents | Resolution                          |
| -------- | ------------------------------ | ---------- | ----------------------------------- |
| Ready    | Applied, readyWhen satisfied   | Proceed    | —                                   |
| NotReady | Applied, readyWhen unsatisfied | Proceed    | Converges                           |
| Pending  | Data not yet available         | Blocked    | Upstream resolves                   |
| Excluded | includeWhen false              | Blocked    | includeWhen inputs change           |
| Conflict | Field ownership contested      | Blocked    | Template change or external release |
| Error    | Transient failure              | Blocked    | Retries next reconcile              |

Ready and NotReady are both "applied and in scope." readyWhen is a health signal that feeds the
Graph's aggregated status — it does not gate dependents. Dependents proceed as soon as the
resource's data is in scope.

Blocked states propagate naturally through the DAG. If A is Pending, B depends on A, B's CEL
expressions fail because A isn't in scope — B is also Pending. No explicit propagation mechanism is
needed; CEL evaluation failure IS the propagation. Independent branches are unaffected.

Conflict is distinct from Error. Error retries automatically on the next reconcile — transient API
failures, webhook rejections, network errors. Conflict persists until resolved: change the template,
add `kro.run/apply: Force`, or wait for the external actor to release the field. Conflict clears
when the template content changes (new hash) or the contested field is released externally.

## Watches

Watches are the DAG's inputs — the sensors that bring external state into scope and trigger
reconciliation when that state changes.

### Event Routing

Two reverse indexes map cluster events to Graphs:

- **Scalar index** — GVR + namespace + name → Graph(s). For Watch templates. An update to the
  watched resource triggers reconciliation of the Graphs that watch it.
- **Collection index** — GVR + selector → Graph(s). For Collection Watch templates. Collection
  membership changes — creates, deletes, and label changes that affect selector matching — trigger
  reconciliation. Updates to existing members that don't affect membership do not trigger the
  parent.

```
Cluster event: ConfigMap "shared-config" updated
       │
       ▼
  Scalar index: v1/ConfigMap/default/shared-config → [my-app]
       │
       ▼
  Graph "my-app" reconciles        ← watches this ConfigMap
  Graph "other-app" untouched      ← doesn't watch it
```

Event routing is O(1) per event — only Graphs that watch the changed resource reconcile.

### Collection Watches and Nested Graphs

Collection watches fire on membership changes, not member updates. This is the
structural basis for per-instance reactivity:

```
Parent Graph                            Child Graphs
┌──────────────────────────┐
│ namespaces (Coll. Watch) │      ┌─────────┐ ┌─────────┐ ┌─────────┐
│       │                  │      │ Child: a │ │ Child: b │ │ Child: c │
│  perNS (forEach)         │      │         │ │         │ │         │
│   ├── Graph: a ──────────┼─────▶│  nsRef  │ │  nsRef  │ │  nsRef  │
│   ├── Graph: b ──────────┼─────▶│  policy │ │  policy │ │  policy │
│   └── Graph: c ──────────┼─────▶│         │ │         │ │         │
└──────────────────────────┘      └─────────┘ └─────────┘ └─────────┘

Event: Namespace "b" spec updated
  → Collection watch: membership unchanged → parent NOT triggered
  → Scalar watch on child "b": fires → only child "b" reconciles
  → O(1) per instance

Event: Namespace "d" created
  → Collection watch: membership changed → parent triggered
  → forEach stamps new child Graph "d"
  → Existing children: template hash match → skip apply
```

## The Walk

Every reconcile does one thing: converge toward the active revision's DAG. The algorithm has two
phases that run every reconcile — wind applies what should exist, prune removes what shouldn't.

### Levels

Resources partition into levels by their maximum dependency depth. All nodes at a level can be
processed in parallel — their dependencies are at lower levels and already resolved.

```
Level 0:  [ config ]  [ webapp ]         ← no dependencies
Level 1:  [ deploy ]                     ← depends on config
Level 2:  [ service ]  [ webappStatus ]  ← depends on deploy, webapp
```

Declaration order is irrelevant. The dependency graph — inferred from CEL expression references —
determines execution order.

### Wind

Forward topological walk. For each level, process all nodes concurrently:

1. **Blocked check** — a dependency is Pending, Excluded, Conflict, or Error. Skip.
2. **propagateWhen check** — a dependency has `propagateWhen` unsatisfied. Retain last-applied
   state (template hash unchanged, no write). Skip.
3. **includeWhen** — evaluate. If false, mark Excluded.
4. **Dispatch** — by template shape. Watch: GET. Collection Watch: List. Owns/Contribute: evaluate
   template, hash the output, compare against the resource's hash annotation. Hash match → skip
   apply. Hash differs → SSA apply.
5. **readyWhen** — evaluate. Sets Ready or NotReady. Data is in scope regardless.

After processing, the node's data enters scope — the full Kubernetes object (including
server-assigned fields like UID, resourceVersion, status). Downstream nodes at the next level can
reference it.

### Prune

After wind completes, the controller diffs the current key set (what wind produced) against the
tracked set (what was previously applied). Resources in the tracked set but absent from the current
set are prune candidates.

Not all absent resources should be pruned:

- **Definitive absence** — the resource was excluded by includeWhen (the condition was evaluated and
  returned false), a dependency was Excluded (contagious — the dependent can't exist without it), or
  the resource's template was removed from the active revision. The DAG intentionally doesn't
  include it. Safe to prune.
- **Uncertain absence** — a dependency is Pending, Conflict, or Error. The resource might appear in
  the key set once the blocker resolves. Not safe to prune — pruning a healthy resource only to
  recreate it when the blocker clears is unnecessary churn.

Prune candidates are removed in reverse dependency order. The revision that created them provides
the dependency information for safe ordering.

```
Wind produced:  { config, deploy, service }
Tracked set:    { config, deploy, service, ingress, monitoring }
Prune set:      { ingress, monitoring }

  ingress depends on service (in old revision's DAG)
  monitoring depends on deploy (in old revision's DAG)

Prune order:  [ ingress, monitoring ]  then  their dependencies (if also pruned)
```

forEach scale-down, includeWhen toggles, and revision transitions all produce the same kind of
diff. The pruning mechanism is uniform.

### Invariants

**Wind-first.** Wind completes before prune runs. Prune's input is the current key set from a
complete wind. A partial wind (blocked dependencies, transient errors) produces an incomplete key
set — pruning against it would delete resources that should still exist. If wind is incomplete, prune
is skipped. The tracked set is unchanged. Next reconcile retries.

**Prune requires ordering.** Pruning in reverse dependency order is not optional. If resource B
depends on A and both are being pruned, deleting A first can leave B's finalizer stuck against a
missing dependency. The superseded revision's DAG provides the ordering. If ordering information is
unavailable, prune is blocked — the controller surfaces an error and retries, never degrades to
unordered deletion.

**Tracked set stores expanded keys.** The tracked set contains actual resource keys
(`group/version/kind/namespace/name`), not template identifiers. A forEach that stamps 100 resources
produces 100 keys in the tracked set, not one.

## forEach

forEach is a single node in the DAG. It evaluates the collection expression, iterates the result,
and stamps the template once per item.

```
┌──────────────┐
│  namespaces  │
│(Coll. Watch) │
└──────┬───────┘
       │
┌──────▼───────┐
│  perNS       │    Single DAG node.
│  (forEach)   │    Evaluates all items.
│              │    Produces array in scope.
│  ├ ns-a      │
│  ├ ns-b      │
│  └ ns-c      │
└──────┬───────┘
       │
┌──────▼───────┐
│  downstream  │    Depends on forEach as a unit.
│              │    Sees the complete array.
└──────────────┘
```

Each stamped resource has its own template hash. When the collection changes, the forEach
re-evaluates all items, but unchanged items produce the same hash and skip the apply. The
collection is re-iterated, but unchanged items produce zero writes.

Resource identity is the evaluated name — GVK + namespace + name that the template produces for
each item. Identity is name-based, not index-based. Reordering the collection without changing the
items produces the same names — no churn.

### Scale Changes

**Scale up** — new items in the collection → new resource keys → create.

**Scale down** — items removed → keys absent from current set → prune.

**Reordering** — same items, different order → same evaluated names → no churn.

**Pending collection** — the collection expression cannot evaluate (upstream Pending). The intended
key set is unknown. Prune is blocked — stale resources persist until the next successful evaluation.

### Nested Graphs for Per-Item Reactivity

forEach re-evaluates all items when the collection changes. For per-item reactivity — where a
change to one item triggers reconciliation of only that item — use nested Graphs. Each child Graph
is independently reconciled by its own watches. The parent's forEach only fires on collection
membership changes (create/delete), not on updates to existing members.

This is the difference between O(N) re-evaluation (flat forEach, all items re-evaluated) and O(1)
per-instance reconciliation (nested Graphs, each child independently triggered).

## Revisions

A revision transition is not a special case. It is the same walk with a different DAG.

When the Graph's spec changes, a new revision is materialized (see 002). The controller begins
converging toward the new revision's DAG — same wind, same prune, same algorithm. New resources
appear in the forward walk (created). Changed resources have different template hashes (updated).
Removed resources appear in the prune diff (deleted or released).

```
Old revision:   config ── deploy ── service
                                 ── ingress

New revision:   config ── deploy ── service
                                 ── monitoring

Wind (new DAG):
  Level 0: config       (hash match → skip)
  Level 1: deploy       (hash match → skip)
  Level 2: service      (hash match → skip)
            monitoring   (new → create)

Prune (tracked - current):
  ingress               (removed → delete)
```

### Multiple Revisions in Flight

Fast spec mutations produce multiple revisions before the controller finishes converging. The
controller always converges toward the latest revision — intermediate revisions are skipped. Their
winds are abandoned. The latest revision's DAG becomes the target.

Superseded revisions are retained until their resources are pruned. Each revision's DAG provides the
dependency ordering needed to safely remove its resources. A superseded revision is deleted only
after all its unique resources have been migrated to the new revision or removed from the cluster.

```
Revision 3:  { A, B, C }     ← fully wound
Revision 4:  { A, B, D }     ← fully wound, C pruned, D created
Revision 5:  { A, E }        ← activated before prune completes

Controller converges to revision 5:
  Wind:  A (skip), E (create)
  Prune: B (from rev 3/4), D (from rev 4)

Prune ordering uses superseded revisions' DAGs:
  Rev 4: D depends on B → prune D before B
  Order: D → B
```

### Tracking

The controller persists the set of resource keys it has applied to — the tracked set. This is the
crash recovery index. On restart, the tracked set tells the controller what exists in the cluster.

The tracked set is only written when it changes — write elimination prevents a resourceVersion bump
and spurious re-reconciliation.

Template shape (Owns vs Contribute) for prune decisions comes from the revision that created the
resource. Conflict state is re-derived at runtime from the label check and SSA response.

## Teardown

When a Graph is deleted, the controller runs a full unwind before removing its finalizer.

The controller confirms the active revision exists before starting teardown — it provides the
dependency ordering. If the revision was deleted (ownerReference cascade race), the controller
regenerates it from the current spec. Teardown is blocked until ordering is available.

```
Unwind order (reverse topological):

Level 2:  [ service ✗ ]  [ webappStatus ✗ ]  ← delete/release first
Level 1:  [ deploy ✗ ]                       ← after dependents are gone
Level 0:  [ config ]  [ webapp ]              ← Watch: no action
```

At each level, all nodes can be processed in parallel. Owns templates delete the resource.
Contribute templates release fields via skeleton apply (see 003). Watch and Collection Watch take
no action. Resources in Conflict state are skipped — the Graph no longer holds the fields.

After processing a level, verify all resources are gone or released — one GET per resource. If any
persist (child Graphs with their own finalizers, resources with external finalizers), requeue. The
next reconcile picks up where it left off.

Once all managed resources are processed, remove the Graph's finalizer. The API server completes the
deletion.

## Why Not

**Index-based forEach identity.** Mapping items to resources by array position means reordering
deletes and recreates everything. Name-based identity is stable under reordering.

**Unordered deletion.** Deleting all resources in a single unordered pass ignores dependencies. If B
depends on A, deleting A first can leave B's finalizer stuck. Reverse topological ordering ensures
dependents are gone first. Correctness is not negotiable — if ordering is unavailable, block.

**Separate rollforward algorithm.** The same wind+prune handles creation, steady state, and revision
transitions. A separate rollforward code path duplicates the reconciliation logic and diverges over
time.

**Partial DAG walks.** Only walking the subgraph affected by the triggering watch event sounds
efficient, but controller-runtime coalesces events — multiple resources can change between
reconciles. Computing the affected subgraph union approaches a full walk. With template hashing
eliminating writes for unchanged resources, the full walk is already cheap — it's CEL evaluation and
hash comparison, not API calls.

**Continuous drift restoration.** Re-applying every resource on every reconcile to detect and fix
non-SSA edits is an N-write steady-state tax for an event that rarely happens. Template hashing
skips the apply when the output matches. Drift from non-SSA edits persists until the template output
changes. This is the pod-template-hash tradeoff — the controller converges on spec change, not
continuously.
