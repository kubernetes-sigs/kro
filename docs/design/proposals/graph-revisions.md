# KREP-013: Graph Revisions

## Summary

When an RGD spec changes, kro recompiles the graph and hot-swaps it into the
running micro-controller. There is no record of what was running before, no way
to diff what changed, no mechanism to pin an instance to a known-good graph, and
no foundation for controlled rollouts across instances.

This proposal introduces `GraphRevision`, a cluster-scoped immutable snapshot of
an RGD spec. When an RGD spec change passes RGD-side pre-flight compilation, the
RGD controller creates a new GraphRevision object. A dedicated GraphRevision
controller compiles the snapshot and stores the result in a shared in-memory
registry. The RGD controller continues to own the generated CRD and dynamic
controller. Instance controllers resolve their compiled graph from the registry
instead of receiving it directly.

GraphRevisions are the foundation for revision pinning, batch instance
migration, graph diffing, and propagation control. This KREP covers the base
layer: snapshot issuance, compilation, registry, and instance resolution.
Follow-up work builds on this to add pinning, diffing, and leveled rollouts.

GraphRevisions are purely additive. Existing RGDs continue to work unchanged.

## Motivation

The current model has no concept of graph history. When an RGD spec changes, the
old compiled graph is gone. This creates three categories of problems:

**No rollback or diffing.** Operators cannot see what changed between RGD edits.
When an update breaks instances, there is no way to inspect the previous graph,
diff it against the current one, or roll back to a known-good state. Debugging
requires reading git history (if it exists) rather than cluster state.

**No controlled migration.** All instances immediately pick up the new graph on
their next reconcile. There is no way to migrate instances in batches, test a
new revision against a canary set, or hold some instances on the old graph while
validating the new one. For platform teams managing hundreds of instances, an
RGD change is all-or-nothing.

**No foundation for propagation control.** Features like leveled applies (apply
changes to a subset of instances, observe, then proceed — see
[KREP-005](https://github.com/kubernetes-sigs/kro/pull/859) and
[KREP-006](https://github.com/kubernetes-sigs/kro/pull/861)) require a stable
revision identity that instances can be pinned to. Without immutable snapshots,
there is nothing to pin to. The instance controller receives its graph through a
closure — there is no addressable revision object.

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
behind a manual review gate or promote the remaining 195 in batches. If
something goes wrong, revision 3 still exists as an immutable snapshot they can
roll back to.

## Proposal

### Overview

A new cluster-scoped CRD, `GraphRevision`, stores an immutable snapshot of an
RGD spec along with the hash that produced it. A new controller reconciles
GraphRevision objects by compiling them and storing the compiled graph in a
shared in-memory registry. The RGD controller becomes the issuer of revisions
after pre-flight validation, but it remains the owner of the generated CRD and
the dynamic controller. The instance controller becomes a consumer of the
registry.

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
name, so a recreated RGD with the same name continues from the existing
lineage. The RGD persists its high-water mark in `status.lastIssuedRevision` so
revision numbers never go backwards within a lineage, even across GC cycles.

**Deterministic hashing.** A canonical SHA-256 hash of the RGD spec determines
whether a new revision is needed. RawExtension payloads are normalized (YAML to
canonical JSON, sorted map keys) before hashing. If the hash matches the latest
GraphRevision, no new revision is created.

**Name-based lineage.** GraphRevisions are grouped by source RGD name. The RGD
name remains the authoritative identity for revision lineage decisions. A
deleted-and-recreated RGD with the same name continues the existing lineage
instead of starting from revision 1.

**Three-state registry.** The in-memory registry tracks each revision as Pending
(created, not yet compiled), Active (compiled, graph available), or Failed
(compilation error). These states are internal scheduling signals between
controllers. The external GraphRevision API surfaces them through conditions
instead of a separate state enum.

### API Changes

#### New CRD: GraphRevision

```yaml
apiVersion: kro.run/v1alpha1
kind: GraphRevision
metadata:
  name: my-webapp-r000003
  labels:
    kro.run/rgd-name: my-webapp
    kro.run/rgd-uid: "abc-123"
  ownerReferences:
    - apiVersion: kro.run/v1alpha1
      kind: ResourceGraphDefinition
      name: my-webapp
spec:
  resourceGraphDefinitionName: my-webapp
  revision: 3
  specHash: "a1b2c3d4..."
  definitionSpec: # immutable copy of rgd.spec
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

Spec fields:

| Field                         | Type    | Description                                     |
| ----------------------------- | ------- | ----------------------------------------------- |
| `resourceGraphDefinitionName` | string  | Source RGD name. Human-readable provenance.     |
| `revision`                    | int64   | Monotonic revision number per RGD lineage.      |
| `specHash`                    | string  | SHA-256 of the canonicalized RGD spec.          |
| `definitionSpec`              | RGDSpec | Immutable snapshot of the source RGD spec.      |

Status conditions follow the established kro pattern (KREP status-conditions):
`Ready` as top-level, `GraphVerified` as sub-condition. The mapping is:
`Pending => Ready=Unknown`, `Active => Ready=True`, `Failed => Ready=False`.

#### RGD Status Additions

```yaml
status:
  latestObservedGR:
    name: my-webapp-r000003
    revision: 3
    specHash: "a1b2c3d4..."
  latestActiveGR:
    name: my-webapp-r000003
    revision: 3
    specHash: "a1b2c3d4..."
  lastIssuedRevision: 3
```

| Field                | Type                   | Description                                           |
| -------------------- | ---------------------- | ----------------------------------------------------- |
| `latestObservedGR`   | GraphRevisionReference | Most recent GraphRevision found for this RGD.         |
| `latestActiveGR`     | GraphRevisionReference | Most recent successfully compiled GraphRevision.      |
| `lastIssuedRevision` | int64                  | High-water mark for revision allocation. Survives GC. |

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
current desired spec has produced a usable latest revision.

- `ResourceGraphAccepted=True` means the current `rgd.spec` passed RGD-side
  pre-flight compilation and was eligible for GraphRevision issuance.
- `KindReady=True` means the generated CRD exists and is established.
- `ControllerReady=True` means the dynamic controller for this RGD is running.
- `LatestObservedGRReady=True` means `status.latestObservedGR` exists and that
  referenced GraphRevision has `Ready=True`.

The derived top-level conditions are:

- `Active = KindReady && ControllerReady`
- `Ready = ResourceGraphAccepted && KindReady && ControllerReady && LatestObservedGRReady`

This makes the degraded case explicit. If the newest desired revision fails but
an older revision remains available, the RGD can stay `Active=True` while
becoming `Ready=False` for the current spec.

## Scope

### In Scope

- GraphRevision CRD and controller
- Deterministic spec hashing
- In-memory revision registry with Pending/Active/Failed states
- RGD controller as revision issuer (create, GC)
- Instance controller consuming compiled graphs from registry
- OwnerReference-based GC on RGD deletion
- Retention-based GC (configurable max revisions per RGD)
- Finalizer on GraphRevision for clean registry eviction on delete
- Startup replay: GR controller rebuilds the in-memory registry from persisted
  GraphRevisions after restart

### Out of Scope

The following are planned follow-ups that build on this foundation:

- **Revision pinning per instance** - Instances always follow the newest issued
  revision in this KREP. If that revision is Pending they requeue, and if it
  Failed they surface a terminal error. Pinning to a specific revision (for
  canary rollouts, batch migration, and rollback) is the immediate next step.
- **Batch migration and leveled applies** - Migrating instances from one
  revision to another in controlled batches. Requires pinning first. Related to
  [KREP-005: Level-based Topological Sorting](https://github.com/kubernetes-sigs/kro/pull/859).
- **Graph diffing tooling** - CLI/kubectl plugin for diffing two revisions. The
  data model from this KREP makes this straightforward.
- **Propagation control integration** - Connecting GraphRevisions to
  [KREP-006: Propagation Control](https://github.com/kubernetes-sigs/kro/pull/861)
  mechanisms.
- **Automatic rollback** - Falling back to the previous Active revision on
  compile failure. Requires the instance controller to query non-latest
  revisions.

The following are not planned:

- **Cross-cluster replication** - GraphRevisions are cluster-local.
- **Webhook admission** - No admission webhook for GraphRevision validation
  beyond the CEL immutability rule.

### Non-Goals

- Replace the graph builder/compiler. The compilation logic is unchanged.
- Add versioning to the RGD API itself. GraphRevisions are an implementation
  detail, not a user-facing versioning scheme.
- Provide a migration tool. The transition is transparent; existing RGDs get
  their first GraphRevision on the next reconcile.

## Design Details

### Revision Lifecycle

```
RGD spec changes
       │
       ▼
  Hash current spec ──── matches latest GR hash? ──── yes ──→ skip issuance
       │
       no
       │
       ▼
  Pre-flight compile current RGD spec
       │
       ├── fail ──→ Mark ResourceGraphAccepted=False on RGD
       │            No new GraphRevision is created
       │
       └── pass ──→ Mark ResourceGraphAccepted=True on RGD
                    Ensure CRD + micro-controller (still owned by RGD)
                    Create GraphRevision (revision = lastIssued + 1)
                    Update lastIssuedRevision
                    Update latestObservedGR
                    Put Pending in registry
                              │
                              ▼
                    GR controller picks up ──→ compile ──→ success? ─── yes ──→ Put Active
                              │                                    │             Mark GR Ready=True
                              │                                    │             Update latestActiveGR
                              │                                    no
                              │                                    │
                              │                                    ▼
                              │                              Put Failed
                              │                              Mark GR Ready=False
                              │                              latestActiveGR unchanged
```

### Hashing

The hash function normalizes the RGD spec before hashing:

1. DeepCopy the spec to avoid mutation.
2. For each `RawExtension` field (schema.spec, schema.types, schema.status, each
   resource template): unmarshal from YAML, re-marshal as canonical JSON. This
   eliminates differences from YAML formatting, comment changes, or field
   reordering.
3. JSON-marshal the normalized spec (Go struct field order is stable).
4. SHA-256 the result.

This produces identical hashes for semantically identical specs regardless of
YAML formatting.

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
    latestObserved int64
    latestActive   int64
    hasObserved    bool
    hasActive      bool
}
```

Key operations:

- `Put(entry)` — upsert; advances latest-observed and latest-active pointers as appropriate.
- `Get(rgdName, revision)` — point lookup.
- `Latest(rgdName)` — newest observed revision.
- `LatestActive(rgdName)` — newest serving revision.
- `Delete(rgdName, revision)` — remove one; recompute cached pointers if needed.
- `DeleteBelow(rgdName, minRevision)` — bulk prune for GC.

The RGD controller does not block on replaying every retained revision into
memory. Issuance decisions come from the API-server view of the latest
GraphRevision hash. The GR controller rebuilds the registry in the background on
startup, and RGD readiness depends on the current serving revision rather than
full historical replay.

### Garbage Collection

Two GC mechanisms:

**OwnerReference cascade.** Each GraphRevision has an OwnerReference pointing to
its source RGD. When the RGD is deleted, Kubernetes garbage collection deletes
all its GraphRevisions. The GR controller's finalizer ensures registry entries
are evicted before the API object disappears.

**Retention floor.** The RGD controller runs GC at the end of each reconcile. If
the number of GraphRevisions exceeds `--rgd-max-graph-revisions` (default 20),
it deletes the oldest revisions beyond the retention limit. However, GC MUST
retain the newest observed revision and the newest Active revision, even if one
or both fall below the retention floor. This preserves the current desired
revision together with the last known-good revision for the lineage. The
registry is pruned in parallel via `DeleteBelow`, subject to the same
invariant.

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

On each reconcile, the instance controller calls `resolver.GetLatestRevision()`. If the
newest issued entry is Active, it uses the compiled graph. If Pending, it
requeues. If Failed, it returns a terminal error. If no entry exists, it
requeues. Falling back to an older Active revision is a follow-up with pinning
and migration policy.

### GraphRevision Controller

The GR controller uses `reconcile.AsReconciler` with
`GenerationChangedPredicate` (generation only changes once since spec is
immutable). On reconcile:

1. Add finalizer (for clean eviction on delete).
2. Put Pending in registry.
3. Compile the graph from the snapshot spec.
4. On success: Put Active (with compiled graph), set GraphVerified=True.
5. On failure: Put Failed, set GraphVerified=False.
6. Update status (topological order, resources, conditions).
7. On deletion: evict from registry, remove finalizer.

### Naming Convention

GraphRevision names follow the pattern `{rgdName}-rNNNNNN` for normal-size names.
For very long RGD names that must be truncated to stay under the 253-byte limit,
the name is `{truncatedRGDName}-{shortHash}-rNNNNNN`, where `shortHash` is a
short hash suffix derived from `rgdName` and `NNNNNN` is a zero-padded decimal
revision number.
If the RGD name would cause the total to exceed 253 characters, the RGD name
prefix is truncated.

### New Controller Flags

| Flag                                     | Default | Description                          |
| ---------------------------------------- | ------- | ------------------------------------ |
| `--graph-revision-concurrent-reconciles` | 1       | Parallel GR reconciles.              |
| `--rgd-max-graph-revisions`              | 20      | Max retained GraphRevisions per RGD. |

## Backward Compatibility

GraphRevisions are purely additive. On upgrade:

1. The new RGD controller creates the first GraphRevision for each active RGD on
   its next reconcile.
2. The GR controller compiles it and stores the result.
3. Instance controllers resolve from the registry instead of the closure.

No migration is needed. RGDs that existed before the upgrade get their first
GraphRevision automatically.

## Future Work

This KREP intentionally builds only the foundation. The following capabilities
depend on GraphRevisions existing and are the primary motivation for this work:

- **Revision pinning.** An annotation or spec field on instances that pins them
  to a specific revision number. This is the core building block for controlled
  rollouts: new instances can target revision N+1 while existing instances stay
  on revision N until explicitly promoted. The `GraphRevisionResolver` interface
  already supports `GetGraphRevision(revision)` for this purpose.
- **Batch instance migration.** With pinning in place, a migration controller
  (or the RGD controller itself) can move instances from revision N to N+1 in
  configurable batches — e.g., 10% at a time, with health checks or manual
  approval gates between batches. This is the "leveled apply" pattern from
  [KREP-005](https://github.com/kubernetes-sigs/kro/pull/859).
- **Graph diffing.** Since each GraphRevision stores an immutable snapshot of
  the RGD spec, diffing two revisions is a straightforward comparison of their
  `definitionSpec` fields. A `kubectl` plugin or CLI command can show exactly
  what changed between revisions, giving operators confidence before promoting a
  new revision across instances.
- **Propagation control integration.**
  [KREP-006: Propagation Control](https://github.com/kubernetes-sigs/kro/pull/861)
  describes mechanisms for controlling how changes flow through instances.
  GraphRevisions provide the revision identity that propagation control operates
  on — you cannot do a staged rollout without something stable to roll out from
  and to.
- **Automatic rollback.** If a new revision fails compilation or causes instance
  failures, fall back to the most recent Active revision instead of blocking all
  instances.
- **Revision-aware status.** Surface per-instance revision information (which
  revision is this instance running?) in instance status, enabling operators to
  monitor migration progress across a fleet.
