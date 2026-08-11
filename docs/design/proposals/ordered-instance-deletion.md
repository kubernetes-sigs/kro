# CEL-independent ordered instance deletion

## Problem statement

Instance deletion currently reconstructs managed-resource identities from the
compiled graph. That makes cleanup depend on the same desired-state and CEL
evaluation pipeline used during normal reconciliation.

[Issue #1316](https://github.com/kubernetes-sigs/kro/issues/1316) demonstrates
why that dependency is unsafe. An instance creates a managed child whose
identity or desired state references an external object. The child remains
alive behind a finalizer owned by another controller. The external reference is
then deleted, followed by (or at the same time as) the root instance. During
deletion, kro can no longer evaluate the child's CEL expressions because the
external data is unavailable. The child is not deleted and the kro finalizer on
the root instance is stuck.

This is one example of a broader lifecycle invariant: deleting an instance must
remain possible when desired-state projection, identity CEL, readiness CEL, or
external observation is unavailable. Cleanup needs durable identities and
ordering information from the resources that normal reconciliation already
created.

## Proposal

Persist each managed child's _apply order_ and use the instance's `ApplySet`
inventory as the authority for deletion. Instance deletion lists that inventory
without resolving desired objects, groups members by persisted order, and
deletes only the highest remaining order. It waits for that entire wave to
disappear before advancing. Members without a usable persisted order share a
fallback wave that runs after every valid order.

#### Overview

The lifecycle is governed by these invariants:

- Managed-resource identity during deletion comes from persisted `ApplySet`
  inventory, not from reconstructed desired objects or current graph nodes.
- Normal reconciliation gives every managed child an annotation
  `internal.kro.run/apply-order`. Its value is the child's one-based reverse
  topological "layer". Dependents have higher values than their dependencies,
  while nodes that can be deleted together share a value. In other words,
  children sharing the same value belong to the same _deletion wave_.
- Deletion is processed in waves, actuating deletion only for the highest order
  still present in inventory.
- A wave remains active until all of its objects are absent; having a deletion
  timestamp is not enough, but the child must be fully garbage collected.
- Members with missing or invalid order metadata share one fallback wave. That
  wave runs last and has no ordering guarantee within it. This is also the
  behavior that will apply to deletions triggered concurrently with rolling this
  change out; effectively, every graph not reconciled at least once with this
  change, has all its children with the fallback apply order (`0`).
- The instance finalizer is removed only after the inventory is empty, i.e. when
  all children have been successfully deleted and garbage collected.
- Progress is made observable with a condition on the root instance.

#### Normal reconciliation writes metadata

Resources with no deletion timestamp, processes and stores metadata as follows:

Each child node gets
- a label with a node id referencing the root instance (current behavior)
- an annotation with an apply order, indicating its topological layer in the DAG
  (this is new for this proposal)

Additionally, the root instance itself gets
- an annotation indicating all GVKs and namespaces where to find children; this
  is the `ApplySet` inventory already managed by the current behavior
- an annotation with a checksum of the inventory, guarding against partial
  mutation that might result in incorrect deletion behavior

The current implementation already ensures that the `ApplySet` inventory
includes all GKVs/namespaces in use, even in the case of transient failures, so
relying on it for finding children is valid even when the RGD has recently been
updated and there are children present in the cluster that are not referenced in
the graph.

#### ⚠️ Label/annotation domain guards

As [previously proposed][krep-label-migration], `internal.kro.run/` is used as a
prefix for new metadata. Additionally, both `kro.run/` and `internal.kro.run/`
are now reserved label and annotation prefixes in resource templates; defining
either manually on an RGD child instance is now an error. RGD authors therefore cannot override the node
identity or deletion order owned by the controller.

[krep-label-migration]: https://github.com/kubernetes-sigs/kro/blob/main/docs/design/proposals/label-migration.md

#### Ordered deletion waves

When deletion of the root instance is requested (its deletion timestamp is non-
nil), the reconciler _does not_ process the entire RGD to resolve identities,
CEL expressions, etc.

Instead, it inspects the root instance's `ApplySet` inventory annotation, and
issues `LIST` calls for each GVK/namespace, filtered by the node-id label.

Resources are collected into _waves_ based on the persisted apply order.
Missing, malformed, zero, or negative values are assigned internal order zero,
which is lower than every valid order; all such candidates therefore share a
fallback wave that is selected only after every valid ordered wave is absent.
There is no ordering guarantee within a wave; this _might_ violate previous
reverse-order guarantees in the fallback wave, but the ordering of the waves
themselves should guarantee reverse-order deletion for all valid resources.

After validation, the controller finds the highest remaining order and selects
only candidates at that order. It skips a `DELETE` for a candidate that already
has a deletion timestamp, but that candidate remains in the active wave and
blocks every lower order until it is actually absent. Other active-wave
candidates are passed to `ApplySet.DeleteOrphan`; its UID precondition prevents
a LIST/DELETE race from deleting a different object recreated with the same
name.

`DELETE` calls within a wave are sequential. All candidates remain visible on
the next LIST, so every reconciliation with non-empty inventory returns a
delayed requeue. A UID conflict also requeues and does not permit a lower wave
to advance. Only a later reconciliation that observes no higher-order members
can begin the next order. Empty inventory is the sole condition for removing
the root finalizer and cleaning up coordinator watches.

Deletion status is reported on the root instance, using `ResourcesReady=Unknown`
with reason `UnderDeletion`. When an RGD defines author conditions, deletion
preserves their last persisted values and overlays this kro-owned lifecycle
condition because author conditions cannot be reevaluated without processing the
full graph including CEL resolution etc. If the author defines a condition with
the same type, the deletion condition temporarily takes precedence so cleanup
failures remain observable.

#### Rollout behavior

When an instance controller registers a GVR at startup, it explicitly enqueues
the instances already present in its cache. Healthy, unsuspended instances
therefore run normal reconciliation and acquire the order annotation through SSA,
even when their child specs are unchanged. This proposal adds neither a
separate migration job nor a forced restart loop.

Some instances can miss that backfill. An instance already deleting when the
new version starts bypasses normal child apply. Suspended instances, instances
whose desired resources cannot resolve, and instances deleted before startup
reconciliation completes can also retain children without the annotation.
Deletion still processes every annotated wave in reverse order, then deletes
all remaining children with missing or invalid annotations in the shared
fallback wave.

This compatibility behavior deliberately prioritizes deletion liveness over
reverse-order guarantees for resources whose historical order was never
persisted. It avoids leaving an instance and its children indefinitely stuck in
deletion during rollout while preserving strict ordering wherever the metadata
is available.

#### Failure behavior

ApplySet `LIST` failures and `RESTMapping` failures retain the root finalizer
and retry through normal reconciliation error handling. `DELETE` errors retain
the finalizer and propagate through the instance-level status. Inventory,
`LIST`, `DELETE`, and UID-conflict errors replace the generic deletion-progress
message with an actionable `ResourcesReady` condition message. A UID
precondition conflict causes a delayed requeue with the active wave unchanged.

Deletion validates the ApplySet parent ID, kro tooling ownership, and the
presence and syntax of the persisted group-kind and namespace annotations
before listing members. Normal reconciliation also writes a checksum over that
inventory; when present, deletion verifies it to detect accidental partial
mutation. Invalid inventory retains the root finalizer and reports an
actionable reconciliation error rather than treating the inventory as empty
and orphaning children. Parents created by older kro versions may omit the
checksum, but must still carry the standard ApplySet inventory annotations.

A child with a deletion timestamp is expected progress rather than an error.
It remains Deleting and continues to block lower orders until the API server no
longer lists it. No error path is allowed to issue deletion for a lower-order
resource while a higher-order member remains.

## Other solutions considered

#### Finalize external references

**Rejected.** External references are read-only, can be shared by unrelated
instances, and can be managed by another authority. Giving kro a finalizer on
them would change that ownership contract and could block their deletion.

#### Fully unordered label-based deletion

**Rejected.** as the general deletion strategy. Finding children by an ownership
label avoids CEL but deleting all resources together discards usable persisted
ordering. The compatibility fallback limits unordered deletion to members that
have no usable order and runs it only after every ordered member is absent, but
under normal operations we should be able to guarantee that dependencies are
deleted before their dependents.

#### Reconstruct identity from CEL and current desired state

**Rejected.** This is the failure mode in #1316. Deletion must survive missing
dependencies, invalid expressions, unavailable observation, and desired state
that no longer describes resources created by an older revision.

#### Infer missing order from current node IDs

**Rejected.** for this rollout. It complicates migration and can be wrong when
the current GraphRevision differs from the revision that created an older
resource. The fallback wave represents that uncertainty explicitly without
inventing an order that may be wrong.
