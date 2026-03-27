# KREP: Level-Aware Graph Synchronization for the Instance Controller

**Authors:** Jakob Moller
**Status:** Draft

## Related Proposals and References

| Reference | Title | Relationship |
|-----------|-------|--------------|
| [KREP-003] | Level-based topological sorting | Foundation: provides Kahn's algorithm and level grouping |
| [KREP-006] | Propagation control | Extension: `propagateWhen` gates integrated into wavefront |
| [KREP-014] | Resource lifecycles | Extension: Adopt/Orphan policies affect diff and prune phases |
| [KREP-022] | `managedResources` in instance status | Consumer: wavefront produces data for managedResources |
| [KEP-3659] | ApplySet: kubectl apply --prune | Specification: inventory design extends ApplySet |
| [GraphRevision CRD](https://kro.run/api/crds/graphrevision) | GraphRevision API | Data source: structural DAG snapshot |
| [SimpleSchema](https://kro.run/api/specifications/simple-schema) | SimpleSchema specification | Data source: RGD schema definition |
| [pkg/controller/instance](https://pkg.go.dev/github.com/kro-run/kro/pkg/controller/instance) | Instance controller (current) | Refactor target: existing code to be evolved |

---

## Summary

The instance controller today applies resources sequentially with no
parallelism, no ordered deletion, and no way to track which GraphRevision each
resource was last reconciled against. This proposal replaces that model with a
**level-aware wavefront synchronizer** that:

1. **Applies resources in parallel within dependency levels** ([KREP-003]),
   while preserving strict ordering *between* levels.
2. **Gates mutations** via `propagateWhen` ([KREP-006]), controlling when
   changes are allowed to propagate.
3. **Tracks managed resources in a level-ordered inventory** that extends
   the ApplySet specification ([KEP-3659]) with topological ordering, enabling
   correct reverse-order deletion.
4. **Tracks per-node GraphRevision**, enabling efficient diffing and ordered
   rollout when the RGD changes.

**How to read this proposal:** [Design](#design) describes the four-phase
reconcile loop and is the core of the proposal. [Level-Aware Inventory
Management](#level-aware-inventory-management) explains the storage scheme.
[Revision Migration](#revision-migration-n-1-to-n) covers GraphRevision
transitions. [Observability](#observability) starts with a debugging guide.
[Implementation Plan](#implementation-plan) maps current code to proposed
changes.

---

## Motivation

### Current state

The instance controller (`pkg/controller/instance`) today processes resources
sequentially in topological order. The `Controller.Reconcile` method walks the
DAG node-by-node: resolve CEL variables via `runtime.Synchronize()`, apply via
SSA, check `readyWhen`, advance. The `InstanceState` type tracks per-node state
as a flat `map[string]*ResourceState`. Node states (defined in
`api/v1alpha1/instance_state.go`, refactored in
[PR #970](https://github.com/kubernetes-sigs/kro/pull/970)) include `Synced`,
`InProgress`, `Error`, `WaitingForReadiness`, `Skipped`, `Deleting`, and
`Deleted`.

This works but has known limitations:

- **No parallelism.** Independent branches serialize unnecessarily. A graph with
  two independent subtrees of depth 5 takes 10 sequential steps instead of 5.
- **No propagation control.** Every reconciliation applies all pending changes
  immediately. There is no mechanism to gate mutation rate, enforce maintenance
  windows, or do incremental rollouts across `forEach` collections.
- **Flat inventory.** The current ApplySet tracks all managed resources as a
  single flat set. Pruning iterates this set without ordering guarantees,
  which means a Service might be deleted before the Deployment it fronts, or a
  Namespace before the resources inside it.
- **No revision-aware reconciliation.** When the RGD changes and a new
  [GraphRevision](https://kro.run/api/crds/graphrevision) is issued, the
  controller has no structured way to determine which resources need updating
  versus which are already at the latest revision.

### Design goals

1. **Correct ordered creation and deletion.** Resources are created in forward
   topological order and pruned in reverse topological order, even across
   reconcile interruptions.
2. **Safe parallelism.** Independent resources within the same dependency level
   execute concurrently, bounded by configurable concurrency limits.
3. **Propagation control integration.** The [KREP-006] `propagateWhen` mechanism
   gates when a node's mutation can *start*, complementing `readyWhen` which
   gates when a node's mutation is *complete*.
4. **Revision-aware convergence.** The synchronizer tracks which
   [GraphRevision](https://kro.run/api/crds/graphrevision) each resource was
   last reconciled against, enabling efficient diffing and ordered rollout of
   RGD changes.
5. **Compatibility with ApplySet.** The inventory scheme is a superset of the
   ApplySet specification ([KEP-3659]). Standard ApplySet tooling can still
   discover and enumerate managed resources; KRO extends the metadata to encode
   level ordering.

---

## Design

### Architecture overview

The synchronizer operates in four phases per reconcile cycle:

```mermaid
flowchart LR
    P["Phase 1\nProject"] --> D["Phase 2\nDiff"]
    D --> W["Phase 3\nExecute\nwavefront"]
    W --> S["Phase 4\nStatus\nupdate"]
    W -- "node becomes ready" --> P
    S -. "watch event\ntriggers new\nreconcile" .-> P

    style P fill:#E1F5EE,stroke:#0F6E56,color:#085041
    style D fill:#E6F1FB,stroke:#185FA5,color:#0C447C
    style W fill:#EEEDFE,stroke:#534AB7,color:#3C3489
    style S fill:#FAEEDA,stroke:#854F0B,color:#633806
```

Each phase may short-circuit or loop based on watch events. The re-projection
arrow from the wavefront back to projection represents the case where a node
reaching `Ready` state causes an `includeWhen` predicate to change, requiring
a partial DAG re-evaluation.

### Phase 1: Project

Projection takes the structural DAG from the current
[GraphRevision](https://kro.run/api/crds/graphrevision) and the instance's
input values, and produces a **runtime DAG**: which resources to create, at
which dependency levels, with fully-rendered templates. Dynamic elements --
`includeWhen`, `forEach`, CEL expressions -- are all resolved here.

```go
type ProjectedDAG struct {
    // Levels is the output of Kahn's algorithm: nodes grouped by dependency
    // depth. Level 0 has no dependencies; level N depends only on levels 0..N-1.
    Levels [][]NodeID

    // Nodes maps each node ID to its projected state.
    Nodes map[NodeID]*ProjectedNode

    // Revision is the target GraphRevision.spec.revision for this reconcile.
    Revision int64
}

type NodeID struct {
    // ResourceID is the logical ID from the RGD (e.g., "deployment").
    ResourceID string

    // ForEachBindings encodes the forEach variable bindings for expanded nodes.
    // nil for non-forEach nodes. Deterministically serialized for stable identity.
    ForEachBindings map[string]string
}

type ProjectedNode struct {
    Included       bool                          // Result of evaluating all includeWhen predicates
    Template       *unstructured.Unstructured    // Fully-rendered Kubernetes resource manifest
    IsExternal     bool                          // externalRef node (read-only)
    Level          int                           // Topological level from Kahn's algorithm
    Dependencies   []NodeID                      // Nodes this node depends on
    ReadyWhen      []string                      // CEL predicates for readiness
    PropagateWhen  []string                      // CEL predicates for mutation gating (KREP-006)
    ResourcePolicy ResourcePolicies              // Adopt/Orphan policies (KREP-014)
}
```

**Projection rules:**

- `externalRef` nodes are resolved first (level -1, conceptually). They
  populate the CEL evaluation context but produce no create/update/delete
  actions. Since they are watched, changes trigger re-projection automatically.
- `includeWhen` predicates are evaluated against the current CEL context. Nodes
  with `Included == false` are excluded from the projected DAG entirely.
- `forEach` expressions are evaluated to produce the expansion set. Each
  combination of bindings produces a distinct `NodeID`.
- After projection, Kahn's algorithm ([KREP-003]) groups included nodes into
  levels.

**Re-projection:** Because `includeWhen` predicates can reference other
resources' status fields, the projected DAG can change during a reconciliation
pass. This is modeled as a fixed-point computation capped at a configurable
depth (default: 5 iterations). The existing cycle detection on
`includeWhen`/`readyWhen` edges prevents true infinite loops; the cap is a
safety net for complex conditional chains.

### Phase 2: Diff

The diff phase compares the projected DAG against the materialized cluster state
to produce a per-node action plan:

```go
type NodeAction int

const (
    // Forward actions (applied in level order 0, 1, 2, ...)
    ActionCreate    NodeAction = iota // In projected DAG, not in cluster
    ActionUpdate                      // In both, template differs from applied
    ActionAdopt                       // Exists, needs ApplySet labels (KREP-014)
    ActionNone                        // In both, matches, readyWhen not satisfied
    ActionReady                       // In both, matches, readyWhen satisfied

    // Reverse actions (applied in reverse level order ..., 2, 1, 0)
    ActionDelete                      // In cluster, not in projected DAG
    ActionOrphan                      // KREP-014: remove labels, keep resource

    // Gating states
    ActionBlocked                     // Dependencies not ready
    ActionGated                       // Dependencies ready, propagateWhen false (KREP-006)
)
```

**Node classification logic:**

For each node in the projected DAG, the diff reads the corresponding live
resource from the cluster (if it exists) and classifies it. The
`kro.run/revision` label on each managed resource enables revision-aware
diffing -- a stale revision forces an update even if the template is identical,
because the new GraphRevision may have changed CEL expressions or predicates:

```
  Resource not in cluster?              --> ActionCreate
  kro.run/revision < target revision?   --> ActionUpdate (force)
  Template differs from live?           --> ActionUpdate
  readyWhen satisfied?                  --> ActionReady
  Otherwise                             --> ActionNone

  In inventory but not in projection?   --> ActionDelete
  KREP-014 adopt policy?               --> ActionAdopt
  KREP-014 orphan policy?              --> ActionOrphan
```

### Phase 3: Execute wavefront

The wavefront processes levels in strict order, with parallelism *within*
each level. A level only begins after all nodes in the previous level have
reached a terminal state (Ready, Error, or Gated). Nodes at the next level
proceed only if their individual dependencies are all Ready (see [Error
isolation](#error-isolation)).

#### Forward wavefront (create/update)

Levels are processed in ascending order (0, 1, 2, ...). Within each level,
all non-gated nodes are applied concurrently up to `maxConcurrency`:

```
DAG:  ConfigMap ──► Deployment ──► Service
      Secret    ──┘              ─► Ingress

Levels after Kahn's algorithm:
  Level 0: [ConfigMap, Secret]       (no dependencies)
  Level 1: [Deployment]              (depends on ConfigMap, Secret)
  Level 2: [Service, Ingress]        (depend on Deployment)

Forward wavefront execution:

  ┌─────────────────────────────────────────────────────────────────┐
  │ Level 0                                                         │
  │                                                                 │
  │   ConfigMap ─── SSA apply ──► exists ──► readyWhen? ──► Ready   │
  │                                          (parallel)             │
  │   Secret ────── SSA apply ──► exists ──► readyWhen? ──► Ready   │
  │                                                                 │
  │   All Ready? ── yes ──► runtime.Synchronize() ──► advance       │
  └─────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
  ┌─────────────────────────────────────────────────────────────────┐
  │ Level 1                                                         │
  │                                                                 │
  │   Deployment ── SSA apply ──► exists ──► readyWhen? ── no       │
  │                                                                 │
  │   Requeue. Wait for watch event (e.g. replicas become ready).   │
  │   Next reconcile re-enters here:                                │
  │                                                                 │
  │   Deployment ── skip apply (unchanged) ─► readyWhen? ── Ready   │
  │                                                                 │
  │   All Ready? ── yes ──► runtime.Synchronize() ──► advance       │
  └─────────────────────────────────────────────────────────────────┘
                                    │
                                    ▼
  ┌─────────────────────────────────────────────────────────────────┐
  │ Level 2                                                         │
  │                                                                 │
  │   Service ──── SSA apply ──► exists ──► readyWhen? ──► Ready    │
  │                                         (parallel)              │
  │   Ingress ──── SSA apply ──► exists ──► readyWhen? ──► Ready    │
  │                                                                 │
  │   All Ready? ── yes ──► instance ACTIVE                         │
  └─────────────────────────────────────────────────────────────────┘
```

Key behaviors:

- **Parallelism within a level.** ConfigMap and Secret are applied
  concurrently. Service and Ingress are applied concurrently. But
  Deployment never starts before both ConfigMap and Secret are `Ready`.
- **readyWhen as a level gate.** The wavefront does not advance to the next
  level until all nodes in the current level reach a terminal state (Ready,
  Error, or Gated). If a node's readyWhen is not yet satisfied, the reconcile
  returns and waits for a watch event. Nodes at the next level whose
  dependencies are all Ready proceed normally; nodes whose dependencies include
  an Error node are marked Blocked (see [Error isolation](#error-isolation)
  below).
- **runtime.Synchronize() between levels.** After a level completes, the
  CEL evaluation context is refreshed with the latest status fields from
  the just-completed resources. This ensures that level 1 templates can
  reference level 0 status values (e.g., a Deployment referencing a
  ConfigMap's resourceVersion).
- **Concurrency bound.** A semaphore limits parallel SSA applies to
  `maxConcurrency` (default: 10) to avoid overwhelming the API server.

#### Reverse wavefront (delete/prune)

When resources need to be removed (instance deletion or nodes removed by a
new GraphRevision), levels are processed in descending order (2, 1, 0).
This ensures dependents are cleaned up before their dependencies:

```
Reverse wavefront execution (instance deletion):

  Level 2: DELETE Service      (parallel)
           DELETE Ingress
           Wait for confirmed deletion via watch.

  Level 1: DELETE Deployment
           Wait for confirmed deletion via watch.

  Level 0: DELETE ConfigMap    (parallel)
           DELETE Secret
           Wait for confirmed deletion via watch.

  Remove finalizer. Instance deleted.
```

This ordering prevents dangling references: the Service is deleted before
the Deployment it routes to, and the Deployment is deleted before the
ConfigMap it mounts.

#### Propagation gating within a level ([KREP-006])

When `propagateWhen` is configured, some nodes within a level may be
gated even though their dependencies are ready. The wavefront applies
non-gated nodes and skips gated ones -- it does not block waiting:

```
Level 1: [deploy-us, deploy-eu, deploy-ap]
         propagateWhen: canary.status.healthy == true

  deploy-us ── propagateWhen? ── true  ──► SSA apply ──► WaitReady
  deploy-eu ── propagateWhen? ── false ──► GATED (skip)
  deploy-ap ── propagateWhen? ── false ──► GATED (skip)

  Level result: 1 applied, 2 gated.
  Reconcile completes with status: GATED.
  Next watch event (canary becomes healthy) triggers new reconcile.
  deploy-eu and deploy-ap re-evaluated.
```

**Why gated nodes don't block the wavefront:**
`propagateWhen` is a gate that *prevents* mutation, not one that *delays* the
reconcile loop. If the wavefront blocked waiting for the predicate (which might
depend on external conditions like maintenance windows), the controller
goroutine would be tied up indefinitely. Instead, the reconcile completes with
a "gated" status, and the next watch event triggers re-evaluation.

**Deletion does not respect `propagateWhen`:** When a user deletes an instance,
all resources should be cleaned up promptly. Propagation control is a
deployment-time safety mechanism, not a deletion-time one.

#### Mixed forward and reverse in the same reconcile

When a new GraphRevision adds some nodes and removes others, both
wavefronts run in the same reconcile cycle. The forward wavefront runs
first (create/update), then the reverse wavefront prunes stale resources:

```
Revision 2: [ConfigMap] -> [Deployment, OldSidecar] -> [Service]
Revision 3: [ConfigMap] -> [Deployment, NewSidecar] -> [Service]

Same reconcile:
  Forward (levels 0, 1, 2):
    Level 0: Update ConfigMap
    Level 1: Update Deployment, Create NewSidecar
    Level 2: Update Service

  Reverse (levels 1):
    Level 1: Delete OldSidecar

Result: OldSidecar removed only after NewSidecar is Ready.
```

#### Error isolation

When a node fails, only its dependents are blocked -- independent branches
at the same or deeper levels continue unaffected. A level is considered
complete when all its nodes reach a terminal state (Ready, Error, or
Gated), not only when all are Ready.

```
DAG:  ConfigMap ──► Deployment-A ──► Service-A
      Secret    ──► Deployment-B ──► Service-B

Level 0: [ConfigMap, Secret]
Level 1: [Deployment-A, Deployment-B]
Level 2: [Service-A, Service-B]

Deployment-A fails (SSA conflict). Deployment-B is Ready.

  Level 1 terminal states: [Error, Ready] -- level complete, advance.

  Level 2 dependency check:
    Service-A depends on Deployment-A (Error) --> Blocked (skip)
    Service-B depends on Deployment-B (Ready) --> SSA apply --> Ready

  Result:
    Deployment-A: Error     Service-A: Blocked
    Deployment-B: Ready     Service-B: Ready
```

The instance is not `ACTIVE` (not all nodes Ready), but the healthy branch
is fully reconciled. On the next reconcile, Deployment-A is retried. If it
succeeds, Service-A unblocks and proceeds.

**Dependency check rule:** A node can proceed if and only if *every* node
in its `Dependencies` list is in `Ready` state. If any dependency is
`Error`, the node is `Blocked`. If any dependency is `Gated`, the node
remains `Blocked` (it cannot proceed until the gate opens and the
dependency reaches `Ready`).

**runtime.Synchronize() with partial errors:** When a level completes with
some nodes in Error, the CEL context is refreshed only with status from
Ready nodes. Error nodes retain their last-known status in the context
(or are absent if they were never successfully applied). Templates at the
next level that reference an Error node's status fields will use stale or
zero values -- but those nodes are Blocked anyway, so the stale values are
never applied to the cluster.

**Status rollup:** The instance condition reflects the partial state:

```
Ready = False
  ResourcesReady = False
    Message: "6/8 nodes Ready. 1 Error: [deployment-a].
             1 Blocked: [service-a] (depends on deployment-a).
             deployment-a: SSA conflict (field manager 'helm'
             owns .spec.replicas)."
```

#### Node state machine

```
                         includeWhen = false
              [*] ──────────────────────────────────► Excluded
               │                                        │
               │ includeWhen = true,                    │ includeWhen
               │ deps not ready                         │ becomes true
               │ OR any dep in Error                    │
               ▼                                        │
            Blocked ◄───────────────────────────────────┘
               │
               ├─── deps ready, propagateWhen = false ──► Gated ─┐
               │                                                  │
               │    deps ready, propagateWhen = true              │ propagateWhen
               │    (or not set)                                  │ becomes true
               ▼                                                  │
           Applying ◄─────────────────────────────────────────────┘
               │  ▲
  SSA apply    │  │ retry on
  failed       │  │ next reconcile
               ▼  │
            Error ─┘
               │
               │ SSA success,
               │ readyWhen = false
               ▼
          WaitReady ─── readyWhen = true ──► Ready
```

- A node enters `Blocked` if any dependency is not `Ready` (including `Error`)
- `propagateWhen` gates mutation *start* ([KREP-006])
- `readyWhen` gates mutation *end*
- `Error` on a node blocks only its dependents, not unrelated branches

**State mapping to [KREP-022] managedResources:**

| Synchronizer state | [KREP-022] `managedResources.state` | Current `instance_state.go` |
|---|---|---|
| `Ready` | `READY` | `NodeStateSynced` |
| `Applying` | `IN_PROGRESS` | `NodeStateInProgress` |
| `WaitReady` | `WAITING_FOR_READINESS` | `NodeStateWaitingForReadiness` |
| `Blocked` | `BLOCKED` | *(new -- dependency in Error or Gated)* |
| `Gated` | `GATED` | *(new -- [KREP-006])* |
| `Excluded` | `EXCLUDED` | `NodeStateSkipped` (renamed) |
| `Error` | `ERROR` | `NodeStateError` |
| `ActionDelete` | `DELETING` | `NodeStateDeleting` |
| `ActionAdopt` | `IN_PROGRESS` | *(new -- [KREP-014])* |

### Phase 4: Status update

**Condition hierarchy (extends KREP-001 and [KREP-006]):**

```
Ready
+-- InstanceManaged       - Finalizers and labels set
+-- GraphResolved         - Runtime graph created, resources resolved
+-- ResourcesReady        - All projected resources pass readyWhen
+-- ResourcesPropagated   - All resources at latest GraphRevision (KREP-006)
```

**Instance state mapping:**

| State | Meaning |
|-------|---------|
| `ACTIVE` | All projected resources ready and propagated |
| `IN_PROGRESS` | Forward wavefront executing |
| `GATED` | Wavefront blocked by `propagateWhen` ([KREP-006]) |
| `FAILED` | One or more resources failed after retries |
| `DELETING` | Reverse wavefront executing |
| `ERROR` | Projection failed |

---

## Level-Aware Inventory Management

### The ApplySet limitation

The ApplySet specification ([KEP-3659]) uses a parent object with labels and
annotations to track set membership. This gives us membership tracking, but it
is a flat set with no ordering. When pruning, a Service might be deleted before
its Deployment. Each resource can belong to at most one ApplySet -- you cannot
create one per level.

This proposal introduces a **Level Inventory** that extends ApplySet with
topological ordering: a per-level inventory attached to the instance's ApplySet
parent, enabling ordered creation in forward topological order and ordered
pruning in reverse topological order -- something a single flat ApplySet cannot
express.

### Level Inventory design

We extend the ApplySet parent's annotations with level metadata:

```yaml
metadata:
  labels:
    applyset.kubernetes.io/id: "kro-<hash>"
    applyset.kubernetes.io/tooling: "kro/<version>"
  annotations:
    applyset.kubernetes.io/contains-group-kinds: "Deployment.apps,Service.,ConfigMap."
    applyset.kubernetes.io/additional-namespaces: "ns-a,ns-b"
    # KRO extension: level-ordered inventory
    kro.run/inventory: |
      {"revision":3,"levels":[
        ["ConfigMap..default.app-config","Secret..default.app-secret"],
        ["Deployment.apps.default.app","Service..default.app-svc"],
        ["Ingress.networking.k8s.io.default.app-ingress"]
      ]}
```

The `revision` field in the inventory is a "fully converged" marker -- it is
only updated after the entire forward wavefront completes and all nodes reach
the target revision. Per-node revision is the source of truth (via
`kro.run/revision` labels on managed resources). During a partial failure,
individual node labels show exactly which nodes migrated and which did not;
the inventory `revision` stays at the old value, ensuring the next reconcile
correctly re-targets all stale nodes.

**Member labels (on each managed resource):**

```yaml
labels:
  applyset.kubernetes.io/part-of: "kro-<hash>"
  kro.run/instance: "<instance-name>"
  kro.run/resource-id: "deployment"
  kro.run/level: "1"
  kro.run/revision: "3"
annotations:
  kro.run/foreach-bindings: '{"region":"eu-west-1"}'  # forEach nodes only
```

### Ordered prune

```mermaid
flowchart TB
    INV["Read inventory"] --> PROJ["Compute projected DAG"]
    PROJ --> DIFF["For each level highest->lowest:\nfind stale resources"]
    DIFF --> DEL["Delete resources\n(parallel within level)"]
    DEL --> WAIT["Wait for confirmed\ndeletion via watch"]
    WAIT --> UPD["Update inventory"]
    UPD --> NEXT{"More\nlevels?"}
    NEXT -- yes --> DIFF
    NEXT -- no --> DONE["Update ApplySet\nannotations"]

    style INV fill:#FAEEDA,stroke:#854F0B,color:#633806
    style DEL fill:#FCEBEB,stroke:#A32D2D,color:#791F1F
    style DONE fill:#EAF3DE,stroke:#3B6D11,color:#27500A
```

### Annotation size analysis

**Size formula:**

```
inventory_bytes ~ 30 + sum(entries_per_level * (avg_gknn_length + 3) + 4)
```

A typical GKNN entry like `"Deployment.apps.my-namespace.my-app-eu-west-1"` is
~50 characters.

**Concrete estimates:**

| Scenario | Total entries | Size | % of 256KB |
|----------|-------------|------|------------|
| Simple web app (5 nodes, 3 levels) | 5 | ~430B | 0.2% |
| Microservice mesh (20 nodes, 5 levels) | 20 | ~1.5KB | 0.6% |
| Multi-region (3 forEach x 10 regions) | 30 | ~2.3KB | 0.9% |
| Large platform (10 + 5 forEach x 50) | 260 | ~18KB | 7% |
| Extreme (5 + 10 forEach x 200) | 2005 | ~140KB | 55% |
| Pathological (20 forEach x 500) | 10000 | ~700KB | **EXCEEDS** |

**Mitigation:**

If the annotation budget becomes a concern, the inventory can be stored
elsewhere instead:

1. **ConfigMap**: A dedicated ConfigMap per instance holds the full inventory
   (up to 1MB). The annotation stores only a pointer:
   `{"overflow":"<configmap-name>"}`.
2. **Instance status**: The inventory is written into the instance's
   `.status.inventory` field, keeping all instance state co-located and avoiding
   a separate object. Status subresource updates don't conflict with spec
   edits.

---

## Revision Migration: n-1 to n

This section consolidates the revision-specific behaviors described in
[Phase 2 (Diff)](#phase-2-diff) and [Phase 3 (Execute wavefront)](#phase-3-execute-wavefront)
into a complete picture of how an instance transitions from one GraphRevision to
the next.

### Trigger

A new GraphRevision is created by the RGD controller whenever
`ResourceGraphDefinition.spec` changes. The GraphRevision object is
immutable (`spec` carries an XValidation rule: `self == oldSelf`) and
contains a full snapshot of the RGD spec:

```yaml
apiVersion: internal.kro.run/v1alpha1
kind: GraphRevision
metadata:
  name: webapp-r00003
  labels:
    kro.run/graph-revision-hash: "sha256-abc123"
    internal.kro.run/resource-graph-definition-name: webapp
spec:
  revision: 3
  snapshot:
    name: webapp
    generation: 7
    spec: { ... }   # full copy of the RGD spec at generation 7
```

The in-memory revision registry (`pkg/graph/revisions/registry.go`)
tracks this object through states `Pending -> Active` (or `Failed`). The
instance controller's `GraphRevisionResolver.GetLatestRevision()` returns
the highest-numbered active entry.

### Detection

At the start of each reconcile, Phase 1 (Project) resolves the latest
active GraphRevision and records it as `ProjectedDAG.Revision` -- the
*target* revision. Each managed resource's *current* revision is read
individually from its `kro.run/revision` label during Phase 2 (Diff).

There is no instance-level "previous revision" field. Per-node tracking
is the source of truth because during a partial migration, different
nodes are at different revisions. The `revision` field in the
`kro.run/inventory` annotation is a "fully converged" marker -- only
updated after all nodes reach the target.

### Execution order

The diff and wavefront phases handle revision migration using the same
mechanics described above: nodes with a stale `kro.run/revision` label
are classified as `ActionUpdate` (see [Phase 2](#phase-2-diff)), and the
forward wavefront applies them level-by-level (see [Phase 3](#phase-3-execute-wavefront)).

```
Revision 2 (current):  [ConfigMap] -> [Deployment] -> [Service]
Revision 3 (target):   [ConfigMap] -> [Deployment] -> [Service, Ingress]
                                                        ^^^^^^^ new node

Forward wavefront:
  Level 0: Update ConfigMap (kro.run/revision: 2 -> 3)
           Wait readyWhen -> Ready
  Level 1: Update Deployment (kro.run/revision: 2 -> 3)
           Wait readyWhen -> Ready
  Level 2: Update Service (kro.run/revision: 2 -> 3)
           Create Ingress (kro.run/revision: 3)
           Wait readyWhen -> Ready

Inventory updated: {"revision": 3, ...}
```

If a node fails at level 1, its dependents are blocked but independent
branches continue (see [Error isolation](#error-isolation)). For example,
if Deployment fails but an independent Service at level 2 has all its
dependencies Ready, that Service is still updated to revision 3. The
inventory `revision` stays at `2` because not all nodes reached the
target. On the next reconcile, the diff sees nodes already at revision 3
(skip or readyWhen-check only) and retries only the failed node and its
blocked dependents.

### Topology changes between revisions

When the new GraphRevision changes the dependency structure:

| Change | Handling |
|--------|----------|
| **Node added** | Appears in projected DAG at its computed level. `ActionCreate`. Created in forward order. |
| **Node removed** | In inventory but absent from projected DAG. `ActionDelete`. Deleted in reverse order after all forward actions complete. |
| **Node moves to a different level** | `kro.run/level` label updated during SSA apply. Inventory rewritten with new level assignments. The forward wavefront processes the node at its new level. |
| **Dependency edge added** | Node waits for an additional dependency. If it creates a cycle, the GraphRevision's `GraphVerified` condition is `False` and the revision enters `Failed` state -- never served to instances. |
| **Dependency edge removed** | Node may move to a lower level. Processed at its new level. |

**Example -- node changes level:**

```
Revision 2:  L0:[ConfigMap, Secret]  L1:[Deployment]  L2:[Service]
Revision 3:  L0:[ConfigMap]          L1:[Secret, Deployment]  L2:[Service]
             (Secret now depends on ConfigMap)

Forward wavefront for revision 3:
  Level 0: Update ConfigMap (revision 2->3)
  Level 1: Update Secret (revision 2->3, kro.run/level: 0->1)
           Update Deployment (revision 2->3)
  Level 2: Update Service (revision 2->3)
```

### The kro.run/revision label

Each managed resource carries `kro.run/revision` set during SSA apply.
It serves three purposes:

1. **Diff optimization.** Skip re-applying resources already at the
   target revision when templates have not changed.
2. **Progress visibility.** `kubectl get <resource> -L kro.run/revision`
   shows at a glance which resources have migrated and which have not.
3. **Debugging.** After a partial failure, the label reveals the exact
   frontier of the migration across all managed resources.

The label is written atomically as part of the SSA apply -- it is never
updated separately from the resource template.

### Edge cases

**Revision issued mid-reconcile:**

If a new GraphRevision becomes active while the wavefront is executing
revision *n*, the current reconcile completes against revision *n*. The
newer revision *n+1* is picked up on the next reconcile cycle. This is
safe because `resolveCompiledGraph()` is called once at the start of
each reconcile and the result is held for the duration. The inventory and
labels are updated to revision *n*, so the next reconcile correctly
diffs *n* against *n+1*.

**Failed revision:**

If the latest GraphRevision enters `Failed` state (compilation error),
`resolveCompiledGraph()` returns a terminal error and the reconcile
aborts. The instance retains its current per-node revision labels. There
is no fallback to the previous active revision -- a failed revision means
the RGD spec is invalid, and silently falling back could mask the
problem. The `GraphVerified` condition on the GraphRevision surfaces the
error; the instance's `GraphResolved` condition transitions to `False`
with a message referencing the failed revision number. Operators must fix
the RGD, which triggers a new GraphRevision.

**Partial migration + topology reorder:**

If revision *n* completes levels 0 and 1 but fails at level 2, and the
operator issues revision *n+1* which reorders levels, the next reconcile
starts fresh against *n+1*. The diff reads each node's `kro.run/revision`
label individually: all are < *n+1*, so all are marked `ActionUpdate` and
processed in the new level order. Partially-applied level-2 resources
from revision *n* are either updated (if still in the projected DAG) or
pruned (if removed).

**New revision arrives during incomplete migration:**

The synchronizer must handle the case where a new GraphRevision *n+1*
becomes active while the wavefront is still migrating from *n-1* to *n*.
Two strategies are possible:

**Strategy A: Skip to latest (default).** The current reconcile completes
its in-progress level against revision *n*, then the next reconcile
targets *n+1* directly. Nodes already at *n* are updated again to *n+1*.
Nodes still at *n-1* jump directly to *n+1*, skipping *n* entirely.

```
Before:  L0:[rev 3]  L1:[rev 2]  L2:[rev 2]    (migrating 2->3, L0 done)
Rev 4 arrives.
Next reconcile targets rev 4:
  L0: rev 3 < 4 --> ActionUpdate (3->4)
  L1: rev 2 < 4 --> ActionUpdate (2->4, skips 3)
  L2: rev 2 < 4 --> ActionUpdate (2->4, skips 3)
```

This is correct because each GraphRevision is a complete snapshot -- there
is no incremental delta that requires processing revision 3 before 4.
Resources jump directly to the latest desired state. This is the simplest
model and avoids serializing migrations that may never complete.

**Strategy B: Complete current before advancing.** The wavefront finishes
the full *n-1* to *n* migration before starting *n* to *n+1*. This
guarantees that every revision is fully applied and validated via
`readyWhen` before moving on.

```
Before:  L0:[rev 3]  L1:[rev 2]  L2:[rev 2]    (migrating 2->3, L0 done)
Rev 4 arrives, but wavefront continues targeting rev 3:
  L1: rev 2 < 3 --> ActionUpdate (2->3)
  L2: rev 2 < 3 --> ActionUpdate (2->3)
  All at rev 3. Inventory updated to rev 3.
Next reconcile targets rev 4:
  L0: rev 3 < 4 --> ActionUpdate (3->4)
  L1: rev 3 < 4 --> ActionUpdate (3->4)
  L2: rev 3 < 4 --> ActionUpdate (3->4)
```

This is safer when `readyWhen` predicates act as health checks -- it
ensures revision *n* is fully healthy before applying *n+1*. But it adds
latency when revisions arrive in rapid succession (each must fully roll
out before the next starts).

**Recommended default: Strategy A (skip to latest).** This matches
Kubernetes controller conventions -- a controller always reconciles
toward the latest desired state. Strategy B can be offered as an opt-in
`revisionPolicy: Serialized` for users who need per-revision health
validation. Under Strategy A, the `resolveCompiledGraph()` call at the
start of each reconcile always returns the latest active revision; under
Strategy B, it returns the in-progress target revision until migration
completes.

Implementation note: Strategy B requires tracking the "in-progress
target revision" in the inventory or instance status so it survives
controller restarts. Strategy A needs no additional state -- it always
reads the latest from the registry.

### Consistency invariants and recovery

The synchronizer maintains state in three places: managed resource labels,
the inventory annotation, and the in-memory projected DAG. These can
diverge after crashes or partial failures. This section defines the
invariants, how they can break, and how the controller recovers.

**Invariant 1: Labels are the source of truth for per-node state.**

The `kro.run/revision` and `kro.run/level` labels on each managed resource
are written atomically as part of the SSA apply. If the controller crashes
after applying some nodes but before updating the inventory, labels and
inventory diverge. Recovery: the next reconcile reads labels from live
resources during Phase 2 (Diff). Nodes whose labels show the target
revision are skipped; nodes with stale labels are re-applied. The
inventory is rewritten at the end of a successful reconcile.

The inventory is an optimization (avoids listing all managed resources on
every reconcile), not the source of truth. If the inventory is lost or
stale, the controller can rebuild it from `applyset.kubernetes.io/part-of`
labels via a cluster scan.

**Invariant 2: The inventory `revision` is always <= the minimum per-node revision.**

The inventory `revision` is only bumped after all nodes reach the target.
If the controller crashes mid-migration, the inventory `revision` stays
at the old value. This is safe: the next reconcile re-reads per-node
labels and only re-applies nodes that are behind the target. Nodes already
at the target are idempotently skipped (SSA is a no-op for matching
templates).

**Invariant 3: Level assignments in the inventory match the current GraphRevision.**

When a new revision changes the topology, level assignments change. The
inventory is rewritten with new level assignments after the forward
wavefront completes. If the controller crashes between updating a node's
`kro.run/level` label and rewriting the inventory, the inventory has stale
level data. Recovery: the next reconcile computes fresh level assignments
from the current GraphRevision (Phase 1) and ignores the inventory's level
data during the diff. The inventory is rewritten at the end.

**Invariant 4: Re-projection does not delete resources based on stale CEL context.**

During error isolation, an Error node's status fields may be stale or
absent in the CEL context. If another node's `includeWhen` predicate
references the Error node's status, the predicate may evaluate to false,
which would exclude the node from the projected DAG and classify it as
`ActionDelete`. This is unsafe — the node should be Blocked, not deleted.

Mitigation: during re-projection, nodes whose dependencies include an
Error node are exempt from `includeWhen` re-evaluation. Their inclusion
state is frozen at the last known value until all dependencies are Ready.
This prevents the CEL context's stale values from causing unintended
deletions.

**Invariant 5: Forward-before-reverse within the same level during revision transitions.**

When a revision transition adds and removes nodes at the same level, the
forward wavefront creates the new node before the reverse wavefront
deletes the old one. This prevents a gap where neither exists. If the new
and old nodes share the same Kubernetes resource name (the resource was
replaced), the forward SSA apply handles this naturally — SSA updates the
existing resource in-place, and no reverse delete is needed (the old
resource *is* the new resource, just updated).

If they have different names, the forward creates the new resource first,
then the reverse deletes the old one. If the forward create fails, the
reverse delete does not proceed for that level — the old resource is kept
until the new one is successfully created.

---

## Edge Cases and Risks

1. **Inventory annotation race with spec updates** (HIGH)
   - *Cause:* Inventory PATCH on main resource conflicts with user spec edits.
     Today's controller only writes `/status`.
   - *Mitigation:* Use SSA with dedicated field manager `kro-inventory`. Or
     store inventory in a separate ConfigMap.

2. **Stale informer cache during re-projection** (MEDIUM)
   - *Cause:* `runtime.Synchronize()` reads from cache that hasn't received the
     watch event yet.
   - *Mitigation:* Use SSA response objects directly to update the runtime
     context, bypassing informer cache.

3. **Finalizer-blocked reverse prune** (MEDIUM)
   - *Cause:* DELETE sets `deletionTimestamp` but resource lingers until an
     external finalizer completes.
   - *Mitigation:* Multi-reconcile deletion: issue DELETE, mark DELETING,
     return. Next reconcile confirms deletion via watch.

4. **forEach identity collision** (MEDIUM)
   - *Cause:* Two forEach bindings produce the same Kubernetes resource name.
   - *Mitigation:* Validate expanded names for uniqueness during the projection
     phase before SSA applies.

5. **Revision transition with immutable field changes** (LOW severity / HIGH blast)
   - *Cause:* RGD changes an immutable field (e.g., Deployment selector) and SSA
     fails permanently.
   - *Mitigation:* Diff structural DAGs at revision creation. Flag known
     immutable field changes as warnings.

6. **Controller crash mid-inventory-write** (LOW)
   - *Cause:* Stale inventory listing deleted resources or wrong level
     assignments.
   - *Mitigation:* Labels are the source of truth, not the inventory (see
     [Invariants 1-3](#consistency-invariants-and-recovery)). On recovery,
     rebuild from `applyset.kubernetes.io/part-of` labels via cluster scan.
     SSA applies are idempotent.

8. **Re-projection deleting resources due to stale Error node status** (MEDIUM)
   - *Cause:* An Error node's status is stale/absent in the CEL context. Another
     node's `includeWhen` references it, evaluates to false, and the node is
     excluded from the projected DAG — triggering an unintended delete.
   - *Mitigation:* Freeze `includeWhen` evaluation for nodes with Error
     dependencies (see [Invariant 4](#consistency-invariants-and-recovery)).

9. **propagateWhen never becoming true** (LOW severity / MEDIUM impact)
   - *Cause:* External resource stuck, producing permanent GATED state
     ([KREP-006]).
   - *Mitigation:* Optional `propagationTimeout`. Condition message:
     "propagateWhen false for 4h on node X".

---

## Observability

### Debugging guide: common failure patterns

| Symptom | What to check | Likely cause | Remediation |
|---------|---------------|--------------|-------------|
| Instance stuck `IN_PROGRESS` | `nodes_error` gauge, `NodeError` events | SSA apply failing repeatedly | Check instance events. Common causes: field manager conflict, webhook rejection, RBAC missing. Fix the conflict or update the RGD template. |
| Instance stuck `GATED` | `nodes_gated` gauge, `NodeGatedTimeout` event | `propagateWhen` predicate never becomes true | Inspect the predicate in the event message. Check the upstream resource it references. Update the RGD to adjust `propagateWhen`. |
| Reconcile latency spike | `reconcile_duration_seconds` by `phase`, `level_duration_seconds` | Slow API server, expensive CEL, or large forEach | If `project` phase is slow: check `reprojection_iterations`. If `execute` is slow: check `node_duration_seconds` for outliers. |
| Resources deleted out of order | `level_duration_seconds` with `direction=reverse` | Stale or missing inventory | If inventory was lost (crash), the controller rebuilds from `part-of` labels on next reconcile. Verify labels are present. |
| forEach creates too many resources | `inventory_entries` gauge | forEach evaluates to unexpectedly large set | Check forEach source data. Add `maxItems` validation on the forEach input in the RGD schema. |
| `ReprojectionCapReached` warning | Structured logs with `reprojection cap` | Circular `includeWhen` predicates | Restructure the RGD. Two nodes should not conditionally include each other based on the other's readiness. |
| Inventory overflow | `inventory_size_bytes` with `storage=configmap` | Large forEach expansion | Expected for large deployments. Monitor ConfigMap size. If approaching 1MB, reduce forEach cardinality. |
| Revision rollout not progressing | `nodes_at_revision` gauge, `nodes_gated` | `propagateWhen` gating rollout (by design) | Check if canary/first-batch resources are healthy. If so, the `propagateWhen` predicate may reference the wrong field. |

### Conditions

The condition hierarchy (Phase 4) surfaces failure information directly in
`kubectl get` output:

```
Ready = False
  ResourcesReady = False
    Message: "2/8 nodes in ERROR state: [deployment, service].
             deployment: SSA conflict on apps/v1 Deployment default/app
             (field manager 'helm' owns .spec.replicas).
             service: connection refused to API server (retries: 3)."
```

```
Ready = False
  ResourcesPropagated = False
    Message: "3 nodes GATED for 4h12m: [deploy-eu, deploy-ap, deploy-us].
             propagateWhen: status.canary.healthy == true
             (canary deploy-us-canary: status.canary.healthy = false)"
```

Each condition message includes:
- **Which nodes** are in a non-terminal state
- **Why** they are stuck (the specific error or unsatisfied predicate)
- **How long** they have been in that state

### Kubernetes Events

| Event | Type | Reason | When |
|-------|------|--------|------|
| Normal | `LevelComplete` | A level finishes execution. Message includes level number, node count, and duration. |
| Normal | `ReconcileComplete` | Full reconcile cycle completes. Message includes total duration and node counts by final state. |
| Normal | `RevisionTransition` | Instance begins reconciling against a new GraphRevision (see [Revision Migration](#revision-migration-n-1-to-n)). Message includes old and new revision numbers and topology change summary (nodes added/removed/moved). |
| Warning | `NodeError` | A node's SSA apply fails. Message includes resource ID, error, and retry count. |
| Warning | `NodeGatedTimeout` | Node in GATED state longer than `propagationTimeout`. Message includes node ID and unsatisfied `propagateWhen` expression. |
| Warning | `ReprojectionCapReached` | Phase 1 hit fixed-point iteration cap. Message includes oscillating `includeWhen` predicates. |
| Warning | `InventoryOverflow` | Inventory exceeded annotation budget. Message includes entry count and byte size. |
| Warning | `ImmutableFieldConflict` | Diff detected change to a known immutable field. Message includes field path and resource. |

### Metrics

The existing instance controller already exposes reconcile-level metrics
(`instance_reconcile_duration_seconds`, `instance_reconcile_total`,
`instance_reconcile_errors_total`, `instance_state_transitions_total`,
`instance_graph_resolution_*`). This proposal changes one existing metric
and adds new ones for the wavefront, inventory, and revision subsystems.

**Changed metric:**

| Metric | Change | Reason |
|--------|--------|--------|
| `instance_reconcile_duration_seconds` | Add `phase` label (`project`, `diff`, `execute`, `status`) | Current metric only records total duration. Per-phase breakdown identifies whether slowness is in CEL evaluation, API server calls, or status writes. |

**New metrics** (all use the `kro_instance_` prefix):

| Metric | Type | Labels | Purpose |
|--------|------|--------|---------|
| `level_duration_seconds` | Histogram | `gvr`, `level`, `direction` | Per-level execution time. Outlier levels reveal bottleneck resources. |
| `level_concurrency` | Histogram | `gvr`, `level` | Actual parallelism achieved per level. Consistently 1 means the DAG has no parallelism at that level. |
| `node_action_total` | Counter | `gvr`, `action` | Action distribution (create, update, delete, adopt, orphan, gate). |
| `node_duration_seconds` | Histogram | `gvr`, `resource_id`, `action` | Per-node SSA apply duration. Identifies slow resources. |
| `nodes_gated` | Gauge | `gvr`, `instance` | Nodes currently in GATED state. Non-zero for hours signals a stuck gate. |
| `nodes_error` | Gauge | `gvr`, `instance` | Nodes currently in ERROR state. |
| `reprojection_iterations` | Histogram | `gvr` | Fixed-point iterations in Phase 1. Near-cap values indicate complex conditional chains. |
| `inventory_size_bytes` | Gauge | `gvr`, `instance`, `storage` | Annotation budget consumption. Alert when approaching 50% of 256KB. |
| `inventory_entries` | Gauge | `gvr`, `instance` | Total entries in inventory. |
| `revision_current` | Gauge | `gvr`, `instance` | GraphRevision being reconciled. Track rollout progress across a fleet. |
| `revision_transition_duration_seconds` | Histogram | `gvr` | End-to-end migration time (first reconcile to all nodes Ready at new revision). |
| `nodes_at_revision` | Gauge | `gvr`, `instance`, `revision` | Resources per revision. Two non-zero values during migration; converges to one when complete. |

### Structured Logging

```
level=info  msg="level complete"      instance=my-app rgd=webapp level=1 direction=forward nodes=3 duration=2.4s
level=info  msg="node applied"        instance=my-app rgd=webapp node=deployment action=update level=1 duration=0.8s revision=3
level=warn  msg="node error"          instance=my-app rgd=webapp node=service action=create level=1 error="conflict" retries=2
level=warn  msg="node gated"          instance=my-app rgd=webapp node=deploy-eu level=1 gated_since=2026-03-27T16:00:00Z predicate="status.canary.healthy == true"
level=info  msg="reprojection"        instance=my-app rgd=webapp iteration=2 changed_nodes=[servicemonitor] trigger="includeWhen became true"
level=error msg="reprojection cap"    instance=my-app rgd=webapp iterations=5 oscillating=[node-a,node-b]
```

---

## Relationship to Other Proposals

### KREP-022: managedResources

[KREP-022] introduces `managedResources` in instance status. The wavefront
synchronizer produces all the data it needs. Key design decisions from
[PR #1161](https://github.com/kubernetes-sigs/kro/pull/1161) review:

- **`graphRevision` must be per-node**, not per-instance. During revision
  transitions, different nodes are at different revisions.
- **External nodes excluded** from managedResources.
- **Add `level` field** to each entry (new -- this proposal recommends it).
- **State naming**: map internal `NodeStateSynced` -> `READY` in status.

### KREP-014: Resource Lifecycles

[KREP-014] introduces resource policies for adoption and orphaning. The
[PR #1091](https://github.com/kubernetes-sigs/kro/pull/1091) review converges
toward separating creation and deletion policies:

```go
type ResourcePolicies struct {
    OnCreate string // "Create" (default) | "Adopt" | "Error"
    OnDelete string // "Delete" (default) | "Orphan" | "Error"
}
```

This fits the synchronizer naturally: forward wavefront reads `OnCreate`,
reverse wavefront reads `OnDelete`. They never interfere.

---

## Implementation Plan

### Mapping current -> proposed

```mermaid
flowchart LR
    subgraph Current["Current controller"]
        C1["Controller.Reconcile\n(sequential walk)"]
        C2["InstanceState\n{State, ResourceStates}"]
        C3["runtime.Synchronize\n(per-node)"]
        C4["graph.Graph\n+ flat TopologicalSort"]
        C5["ApplySet labels\n(flat membership)"]
    end

    subgraph Proposed["Proposed controller"]
        P1["Controller.Reconcile\n4-phase pipeline"]
        P2["InstanceState\n+ Level, Revision"]
        P3["runtime.Synchronize\n(per-level, SSA resp)"]
        P4["TopologicalSortLevels\nKahn's algorithm"]
        P5["ApplySet labels +\nkro.run/inventory"]
        P6["Wavefront (new)"]
        P7["LevelInventory (new)"]
        P8["ProjectedDAG (new)"]
    end

    C1 -- "refactor" --> P1
    C2 -- "extend" --> P2
    C3 -- "optimize" --> P3
    C4 -- "refactor" --> P4
    C5 -- "extend" --> P5

    style C1 fill:#EEEDFE,stroke:#534AB7,color:#3C3489
    style C2 fill:#EEEDFE,stroke:#534AB7,color:#3C3489
    style C3 fill:#EEEDFE,stroke:#534AB7,color:#3C3489
    style C4 fill:#EEEDFE,stroke:#534AB7,color:#3C3489
    style C5 fill:#EEEDFE,stroke:#534AB7,color:#3C3489
    style P1 fill:#E1F5EE,stroke:#0F6E56,color:#085041
    style P2 fill:#E1F5EE,stroke:#0F6E56,color:#085041
    style P3 fill:#E1F5EE,stroke:#0F6E56,color:#085041
    style P4 fill:#E1F5EE,stroke:#0F6E56,color:#085041
    style P5 fill:#E1F5EE,stroke:#0F6E56,color:#085041
    style P6 fill:#FAEEDA,stroke:#854F0B,color:#633806
    style P7 fill:#FAEEDA,stroke:#854F0B,color:#633806
    style P8 fill:#FAEEDA,stroke:#854F0B,color:#633806
```

### What stays

- `Controller.Reconcile(ctx, req) error` -- same entry point.
- `graph.Graph` -- compiled RGD with CEL programs
  ([PR #1014](https://github.com/kubernetes-sigs/kro/pull/1014)). Proposal adds
  levels but doesn't change the type.
- `runtime.Synchronize()` -- called per-level instead of per-node. Should use
  SSA response objects to avoid stale cache risk.
- All ApplySet labels -- strictly additive.
- Node state constants from `api/v1alpha1/instance_state.go` -- kept, with new
  states: `GATED` ([KREP-006]), `EXCLUDED`/`INCLUDED` ([KREP-022]).

### What changes

- `ResourceState` gains `Level int`, `Revision int64`, `Action NodeAction`.
  Backward compatible (zero values = current behavior).
- `ReconcileConfig` gains `MaxConcurrency int` (default: 10).
- Status update gains `managedResources` builder ([KREP-022]) and inventory
  annotation writer.

### New components

| Component | Purpose |
|-----------|---------|
| `Wavefront` | Level-aware parallel executor with [KREP-006] + [KREP-014] gates |
| `LevelInventory` | Serializer for `kro.run/inventory` with ConfigMap overflow |
| `ProjectedDAG` | Explicit runtime DAG with `includeWhen`/`forEach` evaluated |
| `ReconcilePlan` | Typed diff output grouping actions by level |

### Migration path

| Phase | Scope |
|-------|-------|
| 1 | Add `TopologicalSortLevels()` ([KREP-003]). Sequential execution. Add `kro.run/level` labels. |
| 2 | Add `LevelInventory` writer. Write `kro.run/inventory`. Add `kro.run/revision` labels. |
| 3 | Replace sequential walk with wavefront. Add `managedResources` ([KREP-022]). |
| 4 | Add `propagateWhen` ([KREP-006]) and `onCreate`/`onDelete` ([KREP-014]). |

---

## Open Questions

1. **Serialized vs skip-to-latest revision policy** -- The proposal recommends
   skip-to-latest as the default (see [Revision Migration, "New revision arrives
   during incomplete migration"](#edge-cases-1)). Should `revisionPolicy:
   Serialized` be available from the start, or deferred? Serialized requires
   persisting the in-progress target revision.
2. **Debounce on external watches** -- Configurable 1-2s window. Relevant to
   [Phase 1 (Project)](#phase-1-project) re-projection triggered by external
   resource changes. Without debounce, rapid external changes cause unnecessary
   re-projections.
3. **forEach rollout** -- Subsumed by [KREP-006] `propagateWhen` primitives.
   The [propagation gating example](#propagation-gating-within-a-level-krep-006)
   shows how this works in practice.
4. **Inventory storage backend** -- Annotation-first with ConfigMap overflow vs
   always-ConfigMap. See [Annotation size analysis](#annotation-size-analysis)
   for budget estimates. Always-ConfigMap avoids the race in
   [risk #1](#edge-cases-and-risks) but adds an extra object per instance.
5. **Revision fallback policy** -- A `Failed` latest revision blocks all
   instances with no automatic fallback (see [Revision Migration, "Failed
   revision"](#edge-cases-1)). Should we support opt-in
   `revisionPolicy: FallbackToPrevious`? This trades "fail loudly" for
   availability. Defer.

---

## Testing Strategy

### Unit tests

- Kahn's algorithm level computation
- Diff algorithm (create, update, delete, adopt, orphan)
- Inventory serialization and ConfigMap overflow
- Propagation gate evaluation ([KREP-006])
- forEach set reconciliation and identity collision detection
- Annotation size estimation

### Integration tests

- Multi-level wavefront execution
- Partial failure and recovery
- Revision migration: full n-1 to n transition with level-by-level label bumping
- Revision migration: topology change (node added, node removed, node moves level)
- Revision migration: partial failure at level K, recovery on next reconcile
- Revision migration: new revision issued mid-reconcile (ignored until next cycle)
- Revision migration: failed revision blocks instances, fixed RGD unblocks
- forEach expansion/contraction with ordered pruning
- `includeWhen` re-projection
- `propagateWhen` gating ([KREP-006])
- `onCreate`/`onDelete` flows ([KREP-014])
- `managedResources` population ([KREP-022])
- Controller restart with partial inventory

### Edge cases

- Single-level graphs (all independent)
- Linear chains (no parallelism)
- 50+ resources across 10+ levels
- forEach producing 0 elements
- All nodes gated
- Revision reordering levels
- Revision n-1 partially applied, revision n+1 issued (skip n)
- Revision changes immutable field on a node (cross-ref risk #6)
- Inventory exceeding 50% budget
- Resource lifecycle policy transitions mid-reconcile ([KREP-014])

<!-- Reference-style links -->
[KREP-003]: https://github.com/bschaatsbergen/kro/blob/1260308a4475ea622f774e3d3ff0f4ee13bca0b5/docs/design/proposals/krep-003-level-based-topological-sorting.md
[KREP-006]: https://github.com/ellistarn/kro/blob/ba49042d4054b58ca44796fe36f247ca4e92d681/docs/design/proposals/propagation-control.md
[KREP-014]: https://github.com/kubernetes-sigs/kro/pull/1091
[KREP-022]: https://github.com/kubernetes-sigs/kro/pull/1161
[KEP-3659]: https://github.com/kubernetes/enhancements/blob/master/keps/sig-cli/3659-kubectl-apply-prune/README.md
