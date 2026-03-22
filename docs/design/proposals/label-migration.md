# KREP: Internal Metadata Migration

## Summary

kro uses a set of labels, all prefixed with `kro.run/`, as controller
implementation details for ownership tracking, resource reconciliation, and
collection management. Because they share the `kro.run/` API group domain, they
appear to be part of kro's public API contract. This proposal migrates all
controller-owned `kro.run/` labels to the `internal.kro.run/` prefix and
establishes a two-release deprecation window for the old keys. User-set
identifiers (`kro.run/reconcile`, `kro.run/allow-breaking-changes`) are
intentionally kept at `kro.run/` since they are part of the public API.

This is the first in a series of planned breaking changes to kro's internals.
The migration is intentionally scoped as a low-risk starting point to establish
the pattern for communicating and phasing breaking changes.

## Problem Statement

kro stamps labels and annotations on resources it manages. These serve internal
purposes:

- **Ownership tracking**: identifying which RGD and instance own a resource
- **Reconciliation**: selecting resources during list operations, detecting
  conflicts, and managing lifecycle
- **Collection membership**: tracking position and size within forEach
  expansions

All of these identifiers live under the `kro.run/` prefix, the same domain
used for kro's API group (`kro.run/v1alpha1`). This creates two problems:

1. **Users assume `kro.run/` identifiers are stable API.** If a label
   shares the API group prefix, it looks like a public contract. Users or
   tooling that depend on these (e.g., label selectors in monitoring, RBAC
   rules, or external controllers) would break without warning if kro changes
   them.

2. **kro cannot evolve its internals freely.** Any change to label keys, values,
   or semantics becomes a de facto breaking change.

## Proposal

### Overview

Migrate all controller-owned `kro.run/` labels to `internal.kro.run/` using a
two-release phased approach that relies on natural reconciliation to propagate
changes. User-set identifiers (`kro.run/reconcile`, `kro.run/allow-breaking-changes`)
remain at `kro.run/`. No manual intervention is required from users.

### Identifiers in Scope

All controller-owned `kro.run/`-prefixed labels (excludes user-set identifiers
like `kro.run/reconcile` and `kro.run/allow-breaking-changes`).

#### Labels

| Deprecated (`kro.run/`)                  | New (`internal.kro.run/`)                         |
| ---------------------------------------- | ------------------------------------------------- |
| `kro.run/node-id`                        | `internal.kro.run/node-id`                        |
| `kro.run/collection-index`               | `internal.kro.run/collection-index`               |
| `kro.run/collection-size`                | `internal.kro.run/collection-size`                |
| `kro.run/owned`                          | `internal.kro.run/owned`                          |
| `kro.run/kro-version`                    | `internal.kro.run/kro-version`                    |
| `kro.run/instance-id`                    | `internal.kro.run/instance-id`                    |
| `kro.run/instance-name`                  | `internal.kro.run/instance-name`                  |
| `kro.run/instance-namespace`             | `internal.kro.run/instance-namespace`             |
| `kro.run/instance-group`                 | `internal.kro.run/instance-group`                 |
| `kro.run/instance-version`               | `internal.kro.run/instance-version`               |
| `kro.run/instance-kind`                  | `internal.kro.run/instance-kind`                  |
| `kro.run/resource-graph-definition-id`   | `internal.kro.run/resource-graph-definition-id`   |
| `kro.run/resource-graph-definition-name` | `internal.kro.run/resource-graph-definition-name` |

`kro.run/resource-graph-definition-namespace` and
`kro.run/resource-graph-definition-version` were originally listed here but
removed during implementation — the constants were declared but never written
by any labeler, so they are dead code and excluded from the migration.

### Phase 1: Dual-Write (v0.x.0)

**Goal**: Every managed resource carries both label sets. No behavioral change.

- **Writes**: All labeler constructors (`InstanceLabeler`, `NodeLabeler`,
  `baseLabeler`) will write both the deprecated `kro.run/` label and the
  corresponding `internal.kro.run/` label.
- **Reads (point lookups)**: Continue reading `kro.run/` keys directly for
  controller-written labels (`IsKROOwned`, `CompareRGDOwnership`,
  `findRGDsForCRD`). Every resource has `kro.run/` labels during Phase 1 —
  either from before the migration (only `kro.run/`) or from the dual-write
  (both). There is no scenario where a resource has `internal.kro.run/` but
  not `kro.run/`, so a fallback helper is unnecessary for controller-written
  labels. **Constraint**: Phase 1 code MUST NOT write `internal.kro.run/`
  labels without also writing `kro.run/` labels. This invariant guarantees
  that `kro.run/` point lookups are always valid during Phase 1.
- **Reads (label selectors)**: All server-side label selectors (e.g., `List`
  with `LabelSelector`) **must continue to use the deprecated `kro.run/` keys**.
  Label selectors have no fallback mechanism, they match or they don't.
  Resources only carry `kro.run/` labels before Phase 1 reconciliation, so
  selecting on `internal.kro.run/` would silently miss them.
- **Validation**: Extend `validateNoKROOwnedLabels` to reserve both `kro.run/`
  and `internal.kro.run/` prefixes. Users cannot set labels with either prefix
  in RGD resource templates. Today only `kro.run/` is reserved.

During this phase, natural reconciliation progressively stamps the new
`internal.kro.run/` labels onto all existing resources. No forced
re-reconciliation or migration job is needed; the normal controller loop
handles it.

### Phase 2: Cutover (v0.x+1.0)

**Goal**: Remove the deprecated labels entirely.

- **Writes**: Stop writing `kro.run/` labels. Only write `internal.kro.run/`.
  Because kro applies labels via server-side apply, omitting the deprecated keys
  from the patch causes the API server to drop them from the object on the next
  reconcile, no explicit deletion code is needed.
- **Reads (point lookups)**: Switch all point lookups from `kro.run/` to
  `internal.kro.run/` keys.
- **Reads (label selectors)**: Switch all label selectors to use
  `internal.kro.run/` keys.
- **Cleanup**: Remove deprecated label constants from `pkg/metadata/labels.go`.

Regular (non-collection) resources are not affected: the controller discovers
them by name via direct GET (`processRegularNode`), not label selectors. SSA
re-apply stamps the new labels and drops the old ones on the next reconcile.

Collection resources that were never reconciled during the Phase 1 window are
partially affected. `listCollectionItems` uses label
selectors to discover existing items. If those items only carry `kro.run/`
labels, the list returns empty. The controller still re-applies all desired
items from the forEach expansion (SSA stamps the new labels), but the empty
list has two consequences:

1. **Lost change detection during apply**: `existingByKey` is empty, so every
   item's `current` is nil and the controller cannot compare revisions. It
   treats every apply as a create. In practice this is a no-op: SSA apply is
   idempotent, so the resource is patched to the same desired state. The
   observable cost is unnecessary API server writes and extra events, not
   incorrect behavior.
2. **Invisible to deletion**: in the deletion path, an
   empty list causes the node to be marked as already deleted
   so unreconciled collection items are skipped
   during instance teardown and left behind in the cluster.

The deletion case is the real orphaning risk. ApplySet pruning
(`applyset.kubernetes.io/part-of`) runs as part of every reconcile loop and
would still catch these items, but only if the instance is reconciled at least
once during Phase 2 before it is deleted. The true orphan scenario is narrow:
an instance must be deleted during Phase 2 without a single prior Phase 2
reconcile. Given the default resync period of 10 hours
(`--dynamic-controller-default-resync-period=36000`) and that the Phase 1
window spans at least one full release cycle, any resource that remains
unreconciled across that entire window is effectively orphaned already. This is
an acceptable trade-off for an alpha project, unless the user decides to adopt
them explicitly using [Resource Lifecycles](https://github.com/kubernetes-sigs/kro/issues/542).

### Assumptions

- **Natural reconciliation is sufficient.** The controller re-reconciles all
  instances on restart (full re-list from informer cache), and periodic resyncs
  (default: 10 hours, configurable via `--dynamic-controller-default-resync-period`)
  ensure all managed resources are touched within a release cycle. No active
  migration is required.
- **No external consumers depend on `kro.run/` labels today.** This is an alpha
  project. If external tooling does depend on these labels, the Phase 1 release
  notes and upgrading guide provide advance notice.

## Scope

### In Scope

- Migration of all controller-owned `kro.run/` labels to `internal.kro.run/`
- Dual-write implementation for labels in Phase 1
- Point lookups switch from `kro.run/` to `internal.kro.run/` in Phase 2
- Label validation to reserve both prefixes in RGD resource templates
- Removal of deprecated identifiers in Phase 2

### Not in Scope

- Changes to the `kro.run/v1alpha1` API group or CRD schema
- Changes to identifier keys or semantics beyond the prefix migration
- Active migration tooling (jobs, scripts, forced re-reconciliation)
- **User-set `kro.run/` identifiers.** The `kro.run/reconcile` (set by
  users on instance objects to pause reconciliation) and the
  `kro.run/allow-breaking-changes` annotations (set by users on RGDs to opt into
  breaking schema changes) are intentionally kept at `kro.run/`. These are
  user-facing identifiers that belong under the public API prefix, not internal
  implementation details.
- The `kro.run/finalizer` finalizer. Renaming a finalizer requires careful
  ordering (old finalizer must be removed before the new one is added, or the
  resource becomes undeletable). This has different migration mechanics and
  warrants its own proposal.
- Field manager names (`kro.run/applyset`, `kro.run/labeller`). Field managers
  are not user-visible metadata in the way labels are; they do not appear in
  label selectors, monitoring dashboards, or RBAC rules. Changing them affects
  SSA conflict detection, which requires separate analysis.

## Testing

### Phase 1 (v0.x.0)

- Unit tests verify all labeler constructors write both label sets
- Unit tests verify point lookups still read `kro.run/` keys
- Unit tests verify label selectors use deprecated keys
- Integration tests verify resources created by v0.x.0 carry both label sets
- Integration tests verify resources created by older versions (simulated with
  only `kro.run/` labels) are still discoverable via selectors

### Phase 2 (v0.x+1.0)

- Unit tests verify labelers only write `internal.kro.run/` labels
- Unit tests verify no references to deprecated label constants remain
- Integration tests verify end-to-end reconciliation with internal labels only

## Discussion

This migration is intentionally conservative. A single-release cutover would be
simpler but risks leaving collection items invisible to the deletion path in
clusters where reconciliation is slow or paused (see Phase 2 details above). The
two-release window gives operators a full release cycle to ensure all resources
are stamped with the new labels before the old ones are removed.

### Phase 2 as a Breaking Change

Phase 2 is explicitly a breaking change. Removing the `kro.run/` labels means:

- **Collection discovery breaks for unreconciled resources.** The instance
  controller's `listCollectionItems` uses kro label selectors to find forEach
  expansion items. If a resource still carries only `kro.run/` labels when
  selectors switch to `internal.kro.run/`, it will not appear in collection
  list results.
- **Point lookups switch keys.** All code paths reading kro labels (e.g.,
  `IsKROOwned`, `CompareRGDOwnership`, `findRGDsForCRD`) move from `kro.run/`
  to `internal.kro.run/` keys. Unreconciled resources return empty values.
- **External tooling that depends on `kro.run/` labels breaks.** Any label
  selectors, RBAC rules, monitoring dashboards, or external controllers matching
  on `kro.run/` labels will silently stop matching.

Note that **resource pruning is not affected** by this migration. kro uses
ApplySets for ownership tracking and pruning. The ApplySet
implementation selects on `applyset.kubernetes.io/part-of`, which is independent
of kro's label namespace. Pruning will continue to work correctly regardless of
which kro label prefix is present.

These are acceptable trade-offs for an alpha project, but they are real breakage
that must be signaled clearly.

### Coordinating with the API Version Bump

Phase 2 should ship as part of the API version transition from `v1alpha1` to
`v1beta1` (or whichever version follows). The reasons are:

1. **Kubernetes convention signals breakage through version boundaries.** Users
   expect that moving between API versions, especially from alpha to beta, may
   change internal behavior, deprecate fields, and drop backwards-compatibility
   shims. Coupling the label removal with a version bump puts the breakage where
   users are already looking for it.

2. **A single migration checkpoint is easier to reason about than many.** If
   Phase 2 lands in an arbitrary patch or minor release, operators must track
   which specific version removed the old labels. Tying it to the API version
   bump gives a clean before/after boundary: `v1alpha1` uses dual-write,
   `v1beta1` uses `internal.kro.run/` only.

3. **Upgrade guides naturally anchor to API versions.** Documentation for
   "upgrading from v1alpha1 to v1beta1" is a standard pattern in the Kubernetes
   ecosystem. This gives us a natural place to document the label removal,
   orphan risk, and any manual steps operators may need to take.

4. **It batches breaking changes.** The label migration is the first in a series
   of planned internal changes. By gating Phase 2 on the version bump, we can
   batch it alongside other breaking changes (e.g., schema changes, controller
   behavioral changes) rather than drip-feeding breakage across releases. Users
   upgrade once and absorb all changes together.

In practice, this means Phase 1 (dual-write) ships in the remaining `v1alpha1`
release(s), and Phase 2 (removal) ships in the first `v1beta1` release. The
Phase 1 window spans however many `v1alpha1` releases remain before the version
bump, giving operators the maximum possible window to reconcile all resources
with the new labels.
