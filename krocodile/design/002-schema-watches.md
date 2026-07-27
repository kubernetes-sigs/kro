# Schema Watches

When a CRD's schema changes, every Graph that references its Kind
recompiles against the new schema. The controller learns about CRD
changes through a standard CRD informer, dedups events that don't
actually change the schema content, and pushes targeted invalidations
into the compile cache and schema resolver before enqueueing the
affected Graphs.

This document describes the data model, the event-handling rules, the
invalidation chain, and the boundaries between the schema watcher,
the compiler, the registry, and the reconciler.

## Why

Without this, three things go wrong:

1. **Stale type-checking.** A field added to a CRD never reaches the
   compiler's resolver cache. Graphs that reference the Kind still
   type-check against the old shape; a CEL expression reading a newly
   added field surfaces as a runtime error instead of a compile-time
   one.

2. **Stale templates.** The compiler emits `*Program` based on the
   schema state at first compile. If a field is removed from the CRD,
   the next reconcile keeps applying it via SSA against a schema that
   no longer admits it — apply rejects or strips silently depending
   on `preserveUnknownFields` semantics.

3. **No recovery for missing-CRD installs.** A Graph that references a
   Kind whose CRD doesn't exist yet fails to compile (Accepted=False,
   "schema not found"). When the CRD lands later, nothing re-triggers
   the Graph until a user touches its spec or the controller restarts.

The schema watcher solves all three with one mechanism.

## Data model

The watcher holds three pieces of state, all in-memory:

```go
type SchemaWatcher struct {
    byGK         map[schema.GroupKind]map[client.ObjectKey]struct{}  // reverse index
    dynamic      map[client.ObjectKey]struct{}                        // Graphs with CEL apiVersion/kind
    subs         map[client.ObjectKey]*graphSub                       // per-Graph two-cycle state
    schemaHashes map[schema.GroupKind]string                          // dedup cache
    ...
}
```

`byGK` is the lookup the CRD event path consults: given a changed GK,
find every Graph that declared it as a static dependency. `dynamic`
captures Graphs whose `apiVersion` or `kind` is a CEL expression —
their GVK isn't knowable at compile time, so they subscribe to all
CRD changes. `subs` is per-Graph state for the two-cycle Track /
Done(commit) pattern (mirrors the watchrouter). `schemaHashes`
deduplicates non-schema CRD updates.

Nothing here is persisted to status. All four maps are reconstructible
from spec on every reconcile. On controller restart the manager's
`For(&Graph{})` source triggers an initial reconcile for every
existing Graph; each reconcile re-Tracks, and the indexes re-emerge
in seconds.

## Compiler outputs

The compiler walks every node in the Graph and emits two fields on
the resulting `*Program`:

```go
type Program struct {
    ...
    RequiredGroupKinds []schema.GroupKind  // deduplicated set of literal GKs
    HasDynamicGVK      bool                // any node has CEL at apiVersion/kind
}
```

`RequiredGroupKinds` is populated from each non-Def node's
`Object.apiVersion + kind`. `HasDynamicGVK` flips true when any node's
`Variables` list contains a `Path == "apiVersion"` or `Path == "kind"`
entry — the parser would have extracted a CEL fragment at those paths
during compile, regardless of how the runtime would later resolve it.

Def nodes contribute nothing to either field. They don't reference
cluster schemas.

## Top-level shape

```
                ┌────────────────────────────────┐
                │  apiserver CRD informer        │
                │  (shared with mgr.GetCache())  │
                └──────────────┬─────────────────┘
                               │ Add / Update / Delete events
                               ▼
                ┌────────────────────────────────┐
                │  SchemaWatcher                 │
                │                                │
                │   onAdd:      seed hash,       │
                │               enqueue ALL      │
                │   onUpdate:   hash-dedup,      │
                │               enqueue affected │
                │   onDelete:   drop hash,       │
                │               enqueue affected │
                └──────────────┬─────────────────┘
                               │
       ┌───────────────────────┼──────────────────────┐
       │                       │                      │
       ▼                       ▼                      ▼
  ┌─────────────┐       ┌──────────────┐      ┌──────────────────┐
  │  Schema     │       │   Registry   │      │  source.Channel  │
  │  resolver   │       │   (compile   │      │   (manager       │
  │  cache      │       │    cache)    │      │    work queue)   │
  │             │       │              │      │                  │
  │  Invalidate │       │  Delete(key) │      │  enqueue(key)    │
  │   (gk)      │       │              │      │                  │
  └─────────────┘       └──────────────┘      └──────────────────┘
       │                       │                      │
       └───────────┬───────────┴──────────────────────┘
                   │
                   ▼
          next reconcile re-derives:
            - compile against fresh schema
            - publish fresh RequiredGroupKinds
            - re-Track schema dependencies
```

The schema watcher is process-scoped and shared across all Graphs.
The watchrouter (resource drift) and the schema watcher are siblings:
the watchrouter routes child-resource events; the schema watcher
routes CRD events. They feed the same controller-runtime work queue
through separate `source.Channel` connections.

## Subscribe API

The reconciler interacts with the watcher through a per-Graph
subscription handle, obtained at the top of every reconcile:

```go
type Subscription interface {
    Track(gk schema.GroupKind)
    TrackDynamic()
    Done(commit bool)
}

sub := schemaWatcher.ForGraph(graphKey)
for _, gk := range program.RequiredGroupKinds {
    sub.Track(gk)
}
if program.HasDynamicGVK {
    sub.TrackDynamic()
}
// ... apply ...
sub.Done(commit)
```

`Done(commit=true)` swaps the in-flight set into the committed set and
removes any GKs that were committed previously but not re-Tracked this
cycle. `Done(commit=false)` discards the in-flight set; the
previously-committed subscription stays authoritative. This is the
same two-cycle pattern the watchrouter uses for resource watches, for
the same reason: spec edits should add and remove dependencies
atomically without a window of overcoverage or undercoverage.

## CRD event handling

The watcher's three event handlers each follow a different rule:

| Event   | Hash action          | Enqueue scope                                              | Reason                                                  |
| ------- | -------------------- | ---------------------------------------------------------- | ------------------------------------------------------- |
| Add     | seed hash for GK     | every Graph in `byGK[gk]` ∪ `dynamic` ∪ **all subscribers**| New CRD might unblock Graphs that were stuck on it      |
| Update  | dedup on hash diff   | every Graph in `byGK[gk]` ∪ `dynamic`                       | Targeted by indexed dependency                          |
| Delete  | drop hash entry      | every Graph in `byGK[gk]` ∪ `dynamic`                       | They'll fail to compile against missing Kind; intended  |

The Add path's "every subscriber" branch is the bootstrap recovery
case: when a CRD is installed *after* a Graph that references it has
already been reconciled and failed, that Graph isn't in `byGK[gk]`
(its compile failed before reaching Track). Enqueueing every
subscriber lets stuck Graphs retry. CRD adds are rare, so the cost
is negligible.

## Hash dedup

A CRD's `resourceVersion` advances on every metadata write:
annotations, labels, status echoes from the apiserver's own
controllers, finalizer additions. None of those change the schema
content the compiler cares about. Without dedup, every benign CRD
write would enqueue every dependent Graph — a churn vector that
scales poorly.

The hash is SHA-256 over a projected view of the CRD's spec,
including:

- `spec.group`, `spec.names`, `spec.scope`
- `spec.conversion`
- per-version: `name`, `served`, `storage`, `schema.openAPIV3Schema`,
  `subresources`, `additionalPrinterColumns`

Everything else — annotations, labels, status, observedGeneration,
resourceVersion, finalizers — is excluded. A change that doesn't move
any of the included fields produces the same hash, and the update
short-circuits before reaching the enqueue path.

This is what makes the system safe to keep an LRU schema cache with
no TTL: when the schema actually changes, the hash differs and the
invalidation fires precisely. When it doesn't, nothing happens.

## The invalidation chain

When a Graph needs to be re-reconciled because a CRD it depends on
changed, three things must happen, in order:

```
1. SchemaInvalidator.InvalidateSchema(gk)
       │
       ▼  (CachedSchemaResolver evicts every entry whose GroupKind == gk)
       │
2. GraphInvalidator.Delete(key)
       │
       ▼  (Registry drops the cached *Program for this Graph)
       │
3. enqueue(key) via source.Channel
       │
       ▼  (controller-runtime adds the reconcile.Request to its queue)
       │
4. reconcileGraph(key)  ←  triggered by step 3
       │
       ▼  Registry.Compile → miss → Compiler.Compile
       │      ▼
       │      schema resolver → miss → fetch fresh from apiserver
       │      ▼
       │      *Program with possibly-new RequiredGroupKinds
       ▼
   Reconciler re-Tracks dependencies via SchemaWatcher subscription
```

The order matters. If we enqueued before invalidating, the new
reconcile would race the eviction and could still see stale data.
Step 1 before step 2 is for consistency: the resolver cache miss
on a Kind whose schema we already noticed has changed is what
yields the fresh fetch.

## Push invalidation, not periodic refresh

This replaces an earlier TTL-cached resolver. With the schema watcher
running, periodic refetches are redundant — every schema change is
already pushed in by the informer event handler. The TTL became
cargo-culture noise: cache entries refetched every five minutes
"just in case," paying for staleness that the watcher already
prevents.

The current `CachedSchemaResolver` holds entries indefinitely until
either:

- LRU pressure evicts them (oldest first, at the configured cap)
- `InvalidateGroupKind(gk)` evicts every entry for that GroupKind

The trade-off is correctness-vs-redundancy: if the informer somehow
misses an event (informer disconnects, controller-runtime cache
glitches), the cache holds the old schema until either a manual
restart or LRU pressure. In practice informer disconnects are
recovered via list-then-watch resyncs, and the cache reconverges
naturally on the resync's events. Defense-in-depth could re-add a
TTL, but the cost isn't justified by observed failure modes.

## Lifecycle

```
process start
  │
  ▼  manager.Add(schemaWatcher)
  │
  ▼  manager.Start(ctx)
  │     │
  │     ├── manager cache starts CRD informer (shared)
  │     │
  │     └── SchemaWatcher.Start attaches event handler
  │
  ▼  controller reconcile loop begins
  │
  ▼  per Graph reconcile:
  │     │
  │     ├── compile → Program.RequiredGroupKinds + HasDynamicGVK
  │     │
  │     ├── sub := schemaWatcher.ForGraph(key)
  │     │     for gk in RequiredGroupKinds: sub.Track(gk)
  │     │     if HasDynamicGVK: sub.TrackDynamic()
  │     │
  │     ├── apply
  │     │
  │     └── sub.Done(commit=true on success/soft, false on hard)
  │
  ▼  CRD event arrives:
  │     │
  │     ├── hash check (dedup)
  │     │
  │     ├── if changed:
  │     │     for key in byGK[gk] ∪ dynamic:
  │     │       invalidate compiler.schemaCache for gk
  │     │       invalidate registry for key
  │     │       enqueue(key)
  │
  ▼  on Graph delete:
        sub.RemoveGraph(key) drops every GK entry + dynamic-set entry
```

## What this design deliberately does not do

- **No status persistence of GroupKind dependencies.** The reverse
  index is reconstructible from spec via reconcile replay. Persisting
  it would burn etcd writes on every reconcile for no recovery benefit.
- **No ApplySet-style discovery.** We do not LIST resources by label
  to figure out what a Graph owns. Tracking lives in
  `status.managedResources` (per
  [001-resource-tracking](001-resource-tracking.md)).
- **No periodic refresh.** Cache invalidation is push-only. If the
  informer is unhealthy the resolver may be stale; recovery is
  list-resync.
- **No partial-GK watching.** A Graph either depends on a literal
  `(Group, Kind)` or it's dynamic (depends on everything). There's no
  middle ground like "any Kind in the `apps` group."
- **No coarser dependency tracking.** We don't track at the
  `(Group, Kind, Version)` level. A schema change in `v1beta1` of a
  GroupKind re-enqueues Graphs using `v1` of the same GroupKind. This
  is intentional — most CRD edits affect every served version at
  once.

## Boundaries

- **Compiler** emits `RequiredGroupKinds` and `HasDynamicGVK` on the
  Program. It does not subscribe; it does not know about the watcher.
- **Reconciler** owns the subscription lifecycle: open per cycle,
  Track per dependency, commit/abort with the same gate as the
  watchrouter subscription.
- **Registry** is invalidated by the watcher via the `GraphInvalidator`
  interface. It doesn't subscribe to events directly.
- **CachedSchemaResolver** is invalidated by the watcher via the
  `SchemaInvalidator` interface (which the Compiler implements as a
  thin pass-through to its embedded resolver). It doesn't subscribe
  either.
- **SchemaWatcher** owns the informer, the indexes, the hash dedup,
  and the invalidation orchestration. It calls into the resolver and
  the registry, but neither calls into it (apart from the reconciler
  via Subscribe).

This split keeps each component testable in isolation: the compiler
has tests for `emitSchemaDependencies` without touching a watcher;
the watcher has tests for index + dedup without touching a real
compiler; the resolver has tests for `InvalidateGroupKind` without
either. Integration tests then wire them together end-to-end against
envtest.
