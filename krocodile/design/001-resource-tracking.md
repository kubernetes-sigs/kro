# Resource Tracking

The Graph controller persists an authoritative list of cluster
resources it has applied — node IDs, GVK/name/namespace, and post-apply
UIDs — directly in `status.managedResources`. The list survives across
reconciles. It drives prune decisions when the spec changes, and it
drives delete on Graph removal. Identities come from successful Apply
responses; nothing is inferred from the spec at delete time.

This document describes the data model, the diff math, the
write-ahead/commit lifecycle, and the boundaries the design
deliberately stops at.

## Why

Without tracking, two scenarios leak:

1. **Spec edit then delete.** A user renames a template, then deletes
   the Graph. The reconciler re-resolves the current spec at delete
   time, sees only the new name, deletes that, and drops the
   finalizer. The originally-named resource lingers in the cluster
   with no owner.

2. **Spec change in steady state.** A node is dropped from the spec,
   a `forEach` shrinks, or an `includeWhen` flips to `false`. The
   controller stops re-applying — but never deletes — the resources
   the previous reconcile created. Resources accumulate as
   undocumented children of past graph versions.

The fix is the same in both cases: remember what was applied. Compare
the remembered set against the latest applied set on every reconcile;
delete anything the latest set no longer covers; on Graph deletion,
walk the remembered set in reverse-apply order.

## Data model

```go
type GraphStatus struct {
    Conditions       Conditions          `json:"conditions,omitempty"`
    ManagedResources []ManagedResource   `json:"managedResources,omitempty"`
}

type ManagedResource struct {
    NodeID     string `json:"nodeID"`               // groups + orders forEach instances
    APIVersion string `json:"apiVersion"`           // "apps/v1", "v1", ...
    Kind       string `json:"kind"`                 // "Deployment", "ConfigMap", ...
    Namespace  string `json:"namespace,omitempty"`  // empty for cluster-scoped
    Name       string `json:"name"`
    UID        string `json:"uid,omitempty"`        // SSA-response UID; absent on union-state entries
}
```

Five identifier fields plus UID. There is no synthetic `TopoIndex` —
**the slice order is the order**. The reconciler appends entries in
topological apply order; reverse iteration on delete or prune yields
reverse-apply order. Multiple `forEach` instances of one node appear
consecutively (same `NodeID`, distinct identities).

The UID field is populated from the server's SSA response. It is used
as a delete precondition (`metav1.Preconditions{UID: ...}`) so that a
race where the resource was deleted-and-recreated by some other actor
doesn't lead us to remove the impostor.

## Pipeline

```
                    ┌─────────────────────────┐
   reconcile cycle  │  reconcileGraph(g)      │
   begins          ─┤                         │
                    │  previous := g.Status.  │
                    │      ManagedResources   │
                    └────────────┬────────────┘
                                 │
                                 ▼
                ┌─────────────────────────────┐
                │   Executor.Apply(rt, w)     │
                │                             │
                │  walks nodes:               │
                │    - IsIgnored              │
                │    - Resolve                │
                │    - register watch         │
                │    - SSA apply              │
                │    - CheckReadiness         │
                │                             │
                │  returns ApplyResult {      │
                │     Applied:    [...]       │
                │     Unresolved: [nodeIDs]   │
                │  }, err                     │
                └────────────┬────────────────┘
                             │
                             ▼
        ┌────────────────────────────────────────────┐
        │  diffManagedResources(previous, result)    │
        │                                            │
        │  returns:                                  │
        │    newSet         = Applied ∪ preserved    │
        │    pruneCandidates = previous \ newSet     │
        └────────────┬───────────────────────────────┘
                     │
                     ▼
              ┌──────┴──────┐
              │             │
        err == nil       err != nil
              │             │
              ▼             ▼
        ┌──────────┐  ┌─────────────────┐
        │  Prune   │  │   No prune.     │
        │          │  │   status =      │
        │  Delete  │  │     previous ∪  │
        │  prune-  │  │     Applied     │
        │  Cand.   │  │                 │
        │          │  │   Retry on next │
        │  status= │  │   reconcile.    │
        │  newSet  │  │                 │
        └──────────┘  └─────────────────┘
```

## The diff

`diffManagedResources(previous, ApplyResult)` produces two slices:

- `newSet`: what `status.managedResources` should be after a fully
  successful Apply.
- `pruneCandidates`: previous entries no longer wanted, eligible for
  deletion.

The rule for each `previous` entry, given `Applied` and `Unresolved`
from this cycle:

| previous entry's status                                             | classification     | reason                                                                                    |
| ------------------------------------------------------------------- | ------------------ | ----------------------------------------------------------------------------------------- |
| identity (GVKNN) appears in Applied                                 | covered by newSet  | resource is being re-applied this cycle                                                   |
| identity NOT in Applied AND NodeID in Unresolved                    | preserved (newSet) | we couldn't enumerate identities for this NodeID; keep last-known cautiously              |
| identity NOT in Applied AND NodeID NOT in Unresolved                | prune candidate    | node dropped, forEach shrunk, rename, or includeWhen flipped to false                     |

In code:

```go
for _, prev := range previous {
    if _, alreadyApplied := applied[keyOf(prev)]; alreadyApplied { continue }
    if _, isUnresolved   := unresolved[prev.NodeID];   isUnresolved   {
        newSet = append(newSet, prev)
        continue
    }
    pruneCandidates = append(pruneCandidates, prev)
}
```

Note: there is no schema-level distinction between "intentionally
skipped" (includeWhen=false, ignored via contagious propagation) and
"renamed away." Both produce no Applied entry for the previous
identity and don't appear in Unresolved. Both prune. This is by
design: the Reconciler's invariant is *if you didn't apply it this
cycle and you weren't blocked from trying, you don't want it*.

## The two ApplyResult fates

The Executor's contract distinguishes three node fates:

1. **Applied** — node resolved, applied, observed. Its identities
   land in `ApplyResult.Applied`.
2. **Unresolved** — node was in the spec, but its `IsIgnored` /
   `Resolve` evaluation hit `ErrDataPending` (a CEL expression
   referenced data the cluster hasn't surfaced yet). NodeID lands in
   `ApplyResult.Unresolved`. Identities for this cycle are unknown.
3. **Intentionally skipped** — node was in the spec but `includeWhen`
   returned `false` (or contagious ignore propagated). Neither in
   Applied nor in Unresolved.

Only category (3) drives prune. Category (1) populates the next
authoritative tracking record. Category (2) protects the previous
tracking record from being torn down on uncertainty.

## When prune actually fires

The prune step runs **only** when `Executor.Apply` returns `nil`. Any
error — soft (`ErrNotReady` wrapping `ErrDataPending` or
`ErrWaitingForReadiness`) or hard — skips prune. On error, status
widens to `previous ∪ Applied` (the *union*, not the diff), so the
controller never *forgets* a resource it has applied at some point.

The trade-off: a Graph with a perpetually-not-ready node never prunes
anything else, even if the spec was edited to drop other resources.
Operators get a clear signal — the Ready condition stays False with a
reason of `WaitingForReadiness` or `DataPending` — and resources stay
until the not-ready situation resolves. This is preferable to pruning
resources whose context we can't fully evaluate.

## Lifecycle: a Graph through one reconcile

```
spec change / drift event arrives
  │
  ▼
reconcileGraph:
  previous = status.ManagedResources
  applyResult, err = Executor.Apply(...)
  newSet, candidates = diff(previous, applyResult)
  ▼
err == nil ?
  ├── yes: Executor.Delete(candidates)   (prune)
  │        status.ManagedResources = newSet
  │
  └── no:  status.ManagedResources = union(previous, applyResult.Applied)
           (next reconcile retries with widened set)
  ▼
updateStatus → patches status onto API server with generation guard
```

The write happens once per reconcile, at the end. We do *not*
write-ahead before Apply. The reasoning: even a crash mid-Apply is
recoverable because the previous reconcile's status holds, and the
next reconcile re-resolves and re-applies (SSA is idempotent). What
matters is that we never *forget* a resource we applied. The union
on failure handles that.

## Delete (finalizer)

The deletion path used to re-compile and re-resolve the spec to know
what to delete. This is no longer correct because:

- Spec edits between apply and delete change the set of resources
  the spec would re-derive
- Compile-time failures (e.g. data-pending) would leave us unable to
  enumerate at all

The new path:

```go
if !g.DeletionTimestamp.IsZero() {
    if len(g.Status.ManagedResources) > 0 {
        Executor.Delete(ctx, g.Status.ManagedResources)
    }
    ... drop finalizer ...
}
```

The Executor iterates the slice in reverse (reverse-apply order),
calls `client.Delete` per entry with the UID precondition, and
tolerates `NotFound` + `Conflict` (UID mismatch). No spec is consulted.

## UID precondition

Without a UID precondition, a hostile or accidental sequence —

1. Controller applies `cm-1` (UID `aaa`)
2. Some other actor deletes `cm-1`
3. The same actor recreates `cm-1` with a different purpose (UID `bbb`)
4. Controller decides to prune `cm-1`

— would delete the impostor. With the precondition, step 4 fails with
a `Conflict` because the live UID is `bbb` but we asked for `aaa`. The
Executor treats `Conflict` the same as `NotFound`: we are not
authorized to delete what we don't actually own.

The UID field is populated from the SSA response, so a freshly-
applied resource always carries a valid UID. On a controller restart,
the UID re-populates on the next successful Apply.

## What this design deliberately does not do

- **No ApplySet annotations.** No KEP-3659 `applyset.kubernetes.io`
  labels on children. No GroupKind annotation on the Graph. Tracking
  is structured-status only. Other tools cannot discover children by
  label query; conversely, our prune logic never has to LIST.
- **No write-ahead.** Status writes happen post-Apply. A crash
  mid-Apply leaves resources applied but untracked; the next
  reconcile re-applies them and tracks them. The cluster may
  temporarily hold resources not yet in status, but never the
  reverse.
- **No revision history.** Only the current set is persisted, not
  prior generations. Audit trails are out of scope.
- **No size cap.** A `forEach` over 10,000 elements writes 10,000
  status entries. Storage in etcd is bounded but real; documented as
  accepted.
- **No OwnerReferences.** We do not rely on Kubernetes garbage
  collection for cascading delete. Tracking + UID precondition is
  the only mechanism. This means cluster-scoped resources and
  cross-namespace ownership work uniformly (OwnerReferences cannot
  span namespaces).
- **No partial-prune resumption.** A prune failure mid-list returns
  to the caller; the reconciler keeps the union in status and retries
  on the next cycle. There is no per-entry retry record.

## Boundaries

- **Compiler** does not see `ManagedResources`. The compile cache key
  is the normalized `GraphSpec`; tracking status changes don't
  invalidate it.
- **Runtime** does not see `ManagedResources`. The execution model is
  a pure walk over the compiled program; tracking lives one layer up.
- **Executor.Apply** returns identities for what it actually applied;
  it does not know about previous tracking. The Reconciler does the
  diff.
- **Executor.Delete** takes `[]ManagedResource` directly. It does
  not consult a `Runtime` or compile.
- **Reconciler** owns the diff math, the gate logic for when to
  prune, and the status-write contract.

This split keeps the Executor stateless across reconciles and lets
future executors (ApplySet-aware, dry-run, diff-only) drop in without
the tracking layer changing.
