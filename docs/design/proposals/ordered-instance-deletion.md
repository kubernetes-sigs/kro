# CEL-independent ordered instance deletion

## Problem statement

Instance deletion currently reconstructs managed-resource identities from the
compiled graph. That makes cleanup depend on the same desired-state and CEL
evaluation pipeline used during normal reconciliation.

[Issue #1316](https://github.com/kubernetes-sigs/kro/issues/1316) demonstrates
why that dependency is unsafe. An instance creates a managed child whose
identity or desired state references an external object. The child remains
alive behind a finalizer owned by another controller. The external reference is
then deleted, followed by the root instance. During deletion, kro can no longer
evaluate the child's CEL expressions because the external data is unavailable.
The child is not deleted and the kro finalizer on the root instance is stuck.

This is one example of a broader lifecycle invariant: deleting an instance must
remain possible when desired-state projection, identity CEL, readiness CEL, or
external observation is unavailable. Cleanup needs durable identities and
ordering information from the resources that normal reconciliation already
created.

## Proposal

Persist each managed child's apply order and use the instance's ApplySet
inventory as the authority for deletion. Instance deletion lists that inventory
without resolving desired objects, validates every member's persisted order,
and deletes only the highest remaining order. It waits for that entire wave to
disappear before advancing.

External references remain read-only inputs. They are not finalized, mutated,
or deleted by kro.

#### Overview

The lifecycle is governed by these invariants:

- Managed-resource identity during deletion comes from persisted ApplySet
  inventory, not from reconstructed desired objects or current graph nodes.
- Every managed child carries `internal.kro.run/apply-order`. Its value is the
  child's one-based position in the complete runtime DAG's total topological
  order.
- External nodes occupy positions in that total order but are not applied, so
  managed-resource order values may have gaps.
- Deletion processes only the highest order still present in inventory.
- A wave remains active until all of its objects are absent, including objects
  that already have a deletion timestamp.
- The instance finalizer is removed only after the inventory is empty.
- Missing or invalid order metadata is a visible migration error. The root
  finalizer remains, and deletion never silently changes to unordered cleanup.

#### Normal reconciliation

`processNodes` enumerates every runtime node in topological order. Regular
managed resources receive the node ID and the one-based apply order as
controller-owned labels. Every expanded member of a `forEach` collection
receives the same order because the collection is one graph node. External
references and external collections are observed but never applied and receive
no apply-order label.

The existing server-side apply path writes the label alongside the desired
resource. This also backfills an unchanged child: SSA still submits controller
metadata even when the resource spec has not changed. A later successful
reconcile against a new GraphRevision can update an object's order. An object
removed from the graph keeps its last persisted order until normal pruning
deletes it.

Both `kro.run/` and `internal.kro.run/` are reserved label prefixes in resource
templates. RGD authors therefore cannot override the node identity or deletion
order owned by the controller.

#### Deletion inventory

The instance is already the ApplySet parent. Its annotations persist the union
of group-kinds and additional namespaces that can contain members. Deletion
creates the same ApplySet, calls `Project(nil)` to reconstruct that scope from
the parent annotations, and calls `ListOrphans` with an empty keep-UID set. All
ApplySet members are consequently deletion candidates.

This inventory has two important properties. First, external references are
absent because kro never applies them and they are not ApplySet members. Second,
resources retained from an older GraphRevision remain discoverable even if
their group-kind, namespace, or node ID is absent from the current graph.

The deletion path does not call `IsIgnored`, `GetDesired`,
`GetDesiredIdentity`, `DeleteTargets`, readiness evaluation, or external
observation. Deletion is handled before compiled GraphRevision resolution.
This is intentional: resolving the current revision can itself require
unavailable dependencies or invalid CEL, which is exactly the failure mode in
#1316. The early path only needs the parent object, persisted ApplySet
annotations, and dynamic REST mappings. The tradeoff is that deletion status
cannot describe current graph nodes when no revision can be resolved; the
controller still reports the instance-level deleting condition and preserves
the finalizer on inventory or ordering failures. Keeping deletion after graph
resolution would preserve richer node status, but would leave the root
finalizer stuck whenever resolution fails.

#### Ordered deletion waves

Before issuing any DELETE, the controller parses every candidate's
`internal.kro.run/apply-order`. Only positive base-10 integers are accepted.
Missing, malformed, zero, or negative values fail the reconciliation before any
cluster mutation. The error identifies the resource's GVK, namespace, and name.
When the candidate has a current `kro.run/node-id`, that node is marked Error.
There is no inference from node ID, graph shape, collection index, or LIST
order.

After validation, the controller finds the highest remaining order and selects
only candidates at that order. It skips a DELETE for a candidate that already
has a deletion timestamp, but that candidate remains in the active wave and
blocks every lower order until it is actually absent. Other active-wave
candidates are passed to `ApplySet.DeleteOrphan`. Its UID precondition prevents
a LIST/DELETE race from deleting a different object recreated with the same
name.

DELETE calls within a wave are sequential. All candidates remain visible on
the next LIST, so every reconciliation with non-empty inventory returns a
delayed requeue. A UID conflict also requeues and does not permit a lower wave
to advance. Only a later reconciliation that observes no higher-order members
can begin the next order. Empty inventory is the sole condition for removing
the root finalizer and cleaning up coordinator watches.

Node status during deletion is derived without CEL:

- External nodes are Skipped.
- Current managed nodes represented by any live candidate are Deleting,
  including lower-order nodes that have not received DELETE yet.
- Current managed nodes with no candidates are Deleted.
- A DELETE or ordering error marks the relevant current node Error when its
  node ID is available.

#### Rollout behavior

When an instance controller registers a GVR at startup, it explicitly enqueues
the instances already present in its cache. Healthy, unsuspended instances
therefore run normal reconciliation and acquire the order label through SSA,
even when their child specs are unchanged. This proposal adds neither a
separate migration job nor a forced restart loop.

Some instances can miss that backfill. An instance already deleting when the
new version starts bypasses normal child apply. Suspended instances, instances
whose desired resources cannot resolve, and instances deleted before startup
reconciliation completes can also retain unlabeled children. These cases fail
safely during deletion: the ordering error is reported and the root finalizer
is retained.

Operators have two break-glass choices:

1. Determine the correct historical order and manually assign valid
   `internal.kro.run/apply-order` labels before allowing deletion to continue.
2. Remove the root finalizer and manually clean up all remaining managed
   children.

This is an accepted upgrade limitation, not an unordered compatibility mode.
The controller must not guess an order for an unlabeled resource.

#### Failure behavior

ApplySet LIST failures and RESTMapping failures retain the root finalizer and
retry through normal reconciliation error handling. Malformed ordering metadata
is reported before any DELETE. DELETE errors retain the finalizer, mark the
associated node Error when possible, and propagate. A UID precondition conflict
causes a delayed requeue with the active wave unchanged.

A child with a deletion timestamp is expected progress rather than an error.
It remains Deleting and continues to block lower orders until the API server no
longer lists it. No error path is allowed to issue deletion for a lower-order
resource while a higher-order member remains.

## Other solutions considered

#### Finalize external references

Rejected. External references are read-only, can be shared by unrelated
instances, and can be managed by another authority. Giving kro a finalizer on
them would change that ownership contract and could block their deletion.

#### Unordered label-based deletion

Rejected. Finding children by an ownership label avoids CEL but deletes all
resources together. That is a breaking change to kro's dependent-before-
dependency lifecycle and can remove resources while their dependents are still
terminating.

#### Reconstruct identity from CEL and current desired state

Rejected. This is the failure mode in #1316. Deletion must survive missing
dependencies, invalid expressions, unavailable observation, and desired state
that no longer describes resources created by an older revision.

#### Infer missing order from current node IDs

Rejected for this rollout. It complicates migration and can be wrong when the
current GraphRevision differs from the revision that created an older resource.
Failing visibly preserves ordering rather than hiding uncertainty.

## Scoping

#### What is in scope for this proposal?

- Persisting total topological apply positions on regular and collection
  resources.
- Reserving the public and internal kro label prefixes in RGD templates.
- ApplySet-inventory discovery for instance deletion.
- Strict, UID-preconditioned, highest-order deletion waves.
- Startup-reconcile backfill and documented manual remediation for missed
  migration.
- Unit and focused integration coverage for the lifecycle.

#### What is not in scope?

A future level-aware reconciliation engine may replace total positions with
dependency levels, as discussed in
[PR #1215](https://github.com/kubernetes-sigs/kro/pull/1215#discussion_r3164471497).
The durable-inventory and wait-for-wave semantics remain applicable: store the
level on each member, delete the highest remaining level, and wait for absence
before advancing.

This proposal does not add an upgrade migration controller or infer metadata for missed
backfills, or change normal orphan-pruning order.

## Testing strategy

#### Requirements

Fake-client deletion fixtures carry a stable UID, the computed
`applyset.kubernetes.io/part-of` value, `kro.run/node-id`, and
`internal.kro.run/apply-order`. Their parent instance carries the ApplySet
group-kind and namespace annotations. Integration tests use synthetic child
finalizers to make ordering observable rather than timing-dependent.

#### Test plan

Unit tests verify regular-resource order, shared collection order, external
exclusion, SSA label application to existing objects, and validation of both
reserved prefixes. ApplySet tests cover namespaced and cluster-scoped discovery
from parent annotations, including older resources no longer represented by the
current graph.

Deletion unit tests verify that only the highest remaining order receives
DELETE, a terminating highest-order resource blocks lower orders, the next wave
starts only after the higher wave disappears, and all resources at one order
are handled together. They also cover empty-inventory finalizer removal;
missing, malformed, zero, and negative labels before any DELETE; DELETE error
state; UID-conflict requeue; and deletion with an absent external reference.

The core integration suite reproduces #1316 by creating an external ConfigMap
and a managed child derived from it, asserting the child's persisted order,
holding the child with a synthetic finalizer, deleting both the external object
and root, and observing that the child receives a deletion timestamp. Removing
the child finalizer must allow both child and root to disappear.

A second integration scenario creates `A -> B`, holds B with a synthetic
finalizer, and deletes the root. B must become terminating while A has no
deletion timestamp. Only after B disappears may A and then the root disappear.

## Discussion and notes

The persisted order is a total topological position starting at one, not a
dependency depth. Gaps caused by external nodes are intentional and harmless.
The central contract is not that orders are contiguous; it is that deletion
uses only persisted inventory and never advances below the highest remaining
value.
