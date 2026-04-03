---
sidebar_position: 5
---

import RevisionFlow from '@site/src/components/RevisionFlow';

# Graph Revisions

Every time you change an RGD's spec, kro creates a numbered, immutable snapshot
called a **GraphRevision**. GraphRevisions give you a history of what changed
and whether each change compiled successfully. If a new spec breaks, you see
exactly which revision failed and why - instances stop progressing until you
push a valid spec.

<RevisionFlow />

:::warning Internal API
GraphRevisions use the `internal.kro.run/v1alpha1` API group. This API may
change across kro releases without notice. You can safely observe GraphRevisions
via `kubectl` for debugging and auditing, but do not build tooling that depends
on their structure.
:::

## What Happens When You Update an RGD

1. **Hash check.** kro hashes the new spec. If the hash matches the latest
   revision, nothing happens - cosmetic changes like key reordering or
   whitespace are ignored.

2. **Validation.** kro validates the spec structure. If invalid, the RGD is
   marked `GraphAccepted=False` and no revision is created.

3. **Issuance.** A new GraphRevision is created with the next revision number.
   Revision numbers are monotonic and never reused, even after garbage
   collection. The high-water mark is persisted in `status.lastIssuedRevision`
   on the RGD, so numbering survives controller restarts.

4. **Compilation.** The GraphRevision controller independently compiles the
   snapshot - parsing the schema, resolving CEL expressions, and building the
   dependency graph. On success the GraphRevision is marked
   `GraphVerified=True`; on failure, `GraphVerified=False` with an error
   message.

5. **Activation.** Once compiled, the graph is stored in an in-memory registry
   that instance controllers read from. The RGD is marked
   `GraphRevisionsResolved=True` and instance reconciliation proceeds.

:::danger Failed revisions block instances
If the latest revision fails compilation, instances **do not fall back** to an
older revision. They stop progressing until you push a new valid spec.
:::

## Inspecting Revisions

GraphRevisions have the short name `gr`, so you can use either form:

```bash
kubectl get gr
# or
kubectl get graphrevisions
```

```text
NAME              REVISION   READY   AGE
my-webapp-r00001  1          True    2d
my-webapp-r00002  2          True    1d
my-webapp-r00003  3          True    5m
```

The `READY` column reflects the compilation result: `True` means the snapshot
compiled successfully, `False` means compilation failed (inspect with
`kubectl describe`), and `Unknown` means the GraphRevision controller hasn't
processed it yet.

Use `-o wide` to see the source RGD name and spec hash:

```bash
kubectl get gr -o wide
```

```text
NAME              RGD        REVISION   HASH              READY   AGE
my-webapp-r00003  my-webapp  3          a1b2c3d4e5f6...   True    5m
```

To filter revisions for a specific RGD:

```bash
kubectl get gr -l internal.kro.run/resource-graph-definition-name=my-webapp
```

You can also filter by spec hash using the `kro.run/graph-revision-hash` label:

```bash
kubectl get gr -l kro.run/graph-revision-hash=a1b2c3d4e5f6
```

## GraphRevision Fields

### Spec

| Field                       | Description                                                    |
| --------------------------- | -------------------------------------------------------------- |
| `spec.revision`             | Monotonic revision number, unique per RGD                      |
| `spec.snapshot.name`        | Source RGD name                                                |
| `spec.snapshot.generation`  | `metadata.generation` of the RGD when this revision was issued |
| `spec.snapshot.spec`        | Full immutable copy of the RGD spec at issuance time           |

### Status

| Field                     | Description                                                  |
| ------------------------- | ------------------------------------------------------------ |
| `status.conditions`       | `GraphVerified` (compilation result) and aggregate `Ready`   |
| `status.topologicalOrder` | Resource creation order derived from the compiled graph      |
| `status.resources`        | Per-resource dependency information from the compiled graph  |

### Labels

| Label                                                  | Description                                |
| ------------------------------------------------------ | ------------------------------------------ |
| `internal.kro.run/resource-graph-definition-name`      | Source RGD name (selectable field)          |
| `kro.run/graph-revision-hash`                          | Spec hash for dedup and filtering           |

## Debugging

**Update stuck after a spec change?** Check the latest revision:

```bash
kubectl describe gr my-webapp-r00003
```

Look at the `GraphVerified` condition - if `False`, the `Message` field
contains the compilation error. Fix the RGD spec and reapply.

**Want to see the resource ordering?** After a successful compilation, the
`status.topologicalOrder` field shows the order kro will create resources:

```bash
kubectl get gr my-webapp-r00003 -o jsonpath='{.status.topologicalOrder}'
```

```text
["config","bucket","deployment","service"]
```

**Instances not picking up a new spec?** Verify the latest GraphRevision shows
`READY=True`. If `Unknown`, the GraphRevision controller may not have processed
it yet - check controller logs.

**Revision number incrementing without spec changes?** This shouldn't happen -
kro deduplicates by spec hash. Check whether something is modifying the RGD
spec (e.g., a GitOps tool reapplying with formatting that changes semantic
content).

**RGD conditions to check when things are stuck:**

| RGD Condition              | Meaning                                                       |
| -------------------------- | ------------------------------------------------------------- |
| `GraphAccepted=False`      | Spec validation failed, no revision was created               |
| `GraphRevisionsResolved=Unknown` | A revision was issued but hasn't been compiled yet      |
| `GraphRevisionsResolved=False`   | Latest revision failed compilation                      |
| `GraphRevisionsResolved=True`    | Latest revision compiled successfully, serving active   |

## Naming

GraphRevision names follow the pattern `{rgd-name}-r{revision:05d}`, giving
zero-padded revision numbers that sort lexicographically in kubectl (up to
99,999 revisions). If the combined name exceeds the Kubernetes 253-character
limit, kro truncates the RGD name and appends an FNV hash suffix to prevent
collisions.

## Ownership and Lifecycle

Every GraphRevision carries an `ownerReference` pointing to its source RGD.

**Deleting an RGD deletes all its revisions.** Kubernetes garbage collection
cascades the delete via ownerReferences.

:::tip Preserving revisions across RGD recreation
To keep GraphRevisions around when deleting an RGD, use orphan deletion:

```bash
kubectl delete rgd my-webapp --cascade=orphan
```

When you recreate the RGD, kro adopts the orphaned GraphRevisions by
`spec.snapshot.name` - revision history and compiled state carry over.
:::

### Immutability

GraphRevision specs are immutable after creation. The API server rejects updates
to the `spec` field via CEL validation (`self == oldSelf`). Every revision is a
faithful snapshot of the RGD spec at the time it was issued.

### Retention

kro keeps at most N revisions per RGD (default 5). When the count exceeds the
limit, the oldest revisions are pruned. The latest revision is always retained.

## Configuration

| Helm Value                                 | Default | Description                        |
| ------------------------------------------ | ------- | ---------------------------------- |
| `config.graphRevisionConcurrentReconciles` | `1`     | Parallel GraphRevision reconciles  |
| `config.rgd.maxGraphRevisions`             | `5`     | Maximum revisions retained per RGD |

For most clusters the defaults are fine. Lower `maxGraphRevisions` if you have
many frequently-updated RGDs and want to reduce object count.

## How It All Fits Together

The RGD controller and GraphRevision controller split responsibilities cleanly:

| Responsibility     | Owner                     | What it does                                           |
| ------------------ | ------------------------- | ------------------------------------------------------ |
| **Issuance**       | RGD controller            | Hashes spec, deduplicates, creates GraphRevision objects |
| **Compilation**    | GraphRevision controller  | Compiles snapshot, writes result to in-memory registry  |
| **Resolution**     | Instance controllers      | Resolve latest compiled graph from registry             |
| **Garbage collection** | RGD controller        | Prunes old revisions beyond retention limit             |

The runtime scheduling states (`Pending`, `Active`, `Failed`) live in the
in-memory registry, not in the GraphRevision status. The API only exposes
`GraphVerified` and aggregate `Ready` conditions. Revision numbering is
persisted in `status.lastIssuedRevision` on the RGD, so it survives controller
restarts and is never reset by garbage collection.
