---
sidebar_position: 5
---

# Status and Lifecycle

kro reports what it is doing with a Graph through `status.conditions`, records
what it has created in `status.managedResources`, and uses that record to clean
up when nodes are removed or the Graph is deleted.

## Conditions

A Graph has three conditions. `Ready` is the summary; `Accepted` and
`ResourcesConverged` are what it is built from.

```text
Ready                     True only when both below are True
├── Accepted              the spec compiled
└── ResourcesConverged    every node is applied and its readyWhen holds
```

| Condition | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `Accepted` | `True` | `Compiled` | The spec is valid: node IDs are unique, expressions parse and type-check, and the dependency graph is acyclic. |
| `Accepted` | `False` | `InvalidGraph` | The spec failed validation or compilation. The message says why. Nothing is applied. |
| `ResourcesConverged` | `True` | `Applied` | Every node has been applied and satisfies its `readyWhen` conditions. |
| `ResourcesConverged` | `False` | `WaitingForReadiness` | Resources were applied, but at least one `readyWhen` condition is not yet true. |
| `ResourcesConverged` | `False` | `DataPending` | A node references data that does not exist in the cluster yet, so it and its dependents cannot be resolved. |
| `ResourcesConverged` | `False` | `FieldManagerConflict` | A field a node wants to write is owned by another field manager that kro will not take over: a `patch` field owned by anything else, or a `template` field or object owned by another Graph. The message names the target. Nothing is written until that owner releases the field. |
| `ResourcesConverged` | `False` | `ApplyFailed` | A resource could not be applied. The message carries the API server error. |
| `ResourcesConverged` | `False` | `PruneFailed` | A retired resource could not be deleted. |
| `ResourcesConverged` | `False` | `ReleaseFailed` | A retired `patch` contribution could not be released from its target. |
| `ResourcesConverged` | `False` | `DeleteFailed` | During Graph deletion, a managed resource or contribution could not be removed. |
| `Ready` | `True` | `Ready` | `Accepted` and `ResourcesConverged` are both `True`. |
| `Ready` | `False` / `Unknown` | *(copied)* | Copies the status, reason, and message of the least healthy dependent condition. |

`WaitingForReadiness` and `DataPending` are normal while a Graph settles; kro
retries with backoff. `FieldManagerConflict` is retried the same way, but it does
not clear until the other manager gives up the field. `InvalidGraph` and
`ApplyFailed` require a change to the spec or the cluster.

```bash
$ kubectl get graph my-app -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
Accepted=True (Compiled)
ResourcesConverged=False (WaitingForReadiness)
Ready=False (WaitingForReadiness)
```

`kubectl get graphs` shows the `Ready` status in the `READY` column.

## Status Fields

```kro
status:
  conditions: []
  appliedServiceAccount: system:serviceaccount:default:my-app-applier
  managedResources:
    - nodeID: deployment
      apiVersion: apps/v1
      kind: Deployment
      namespace: default
      name: my-app
      uid: 4b0e7d3a-...
  contributions:
    - apiVersion: apps.example.com/v1
      kind: WebApp
      namespace: default
      name: my-webapp
      subresource: status
      fieldManager: kro-graphengine.patch.d2ba416cfd76.9a1c4e0f2b7d
```

- **`managedResources`** - Every resource a `template` node has applied, in
  apply order. `nodeID` repeats for each item of a `forEach` collection. This
  list is the authority for pruning and deletion, so a resource kro created is
  cleaned up even after the node that created it is renamed or removed. It is
  capped at 5000 entries.
- **`contributions`** - Every target and field manager a `patch` node has
  written to. Used to release fields when a `patch` node is removed or the Graph
  is deleted.
- **`appliedServiceAccount`** - The impersonated identity the last successful
  apply ran under. Deletion uses this identity, not the current
  `spec.serviceAccountName`, so changing the ServiceAccount after resources
  exist cannot leave them undeletable.

## What kro Stamps on Resources

Resources created by a `template` node carry:

| Key | Type | Value |
| --- | --- | --- |
| `kro.run/node-id` | label | The node's `id`. For a node inside a `graph` node, the dot-joined path (`backend.deployment`); hashed if longer than 63 characters. |
| `kro.run/collection-index` | label | Position within a `forEach` expansion. Collection items only. |
| `kro.run/collection-size` | label | Total item count of the expansion. Collection items only. |
| `kro.run/instance-id` | label | The Graph's UID. Collection items only. |
| `internal.kro.run/node-path` | annotation | The full slash-separated node path (`backend/deployment`). |
| `internal.kro.run/apply-order` | annotation | The node's dependency-layer number. Informational only. |

kro does not set owner references on the resources a Graph creates. Cleanup is
driven by `status.managedResources` and the Graph's finalizer, not by
Kubernetes garbage collection.

Server-side apply field managers are visible in each resource's
`metadata.managedFields`:

- `template` nodes apply as `kro-graphengine.tmpl.<graph>`, where `<graph>` is
  a 12-character digest of the Graph's UID. The manager is per Graph, not per
  node, and applies without force. If another Graph already manages a field,
  the apply is rejected with a conflict rather than stealing the field, and the
  node is reported not ready. If a field kro manages is edited by something
  that is not another Graph, kro reclaims it on the next reconcile.
- `patch` nodes apply as `kro-graphengine.patch.<graph>.<node>`. Status
  patches are forced; main-resource patches are not.

## Pruning

Each reconcile compares what was applied against `status.managedResources`. A
resource that was applied before and is not applied now is **pruned**: kro
deletes it. This happens when a node is removed from the spec, when a
`forEach` collection shrinks, when an `includeWhen` becomes false, or when a
resource is renamed.

A resource is never pruned because its node is merely waiting. If a node cannot
be resolved this reconcile (`DataPending`), the resources it applied previously
are kept.

Retired `patch` contributions are **released** rather than pruned: kro applies
an empty payload under the contribution's field manager, which removes the
fields that manager owned and leaves the rest of the target untouched. The
target object is never deleted by a `patch` node.

Before any apply, kro records the resources it is about to create in
`status.managedResources`. If the controller restarts between applying a
resource and recording the result, the next reconcile still knows about it.

## Deletion

Deleting a Graph triggers cleanup through the `kro.run/graph-finalizer`
finalizer:

1. DELETE requests are issued for tracked resources in reverse
   `status.managedResources` order.
2. Every entry in `status.contributions` is released.
3. The finalizer is removed and the Graph object is deleted.

Deletion works entirely from status. The spec is not recompiled, so a Graph
whose spec has become invalid still cleans up everything it created. Deletion
runs under `status.appliedServiceAccount`.

If a resource cannot be deleted, kro sets `ResourcesConverged=False` with reason
`DeleteFailed`, keeps the finalizer, and retries. The Graph object remains until
cleanup succeeds or the finalizer is removed by hand.

## Retries

When a Graph is waiting (`WaitingForReadiness`, `DataPending`, or
`FieldManagerConflict`), kro requeues it with exponential backoff starting at one
second and capped at five minutes.
The backoff resets on any reconcile that is not waiting. Changes to a resource
the Graph created or reads also trigger an immediate reconcile through kro's
watches, so a Graph blocked on a missing `ref` target reconciles as soon as the
target appears.

Hard failures (`ApplyFailed` and the other `*Failed` reasons) use
controller-runtime's standard rate-limited retry.

## Tuning

| Setting | Helm value | Flag | Default |
| --- | --- | --- | --- |
| Graphs reconciled in parallel | `config.graphConcurrentReconciles` | `--graph-concurrent-reconciles` | `1` |
| Parallel applies within one `forEach` collection | `config.applyConcurrency` | `--apply-concurrency` | `20` |
| Maximum items per `forEach` expansion | `config.rgd.maxCollectionSize` | `--rgd-max-collection-size` | `1000` |

See [Controller Tuning](../../advanced/03-controller-tuning.md) for the full set.

## No Revision History

A Graph is compiled from its current spec on every change. Unlike a
ResourceGraphDefinition, kro does not create
[GraphRevision](../../advanced/05-graph-revisions.md) objects for Graphs, and
there is no persisted history of previous specs. Use your source control system
or GitOps tooling for that.

## Next Steps

- **[Overview](./01-overview.md)** - When to use a Graph and how to enable it
- **[Nodes](./02-nodes.md)** - Which nodes create, read, and contribute to resources
- **[Access Control](../../advanced/01-access-control.md#graph-and-serviceaccount-impersonation)** - The identity resources are applied under
- **[Graph API Reference](../../../api/crds/graph.md)** - Every status field
