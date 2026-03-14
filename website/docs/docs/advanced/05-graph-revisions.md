---
sidebar_position: 5
---

# Graph Revisions

When you update a ResourceGraphDefinition's spec, kro creates an immutable
snapshot called a **GraphRevision** before applying the change. This gives you a
durable record of every compiled graph that ran in your cluster - useful for
debugging, auditing, and understanding what changed between deployments.

:::warning Internal API GraphRevisions use the `internal.kro.run/v1alpha1` API
group. This API is **not intended for programmatic consumption** - its schema,
naming conventions, and behavior may change across kro releases without notice.
You can safely observe GraphRevisions via `kubectl` for debugging and auditing
purposes, but do not build tooling or automation that depends on their
structure. :::

## Why Revisions Exist

Without revisions, updating an RGD immediately hot-swaps the compiled graph for
all instances. There is no record of what ran before, no way to compare what
changed, and no identity to pin a rollout to. GraphRevisions solve this by:

- **Recording history** - Every distinct RGD spec produces a numbered, immutable
  snapshot
- **Decoupling compilation** - A dedicated controller compiles each revision
  independently, so a broken spec doesn't corrupt the running graph
- **Enabling future rollout controls** - Revision identity is a prerequisite for
  features like canary migration and rollback (not yet implemented)

## How It Works

When you create or update an RGD:

1. **Hash** - kro computes a deterministic hash of the RGD spec (normalizing
   YAML formatting and key ordering so cosmetic edits don't trigger new
   revisions)
2. **Deduplicate** - If the hash matches the latest GraphRevision, nothing
   happens
3. **Pre-flight compile** - kro compiles the spec in-memory to catch errors
   early
4. **Issue revision** - On success, kro creates a new GraphRevision with a
   monotonically increasing revision number
5. **Compile and activate** - The GraphRevision controller compiles the snapshot
   and makes it available to instance controllers
6. **GC** - Old revisions beyond the retention limit are pruned

```
RGD spec change
    │
    ▼
Hash current spec ──── matches latest GR? ──── yes ──▶ no-op
    │
    no
    │
    ▼
Pre-flight compile ──── fails? ──▶ set condition, stop
    │
    ok
    │
    ▼
Create GraphRevision (revision N+1)
    │
    ▼
GR controller compiles ──── fails? ──▶ GR marked Failed
    │
    ok
    │
    ▼
GR marked Active, instances use it
```

Instances always resolve the latest **Active** revision. If a new revision is
still compiling (Pending) or failed compilation, instances continue using the
previous Active revision until a new one succeeds.

## Observing Revisions

List all GraphRevisions:

```bash
$ kubectl get graphrevisions
NAME                REVISION   READY   AGE
my-webapp-r000001   1          True    2d
my-webapp-r000002   2          True    1d
my-webapp-r000003   3          True    5m
```

Inspect a specific revision:

```bash
$ kubectl get graphrevision my-webapp-r000003 -o wide
NAME                RGD        REVISION   HASH              READY   AGE
my-webapp-r000003   my-webapp  3          a1b2c3d4e5f6...   True    5m
```

Check revision conditions:

```bash
$ kubectl describe graphrevision my-webapp-r000003
```

Key fields in a GraphRevision:

| Field                              | Description                                      |
| ---------------------------------- | ------------------------------------------------ |
| `spec.resourceGraphDefinitionName` | Source RGD name                                  |
| `spec.revision`                    | Monotonic revision number within this RGD        |
| `spec.specHash`                    | SHA-256 hash of the normalized RGD spec          |
| `spec.definitionSpec`              | Full immutable snapshot of the RGD spec          |
| `status.topologicalOrder`          | Resource creation order from the compiled graph  |
| `status.conditions`                | `GraphVerified` (compilation result) and `Ready` |

### RGD Status

The RGD's status includes a high-water mark for revision tracking:

| Field                       | Description                                                                                                     |
| --------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `status.lastIssuedRevision` | Highest revision number ever issued for this RGD. Persisted and monotonic - survives GC and controller restarts. |

## Revision States

Each GraphRevision transitions through one of three states:

| State       | Ready Condition | Meaning                                                                     |
| ----------- | --------------- | --------------------------------------------------------------------------- |
| **Pending** | `Unknown`       | Created but not yet compiled                                                |
| **Active**  | `True`          | Compiled successfully, available to instances                               |
| **Failed**  | `False`         | Compilation failed - check the `GraphVerified` condition message for details |

A Failed revision does not affect running instances. They continue using the
last Active revision.

## Lifecycle and Cleanup

GraphRevisions are cleaned up through two mechanisms:

**Owner references** - Each GraphRevision has an `ownerReference` pointing to its
source RGD. Deleting an RGD triggers Kubernetes garbage collection of all its
revisions.

**Retention limit** - kro retains a bounded number of revisions per RGD. When the
count exceeds the limit, the oldest revisions are pruned. The newest Active
revision is always retained regardless of the limit.

## Configuration

GraphRevision behavior is controlled through Helm values:

| Setting                                    | Default | Description                        |
| ------------------------------------------ | ------- | ---------------------------------- |
| `config.graphRevisionConcurrentReconciles` | `1`     | Parallel GraphRevision reconciles  |
| `config.rgd.maxGraphRevisions`             | `5`    | Maximum revisions retained per RGD |

```yaml
config:
  graphRevisionConcurrentReconciles: 1
  rgd:
    maxGraphRevisions: 5
```

For most clusters the defaults are fine. Lower `maxGraphRevisions` if you have
many frequently-updated RGDs and want to reduce object count. Raise it if you
need deeper audit history.

## Debugging

**RGD stuck in Inactive after spec update:**

Check whether the latest GraphRevision compiled successfully:

```bash
kubectl get graphrevisions -l internal.kro.run/resource-graph-definition-name=<rgd-name>
```

If the latest revision shows `READY=False`, describe it for the compilation
error:

```bash
kubectl describe graphrevision <rgd-name>-r<NNNNNN>
```

**Revision number keeps incrementing without spec changes:**

This shouldn't happen - kro deduplicates by spec hash. If you see this, check
whether something is modifying the RGD spec (e.g., a GitOps tool reapplying with
different YAML formatting that changes semantic content).

**Instances not picking up new spec:**

Verify the latest GraphRevision is Active (`READY=True`). If it's Pending, the
GraphRevision controller may not be running or may be backlogged - check
controller logs.
