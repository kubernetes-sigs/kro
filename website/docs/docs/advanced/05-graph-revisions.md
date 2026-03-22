---
sidebar_position: 5
---

# Graph Revisions

Every time you change an RGD's spec, kro creates a numbered, immutable snapshot
called a **GraphRevision**. You can list them with `kubectl` to see what changed,
when, and whether the change compiled successfully.

```bash
$ kubectl get graphrevisions
NAME                          REVISION   READY   AGE
my-webapp-r1-a1b2c3d4e5f6     1          True    2d
my-webapp-r2-a1b2c3d4e5f6     2          True    1d
my-webapp-r3-a1b2c3d4e5f6     3          True    5m
```

:::warning Internal API
GraphRevisions use the `internal.kro.run/v1alpha1` API group. This API is **not
intended for programmatic consumption** — its schema, naming conventions, and
behavior may change across kro releases without notice. You can safely observe
GraphRevisions via `kubectl` for debugging and auditing purposes, but do not
build tooling or automation that depends on their structure.
:::

## What You See

Each GraphRevision has a `READY` column that tells you its compilation state:

| READY     | What it means                                             |
| --------- | --------------------------------------------------------- |
| `True`    | Compiled successfully — instances are using this revision  |
| `Unknown` | Still compiling — instances wait until it finishes         |
| `False`   | Compilation failed — instances are blocked (see below)     |

Use `-o wide` to see which RGD a revision belongs to and its spec hash:

```bash
$ kubectl get graphrevisions -o wide
NAME                          RGD        REVISION   HASH              READY   AGE
my-webapp-r3-a1b2c3d4e5f6     my-webapp  3          a1b2c3d4e5f6...   True    5m
```

To see the full compilation error on a failed revision:

```bash
kubectl describe graphrevision my-webapp-r3-a1b2c3d4e5f6
```

## What Happens When You Update an RGD

1. kro hashes your new spec. If the hash matches the latest revision, nothing
   happens — cosmetic YAML changes (key reordering, whitespace) are ignored.
2. kro validates the spec. If it's invalid, the RGD is marked
   `ResourceGraphAccepted=False` and no revision is created.
3. A new GraphRevision is created with the next revision number.
4. The GraphRevision controller compiles the snapshot independently.
5. Once compiled, instances start using the new revision.

:::danger Failed revisions block instances
If the latest revision fails compilation, instances **do not fall back** to an
older revision. They stop progressing until you push a new valid spec. Always
check `kubectl get graphrevisions` if your RGD is stuck in `Inactive` after an
update.
:::

## Debugging

**RGD stuck in Inactive after a spec update:**

```bash
# List revisions for a specific RGD
kubectl get graphrevisions -l internal.kro.run/resource-graph-definition-name=<rgd-name>

# If the latest shows READY=False, inspect the error
kubectl describe graphrevision <name>
```

**Revision number keeps incrementing without spec changes:**

This shouldn't happen — kro deduplicates by spec hash. Check whether something
is modifying the RGD spec (e.g., a GitOps tool reapplying with formatting that
changes semantic content).

**Instances not picking up a new spec:**

Verify the latest GraphRevision shows `READY=True`. If it's `Unknown`, the
GraphRevision controller may be backlogged — check controller logs.

## Ownership and Lifecycle

Every GraphRevision carries an `ownerReference` pointing back to its source RGD.
This has two consequences you should know about:

**Deleting an RGD deletes all its revisions.** Kubernetes garbage collection
cascades the delete. Each revision is finalized individually — kro evicts it from
the in-memory registry before allowing the object to be removed, so instance
controllers never read stale compiled graphs from a deleted revision.

**Recreating an RGD with the same name starts fresh.** The new RGD gets a new
UID, so it won't match the old ownerReferences. Any leftover registry entries
from the old RGD are cleaned up on the first reconcile.

GraphRevisions are also subject to a **retention limit** — kro keeps at most N
revisions per RGD (default 5). When the count exceeds the limit, the oldest
revisions are pruned. The latest revision is always retained regardless of the
limit.

### Immutability

GraphRevision specs are immutable after creation. The API server rejects any
update to the `spec` field via CEL validation (`self == oldSelf`). This
guarantees that every revision is a faithful snapshot of the RGD spec at the
time it was issued — there is no way to silently alter a revision's content
after the fact.

## Configuration

GraphRevision behavior is controlled through Helm values:

| Setting                                    | Default | Description                        |
| ------------------------------------------ | ------- | ---------------------------------- |
| `config.graphRevisionConcurrentReconciles` | `1`     | Parallel GraphRevision reconciles  |
| `config.rgd.maxGraphRevisions`             | `5`     | Maximum revisions retained per RGD |

```yaml
config:
  graphRevisionConcurrentReconciles: 1
  rgd:
    maxGraphRevisions: 5
```

For most clusters the defaults are fine. Lower `maxGraphRevisions` if you have
many frequently-updated RGDs and want to reduce object count. Raise it if you
need deeper audit history.

## How It Works

Under the hood, the RGD controller and the GraphRevision controller split
responsibilities:

```
RGD spec change
    │
    ▼
Hash current spec ──── matches latest revision? ──── yes ──▶ no-op
    │
    no
    │
    ▼
Validate spec ──── invalid? ──▶ mark ResourceGraphAccepted=False, stop
    │
    ok
    │
    ▼
Create GraphRevision (revision N+1)
    │
    ▼
GR controller compiles snapshot ──── fails? ──▶ GR marked Failed
    │
    ok
    │
    ▼
GR marked Active, instances use it
```

The RGD controller owns the revision lineage: hashing, dedup, issuance, and
garbage collection. The GraphRevision controller owns compilation: it takes
each snapshot, builds the graph, and writes the result into an in-memory
registry that instance controllers read from.

Instances always resolve the latest issued revision. The revision number is
monotonic and persisted in `status.lastIssuedRevision` — it survives controller
restarts and is not reset by garbage collection.

### GraphRevision Fields

| Field                    | Description                                        |
| ------------------------ | -------------------------------------------------- |
| `spec.revision`          | Monotonic revision number within this RGD          |
| `spec.snapshot.name`     | Source RGD name                                    |
| `spec.snapshot.spec`     | Full immutable copy of the RGD spec at issuance    |
| `status.topologicalOrder`| Resource creation order from the compiled graph    |
| `status.conditions`      | `GraphVerified` and aggregate `Ready`              |

The spec hash is exposed as the label `kro.run/graph-revision-hash` for
filtering convenience.
