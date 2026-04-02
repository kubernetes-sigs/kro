# KREP-013: Graph Revisions

## Summary

This proposal introduces `GraphRevision`, a cluster-scoped immutable snapshot of
a compiled RGD spec. On each accepted RGD spec change, the RGD controller issues
a new GraphRevision. A dedicated GraphRevision controller compiles the snapshot
and stores the result in a shared in-memory registry. Instance controllers
resolve their compiled graph from the registry instead of receiving it directly.
This KREP covers snapshot issuance, compilation, registry, and instance
resolution. Revision pinning, leveled topologies, and propagation control are
follow-up work that builds on this foundation.

## Motivation

The current model has no concept of graph history. When an RGD spec changes, the
old compiled graph is gone. This creates three categories of problems:

**No rollback or diffing.** Operators cannot see what changed between RGD edits.
When an update breaks instances, there is no way to inspect the previous graph,
diff it against the current one, or roll back to a known-good state. Debugging
requires reading git history (if it exists) rather than cluster state.

**No controlled rollout.** All instances immediately pick up the new graph on
their next reconcile. There is no way to roll out changes in levels, test a
new revision against a canary set, or hold some instances on the old graph while
validating the new one. For platform teams managing hundreds of instances, an
RGD change is all-or-nothing.

**No foundation for propagation control.** Features like leveled applies (apply
changes to a subset of instances, observe, then proceed - see
[KREP-005](https://github.com/kubernetes-sigs/kro/pull/859) and
[KREP-006](https://github.com/kubernetes-sigs/kro/pull/861)) require a stable
revision identity that instances can be pinned to. Without immutable snapshots,
there is nothing to pin to. The instance controller receives its graph through a
closure - there is no addressable revision object.

### Concrete example

A platform team runs an RGD that manages a `WebApp` custom resource. 200
instances exist across the cluster. The team updates the RGD to add a sidecar
container to the deployment template.

Without GraphRevisions: all 200 instances pick up the new graph on their next
reconcile. If the sidecar has a bug, all 200 instances are affected. The team
cannot see what the previous graph looked like, cannot roll back without
reverting the RGD spec (and hoping they remember the exact previous state), and
cannot test the change on a subset first.

With GraphRevisions: the RGD controller creates revision 4. The team can diff
revision 3 vs 4 to see exactly what changed. In future work, they can pin 5
canary instances to revision 4, verify the sidecar works, then either pause
behind a manual review gate or promote the remaining 195 in levels. If
something goes wrong, revision 3 still exists as an immutable snapshot they can
roll back to.

## Proposal

### Overview

RGDs are cluster-scoped (`+kubebuilder:resource:scope=Cluster`), so
GraphRevisions are cluster-scoped as well - they follow their parent's scope.

A new cluster-scoped CRD, `GraphRevision`, stores an immutable snapshot of an
RGD spec. A new controller reconciles GraphRevision objects by compiling them
and storing the compiled graph in a shared in-memory registry. The RGD
controller becomes the issuer of revisions after pre-flight validation, but it
remains the owner of the generated CRD and the dynamic controller. The instance
controller becomes a consumer of the registry.

```
 ┌────────────────┐
 │ RGD Controller │
 └───────┬────────┘
         │ creates
         ▼
 ┌───────────────────┐       ┌──────────────────┐
 │  GraphRevision    │──────▶│  GR Controller   │
 │  (API object)     │       │  (compiles graph) │
 └───────────────────┘       └────────┬─────────┘
                                      │ stores
                                      ▼
                             ┌────────────────────┐
                             │  In-Memory Registry │
                             │  (compiled graphs)  │
                             └────────┬────────────┘
                                      │ reads
                                      ▼
                             ┌────────────────────┐
                             │ Instance Controller │
                             └─────────────────────┘
```

### Core Concepts

**Immutable snapshots.** A GraphRevision's spec is immutable (enforced by a CEL
validation rule: `self == oldSelf`). Once created, it never changes. A new RGD
spec change produces a new GraphRevision, never an update to an existing one.

**Pre-flight issuance gate.** Not every attempted RGD edit becomes a
GraphRevision. The RGD controller first compiles the current spec. If that
fails, the edit is surfaced as invalid on the RGD and no new GraphRevision is
created. Revision history records accepted graph snapshots, not arbitrary failed
edits.

**Monotonic revision numbers.** Each GraphRevision has a `revision` field,
monotonically increasing per RGD lineage. The lineage key is the source RGD
name, so a recreated RGD with the same name continues from the existing lineage.
The RGD persists its high-water mark in `status.lastIssuedRevision` so revision
numbers never go backwards within a lineage, even across GC cycles.

On delete+recreate, the new RGD's `status.lastIssuedRevision` starts at 0, but
the RGD controller lists existing GraphRevisions by name (field index on
`spec.snapshot.name`) and computes the next revision as
`max(status.lastIssuedRevision, highestExistingRevision) + 1`. This means:

- If GRs from the previous incarnation survive (GC still in progress or
  retention keeps them), the lineage continues from the highest existing
  revision number.
- If all previous GRs have been garbage collected, the revision resets to 1 -
  which is safe because there are no surviving objects to collide with.

**Internal hashing for change detection.** A canonical FNV-64a hash of the
normalized RGD spec determines whether a new revision is needed (see Hashing
section). The hash is computed in-process and stored only in the in-memory
registry - never persisted on the GraphRevision API object. This makes the hash
algorithm an internal implementation detail that can change between binary
versions without migration. A label (`kro.run/graph-revision-hash`) is written
to the GraphRevision for kubectl discoverability but is never read back.
Semantically identical edits - whitespace changes, YAML field reordering,
comment differences that normalize to the same canonical JSON - do not produce
new revisions.

**Three-state registry.** The in-memory registry tracks each revision as Pending
(created, not yet compiled), Active (compiled, graph available), or Failed
(compilation error). These states are internal scheduling signals between
controllers. The external GraphRevision API surfaces them through conditions
instead of a separate state enum.

### API Changes

#### New CRD: GraphRevision

The `internal.kro.run` API group signals that this API is subject to breaking
changes between releases. Operators can inspect GraphRevision objects via
`kubectl` for debugging and observability, but external automation should use
RGD status fields (conditions, `lastIssuedRevision`) rather than scraping
GraphRevision objects directly. The API group may move to `kro.run` if/when the
schema stabilizes.

```yaml
apiVersion: internal.kro.run/v1alpha1
kind: GraphRevision
metadata:
  name: my-webapp-r00003
  labels:
    kro.run/resource-graph-definition-name: my-webapp
    kro.run/graph-revision-hash: "a1b2c3d4e5f6" # informational, not authoritative
  ownerReferences:
    - apiVersion: kro.run/v1alpha1
      kind: ResourceGraphDefinition
      name: my-webapp
spec:
  revision: 3
  snapshot:
    name: my-webapp
    generation: 2
    spec: # immutable copy of rgd.spec
      schema:
        kind: WebApp
        apiVersion: v1alpha1
      resources:
        - id: deployment
          template: ...
status:
  topologicalOrder: ["configmap", "deployment", "service"]
  conditions:
    - type: GraphVerified
      status: "True"
      reason: Verified
      message: "graph revision compiled and verified"
    - type: Ready
      status: "True"
      reason: AllReady
  resources:
    - id: deployment
      dependencies:
        - id: configmap
```

The spec is wrapped in a `snapshot` object that captures the source RGD identity
and the immutable spec copy. The `generation` field records the RGD's
`metadata.generation` at issuance time, linking the snapshot to a specific spec
mutation. It is set by the RGD controller at issuance from
`rgd.ObjectMeta.Generation` and is not read back by any controller logic — it
exists for forensic and debugging purposes (`kubectl get graphrevision -o yaml`
shows exactly which RGD state produced a given snapshot). Lineage decisions use
the name only.

Spec fields:

| Field                 | Type                        | Description                                                 |
| --------------------- | --------------------------- | ----------------------------------------------------------- |
| `revision`            | int64                       | Monotonic revision number per RGD lineage.                  |
| `snapshot.name`       | string                      | Source RGD name. Authoritative identity for lineage.        |
| `snapshot.generation` | int64                       | Source RGD generation at issuance time. Informational only. |
| `snapshot.spec`       | ResourceGraphDefinitionSpec | Immutable copy of the source RGD spec.                      |

Status conditions follow the established kro pattern (KREP status-conditions):
`Ready` as top-level, `GraphVerified` as sub-condition. The mapping is:
`Pending => Ready=Unknown`, `Active => Ready=True`, `Failed => Ready=False`.

#### RGD Status Additions

```yaml
status:
  lastIssuedRevision: 3
```

| Field                | Type  | Description                                           |
| -------------------- | ----- | ----------------------------------------------------- |
| `lastIssuedRevision` | int64 | High-water mark for revision allocation. Survives GC. |

The implementation tracks only `lastIssuedRevision` on the RGD status. There are
no `latestObservedGR` or `latestActiveGR` reference fields. The RGD controller
determines the latest revision by listing GraphRevision objects (via a field
index on `spec.snapshot.name`) and checking the in-memory registry directly.
Structured GR reference fields on RGD status are deferred to future work
(revision pinning, automatic rollback).

#### Condition Specification

GraphRevision and RGD conditions have different responsibilities.

**GraphRevision conditions**

- `GraphVerified=True` means the immutable snapshot compiled successfully.
- `GraphVerified=False` means compilation failed for that snapshot.
- `Ready` is the top-level condition for the revision and mirrors compilation
  state:
  - `Pending => Ready=Unknown`
  - `Active => Ready=True`
  - `Failed => Ready=False`

**RGD conditions**

The RGD continues to own the generated CRD and the dynamic controller. Its
conditions describe the serving infrastructure for the kind and whether the
revision lineage has converged.

- `GraphAccepted=True` means the current `rgd.spec` passed RGD-side
  pre-flight compilation and was eligible for GraphRevision issuance.
- `GraphRevisionsResolved=True` means the latest GraphRevision lineage has
  settled and the selected revision is ready to serve. This condition surfaces
  intermediate states as Unknown with specific reasons:
  `WaitingForGraphRevisionSettlement` (terminating revisions blocking),
  `WaitingForGraphRevisionWarmup` (registry not yet populated),
  `WaitingForGraphRevisionCompilation` (latest revision still compiling).
- `KindReady=True` means the generated CRD exists and is established.
- `ControllerReady=True` means the dynamic controller for this RGD is running.

GC health is not surfaced as a condition. GC failures are not actionable from
the RGD's perspective — operators cannot fix GC by modifying the RGD. Instead,
GC health is observable via the `rgd_graph_revision_gc_deleted_total` and
`rgd_graph_revision_gc_errors_total` metrics.

The derived top-level state is:

- `Active = GraphAccepted && KindReady && ControllerReady`

`GraphRevisionsResolved` is an informational condition. It does not participate
in the `Active`/`Ready` calculation. An RGD can be Active while only a subset
of revisions are valid - this is expected after restarts when older
GraphRevisions may be re-evaluated against a different cluster shape.

**Blast radius of lineage dependency.** The hard dependency on lineage
resolution means a GR controller outage blocks new RGD spec changes from taking
effect (the new GR stays Pending, the RGD controller requeues with
`WaitingForGraphRevisionCompilation`). However, existing instances continue
serving on their current Active revision - the registry retains previously
compiled entries in memory. The blast radius is "new RGD changes don't
propagate," not "existing instances stop working." Operators can diagnose a
stuck `WaitingForGraphRevisionCompilation` state by checking GR controller pod
health and logs, or via the `graph_revision_compile_total` metric (a
flatline when activity is expected signals a stuck controller).

**Latest revision failure is a hard block.** If the latest GraphRevision fails
compilation, the RGD controller sets `GraphAccepted=False` with reason
`InvalidResourceGraph` and returns an error. The instance controller sees
`Failed` from `GetLatestRevision()` and returns a terminal error - no requeue.
There is no automatic fallback to the previous Active revision.

This is a deliberate choice for this KREP. The alternative - silently falling
back to the last Active revision - is safer for uptime but hides broken specs.
An operator who pushes a bad RGD change should see it fail loudly rather than
discover hours later that their change never took effect. The conditions make
the failure visible:

- RGD: `GraphAccepted=False`, `GraphRevisionsResolved=False`
- GraphRevision: `GraphVerified=False`, `Ready=False`

The previous Active revision remains in the registry and is not evicted. This is
the foundation for automatic rollback (future work): the data to fall back to
already exists, what's missing is the policy for when to do so. When automatic
rollback lands, the behavior changes from "hard block, operator fixes" to
"degrade to previous revision, surface the failure as a condition, operator
decides whether to fix or roll forward."

Older retained revisions that fail recompilation on restart do not block the
RGD. Only the latest revision's state matters for lineage resolution. An RGD can
be Active while older revisions are Failed - this is expected when older
snapshots are re-evaluated against a changed cluster shape.

## Scope

### In Scope

- GraphRevision CRD and controller
- In-memory spec hashing for change detection (algorithm is an implementation
  detail)
- In-memory revision registry with Pending/Active/Failed states
- RGD controller as revision issuer (create, GC)
- Instance controller consuming compiled graphs from registry
- OwnerReference-based GC on RGD deletion
- Retention-based GC (configurable max revisions per RGD)
- Finalizer on GraphRevision for clean registry eviction on delete
- Startup replay: GR controller rebuilds the in-memory registry from persisted
  GraphRevisions after restart, computing hashes from snapshot specs

### Out of Scope

The following are planned follow-ups that build on this foundation. They are the
primary motivation for this work - GraphRevisions exist so these features have
something to operate on:

- **Revision pinning per instance** - the immediate next step. An annotation or
  spec field pins an instance to a specific revision number, enabling canary
  rollouts and rollback. The `GraphRevisionResolver` interface already supports
  `GetGraphRevision(revision)` for this purpose.
- **Leveled topologies** - rolling out revision N+1 across instances in
  controlled levels with health checks between waves. Requires pinning. Related
  to [KREP-005](https://github.com/kubernetes-sigs/kro/pull/859).
- **Propagation control integration** - connecting GraphRevisions to
  [KREP-006](https://github.com/kubernetes-sigs/kro/pull/861) mechanisms.
  GraphRevisions provide the revision identity that propagation control operates
  on.
- **Graph diffing** - CLI/kubectl plugin for diffing two revisions. Trivial
  given immutable `snapshot.spec` fields.
- **Automatic rollback** - falling back to the previous Active revision on
  compile failure. Requires the instance controller to query non-latest
  revisions.
- **Revision-aware status** - surface per-instance revision information in
  instance status for fleet rollout monitoring.

Not planned:

- **Webhook admission** - no admission webhook beyond the CEL immutability rule.

### Non-Goals

- Replace the graph builder/compiler. The compilation logic is unchanged.
- Add versioning to the RGD API itself. GraphRevisions are an implementation
  detail, not a user-facing versioning scheme.
- Provide a migration tool. The transition is transparent; existing RGDs get
  their first GraphRevision on the next reconcile.

## Design Details

### Watch and Predicate Logic

The RGD controller uses custom predicates to avoid unnecessary reconciliation:

- **Spec changes**: reconciles on generation changes (spec mutation) and when
  `deletionTimestamp` transitions from zero to non-zero. Plain Delete events
  (object already gone) are ignored - Kubernetes does not bump generation when
  setting deletionTimestamp, so a plain `GenerationChangedPredicate` would miss
  the deletion hook.
- **Annotation changes**: reconciles when the `allow-breaking-changes`
  annotation transitions between true/false. Does not fire on creation or
  deletion.
- **CRD watches**: the RGD controller watches its generated CRD via
  `WatchesMetadata` and reconciles the owning RGD on CRD Update and Delete (not
  Create). This ensures the RGD re-converges if its CRD is externally modified
  or deleted.

GraphRevisions are listed via a **field index on `spec.snapshot.name`** rather
than a label selector. The field is immutable (part of the CEL-validated spec),
so it cannot be modified externally - unlike labels, which could be changed and
break lineage queries.

### Revision Lifecycle

The RGD reconcile separates into two phases: **lineage** then **serving**.

```
 RGD spec change
       │
       ▼
 ┌─────────────┐     ┌──────────────┐     ┌─────────────────┐
 │ Hash spec   │────▶│ List GRs     │────▶│ Resolve lineage │
 └─────────────┘     └──────────────┘     └───────┬─────────┘
                                                  │
                      ┌───────────────────────────┼───────────────────┐
                      │                           │                   │
                      ▼                           ▼                   ▼
                  requeue                    issue new GR           error
                  • terminating GRs          • no GRs exist         • GR Failed
                  • not in cache yet         • hash mismatch        • compile fails
                  • GR still Pending              │
                                                  ▼
                                           ┌─────────────┐
                                           │ Compile spec │
                                           │ Create GR    │──▶ requeue
                                           └─────────────┘

 ── on next reconcile, lineage resolves to Active ──

 ┌─────────────┐     ┌──────────────────┐
 │ Ensure CRD  │────▶│ Register ctrl    │────▶ Active
 └─────────────┘     └──────────────────┘
```

The lineage phase lists GraphRevisions and splits them into live and
terminating sets. **If any terminating GRs exist, lineage blocks with
`WaitingForGraphRevisionSettlement` and requeues.** This prevents races during
GC: a terminating GR's finalizer has not yet evicted its registry entry, so
issuing a new revision while old entries are still in flight could cause the
registry to briefly hold stale state. Settlement ensures all in-deletion
revisions are fully gone before the next lineage decision.

The GR controller runs asynchronously:

```
 ┌────────────┐     ┌───────────────────┐     Active  (graph cached)
 │ GR created │────▶│ Compile snapshot  │────▶ or
 └────────────┘     └───────────────────┘     Failed
```

### Hashing

The hash function (`pkg/graph/hash`) normalizes the RGD spec before hashing:

1. DeepCopy the spec to avoid mutation.
2. For each `RawExtension` field (schema.spec, schema.types, schema.status, each
   resource template): unmarshal, re-marshal as canonical JSON. This eliminates
   differences from YAML formatting, comment changes, or field reordering.
3. Sort resources by ID for stable ordering.
4. Sort slice fields (`readyWhen`, `includeWhen`) within each resource. `forEach`
   order is preserved because collection expansion order is semantic.
5. JSON-marshal the normalized spec (Go struct field order is stable).
6. FNV-64a the result, hex-encode.

The RGD controller computes the hash at creation time for deduplication and
seeds the registry entry. At startup, the GR controller recomputes hashes from
snapshot specs - see Startup Behavior.

A separate FNV-32a hash of the **RGD name** (not the spec) is used in
GraphRevision object naming as a collision guard for truncated names. The same
RGD always produces the same name hash regardless of spec changes, giving
consistent naming across revisions.

### In-Memory Registry

The registry is a `sync.RWMutex`-protected map from RGD name to a bucket of
revision entries:

```go
type Registry struct {
    mu    sync.RWMutex
    byRGD map[string]*rgdBucket
}

type rgdBucket struct {
    entries        map[int64]Entry
    latestRevision int64
    hasLatest      bool
}

type Entry struct {
    RGDName       string
    Revision      int64
    SpecHash      string          // computed in-process, not from the API object
    State         RevisionState
    CompiledGraph *graph.Graph    // populated for Active entries
}
```

The bucket tracks only a single `latestRevision` pointer (the highest revision
number seen), not separate observed/active pointers. Callers check the entry's
`State` field to determine whether the latest revision is servable.

Key operations:

- `Put(entry)` - upsert; advances latest pointer only when a strictly newer
  revision is written.
- `Get(rgdName, revision)` - point lookup.
- `Latest(rgdName)` - highest revision entry (any state).
- `HasAll(rgdName, revisions)` - membership check for a list of revision
  numbers.
- `Delete(rgdName, revision)` - remove one; recompute latest pointer if needed.
- `DeleteAll(rgdName)` - evict all entries for an RGD (used during RGD
  deletion).
- `DeleteRevisionsBefore(rgdName, minRevision)` - bulk prune for retention GC.

### Garbage Collection

Two GC mechanisms:

**OwnerReference cascade.** Each GraphRevision has an OwnerReference pointing to
its source RGD. When the RGD is deleted, Kubernetes garbage collection deletes
all its GraphRevisions. The GR controller's finalizer ensures registry entries
are evicted before the API object disappears. The RGD controller does not evict
registry entries itself during cleanup - each GR's finalizer handles its own
eviction as the cascade proceeds. On RGD deletion, the microcontroller is shut
down before any cleanup begins to prevent data races between the instance
controller (reading from registry) and the cascade (evicting from registry).

**Retention floor.** Retention GC runs inline at the end of each RGD reconcile,
after the serving state is established. For each RGD, if the number of live
(non-terminating) GraphRevisions exceeds `--rgd-max-graph-revisions` (default
5), it sorts revision numbers, computes a retention floor (the
Nth-from-latest), and deletes all revisions below that floor. Registry eviction
is handled by the GR controller's finalizer as each revision is deleted — the
RGD controller does not eagerly evict registry entries during GC. GC failures
are logged and recorded as a metric but do not block the reconcile.

The default of 5 is a pragmatic choice, not derived from workload analysis. It
is large enough to support rollback to a recent known-good state plus some
buffer for audit and debugging. The value is tunable via the flag. Retention
pruning does not break lineage continuation: the `max` mechanism uses the
highest surviving revision number, not the count of retained revisions.

### Instance Controller Changes

The instance controller no longer receives a `*graph.Graph` directly. Instead it
holds a small resolver interface and receives a registry-backed implementation
for the target RGD:

```go
type GraphRevisionResolver interface {
    GetLatestRevision() (revisions.Entry, bool)
    GetGraphRevision(revision int64) (revisions.Entry, bool)
}

resolver := registry.ResolverForRGD(rgdName)
```

On each reconcile, the instance controller calls `resolver.GetLatestRevision()`.
If the newest issued entry is Active, it uses the compiled graph. If Pending, it
requeues. If Failed, it returns a terminal error. If no entry exists, it
requeues.

**Recovery after terminal error.** When the instance controller returns a
terminal error (Failed revision), no requeue is scheduled. Recovery happens when
the operator fixes the RGD spec and a new Active revision appears: the RGD
controller restarts the dynamic controller for the affected kind, which
re-lists all instances and triggers fresh reconciles against the new revision.

Note: in this KREP, all instances follow the latest revision - the all-at-once
behavior is unchanged. The `GraphRevisionResolver` interface is the extension
point for revision pinning: `GetGraphRevision(revision)` already supports point
lookups by revision number. Follow-up work adds an annotation or spec field on
instances that pins them to a specific revision, using this method to resolve
the pinned graph instead of always calling `GetLatestRevision()`.

The instance controller also receives an explicit `namespaced` bool from the
compiled graph's instance node metadata (`Graph.Instance.Meta.Namespaced`),
which it uses for namespace-scoped API operations and instance labeling.

### GraphRevision Controller

The GR controller uses `reconcile.AsReconciler`. On reconcile:

1. Add finalizer (for clean eviction on delete).
2. Compute the spec hash from `snapshot.spec` and put Pending in registry (only
   if not already present - re-reconciles preserve existing state).
3. Compile the graph from the snapshot spec.
4. On success: prepare an Active entry (with compiled graph and hash), set
   GraphVerified=True.
5. On failure: Put Failed (with hash), set GraphVerified=False.
6. Update status (topological order, resources, conditions). **The Active entry
   is only published to the registry after the status write succeeds.** Until
   then the revision remains Pending, so consumers conservatively requeue. If
   the status write fails, the Active entry is not published and the controller
   returns an error to trigger a re-reconcile. This deferred-publish pattern
   avoids the need for a revert: the registry never sees Active for a revision
   whose status hasn't been persisted.
7. On deletion: evict from registry, remove finalizer.

Status updates use `client.MergeFrom()` with `retry.RetryOnConflict` to handle
concurrent writes.

### Startup Behavior

On restart, the in-memory registry is empty. The system recovers through
concurrent reconciliation:

1. The GR controller's informer lists all persisted GraphRevision objects and
   enqueues them for reconciliation. Each reconcile recomputes the spec hash
   from the snapshot (using the current algorithm - no migration needed after
   hash algorithm changes), compiles the graph, and puts the result into the
   registry as Active or Failed.
2. Meanwhile, the RGD controller reconciles too. It checks whether the latest
   revision for this RGD is present in the registry. If not, it sets
   `WaitingForGraphRevisionWarmup` and requeues after the configured progress
   requeue delay (default 3 seconds). Older retained revisions compile in the
   background and do not block serving.
3. Instance controllers call `resolver.GetLatestRevision()`. If no entry exists,
   they requeue with the configured default requeue duration.

Instances requeue (not block) until their latest revision appears. Warmup time
for serving readiness is bounded by the number of GraphRevisions multiplied by
per-revision compile time, and is tunable via
`--graph-revision-concurrent-reconciles` (default 1).

### Naming Convention

GraphRevision names follow the pattern `{rgdName}-r{N:05d}` in the common case,
where `N` is the zero-padded decimal revision number (up to 99,999 revisions
per RGD for free lexicographic sorting in kubectl).

Common case (name + suffix ≤ 253):

    my-webapp-r00001

Overflow case (name would exceed 253):

    {truncated-name}-r{N:05d}-{fnv32a(rgdName)}

The hash suffix is only appended when truncation occurs, and is an FNV-32a hash
of the full, untruncated RGD name. This ensures two different RGD names that
truncate to the same prefix still produce unique revision names. The same RGD
always produces the same hash across all revisions.

Trailing dots and hyphens are trimmed from the truncated RGD name to keep the
joined name DNS-1123 valid.

### Metrics

Metrics are split across the GR controller, the RGD controller, and the
in-memory registry. Together they make the compilation lifecycle, resolution
decisions, and registry state observable without log access.

**GR controller metrics** (`pkg/controller/graphrevision/metrics.go`):

| Metric                                               | Type      | Labels                         | What it answers                                                                                                                  |
| ---------------------------------------------------- | --------- | ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------- |
| `graph_revision_compile_total`                   | Counter   | `result={success,failed}`      | Are revisions compiling? A flatline means the GR controller is stuck. A spike in failures means specs are broken.                |
| `graph_revision_compile_duration_seconds`        | Histogram | `result={success,failed}`      | How long does compilation take? Informs `--graph-revision-concurrent-reconciles` tuning and catches regressions.                 |
| `graph_revision_status_update_errors_total`      | Counter   | -                              | Are status writes failing? Persistent failures mean Active entries never publish, leaving revisions stuck as Pending.             |
| `graph_revision_activation_deferred_total`       | Counter   | -                              | How often is Active publish deferred due to status write failure? Complement to status update errors.                             |
| `graph_revision_finalizer_evictions_total`       | Counter   | -                              | How many registry evictions via finalizer cleanup? Tracks cascade-delete health.                                                 |

**RGD controller metrics** (`pkg/controller/resourcegraphdefinition/metrics.go`):

| Metric                                               | Type      | Labels                                           | What it answers                                                                                                  |
| ---------------------------------------------------- | --------- | ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------- |
| `rgd_graph_revision_issue_total`                 | Counter   | `reason={no_revision,spec_changed}`              | Why are new revisions being created?                                                                             |
| `rgd_graph_revision_wait_total`                  | Counter   | `reason={warming_up,compiling,settling}`         | What is blocking lineage resolution?                                                                             |
| `rgd_graph_revision_resolution_total`            | Counter   | `result={serve,issue,wait,failed}`               | Distribution of resolution outcomes across reconciles.                                                           |
| `rgd_graph_revision_registry_miss_total`         | Counter   | `reason={latest_not_warmed,runtime_entry_missing}` | How often does the RGD controller observe revisions ahead of the registry?                                       |
| `rgd_graph_revision_gc_deleted_total`            | Counter   | -                                                | Is retention GC running? If entries grow beyond max but deletions aren't incrementing, GC is broken.             |
| `rgd_graph_revision_gc_errors_total`             | Counter   | -                                                | Is retention GC healthy? GC failures are not actionable from the RGD and are better surfaced as an alertable metric. |

**Registry metrics** (`pkg/graph/revisions/metrics.go`):

| Metric                                               | Type      | Labels                         | What it answers                                                                                                  |
| ---------------------------------------------------- | --------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------- |
| `graph_revision_registry_entries`                | Gauge     | `state={pending,active,failed}` | Are revisions healthy? Pending entries that don't transition are stuck. Failed accumulating signals spec problems. |
| `graph_revision_registry_transitions_total`      | Counter   | `from`, `to`                   | State transition flow between Pending/Active/Failed.                                                             |
| `graph_revision_registry_evictions_total`        | Counter   | -                              | Total evictions from the registry (Delete, DeleteAll, DeleteRevisionsBefore).                                    |

**Explicitly deferred to pinning/leveled topologies work:** per-instance revision
gauge (`instance_active_revision`), fleet revision distribution. Right now
every instance follows latest - there is nothing to distribute. When pinning
lands, these become essential.

**Not instrumented:** registry read latency (in-memory RWMutex map, not a
bottleneck), GC duration (downstream effects visible via other metrics),
per-object counts (registry gauge is authoritative).

### New Controller Flags

| Flag                                     | Default | Description                                                    |
| ---------------------------------------- | ------- | -------------------------------------------------------------- |
| `--graph-revision-concurrent-reconciles` | 1       | Parallel GR reconciles.                                        |
| `--rgd-max-graph-revisions`              | 5       | Max retained GraphRevisions per RGD.                           |
| `--rgd-progress-requeue-delay`           | 3s      | Delay before requeuing when waiting for revision progress.     |

## Backward Compatibility

GraphRevisions are purely additive. On upgrade:

1. The new RGD controller creates the first GraphRevision for each active RGD on
   its next reconcile.
2. The GR controller compiles it and stores the result.
3. Instance controllers resolve from the registry instead of the closure.

No migration is needed. RGDs that existed before the upgrade get their first
GraphRevision automatically.

## Future Work

This KREP builds the foundation. The following capabilities depend on
GraphRevisions and are the primary motivation for this work:

- **Revision pinning.** An annotation or spec field on instances that pins them
  to a specific revision number. New instances target revision N+1 while
  existing instances stay on N until explicitly promoted. The
  `GraphRevisionResolver` interface already supports
  `GetGraphRevision(revision)` for this purpose.
- **Leveled topologies.** With pinning in place, roll out revision N+1 across
  instances in configurable levels - e.g., 10% at a time, with health checks or
  manual approval gates between waves. See
  [KREP-005](https://github.com/kubernetes-sigs/kro/pull/859).
- **Propagation control integration.**
  [KREP-006](https://github.com/kubernetes-sigs/kro/pull/861) describes
  mechanisms for controlling how changes flow through instances. GraphRevisions
  provide the stable revision identity that propagation control operates on -
  you cannot do a staged rollout without something stable to roll out from and
  to.
- **Graph diffing.** A `kubectl` plugin or CLI command that diffs two revisions'
  `snapshot.spec` fields, giving operators confidence before promoting a new
  revision across instances.
- **Automatic rollback.** If a new revision fails compilation or causes instance
  failures, fall back to the most recent Active revision instead of blocking all
  instances.
- **Revision-aware status.** Surface per-instance revision information (which
  revision is this instance running?) in instance status, enabling operators to
  monitor rollout progress across a fleet.
