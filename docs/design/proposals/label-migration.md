# KREP: Internal Label Migration

## Summary

kro applies a set of labels (prefixed with `kro.run/`) to every resource it
manages. These labels are controller implementation details used for ownership
tracking, resource reconciliation, and collection management. Because they share
the `kro.run/` API group domain, they appear to be part of kro's public API
contract. This proposal migrates all controller-owned labels to the
`internal.kro.run/` prefix and establishes a two-release deprecation window for
the old labels.

This is the first in a series of planned breaking changes to kro's internals.
The label migration is intentionally scoped as a low-risk starting point to
establish the pattern for communicating and phasing breaking changes.

## Problem Statement

kro stamps labels on every resource it creates. These labels serve internal
purposes:

- **Ownership tracking**: identifying which RGD and instance own a resource
- **Reconciliation**: selecting resources during list operations, detecting
  conflicts, and managing lifecycle
- **Collection membership**: tracking position and size within forEach
  expansions

All of these labels live under the `kro.run/` prefix — the same domain used for
kro's API group (`kro.run/v1alpha1`). This creates two problems:

1. **Users reasonably assume `kro.run/` labels are stable API.** If a label
   shares the API group prefix, it looks like a public contract. Users or
   tooling that depend on these labels (e.g., label selectors in monitoring,
   RBAC rules, or external controllers) would break without warning if kro
   changes them.

2. **kro cannot evolve its internals freely.** Any change to label keys, values,
   or semantics becomes a de facto breaking change.

## Proposal

### Overview

Migrate all controller-owned labels from the `kro.run/` prefix to
`internal.kro.run/`. The migration uses a two-release phased approach that
relies on natural reconciliation to propagate changes, requiring no manual
intervention from users.

### Labels in Scope

All labels currently under `kro.run/` that are set by the controller:

| Deprecated (`kro.run/`)                       | New (`internal.kro.run/`)                              |
| --------------------------------------------- | ------------------------------------------------------ |
| `kro.run/node-id`                             | `internal.kro.run/node-id`                             |
| `kro.run/collection-index`                    | `internal.kro.run/collection-index`                    |
| `kro.run/collection-size`                     | `internal.kro.run/collection-size`                     |
| `kro.run/owned`                               | `internal.kro.run/owned`                               |
| `kro.run/kro-version`                         | `internal.kro.run/kro-version`                         |
| `kro.run/instance-id`                         | `internal.kro.run/instance-id`                         |
| `kro.run/instance-name`                       | `internal.kro.run/instance-name`                       |
| `kro.run/instance-namespace`                  | `internal.kro.run/instance-namespace`                  |
| `kro.run/instance-group`                      | `internal.kro.run/instance-group`                      |
| `kro.run/instance-version`                    | `internal.kro.run/instance-version`                    |
| `kro.run/instance-kind`                       | `internal.kro.run/instance-kind`                       |
| `kro.run/reconcile`                           | `internal.kro.run/reconcile`                           |
| `kro.run/resource-graph-definition-id`        | `internal.kro.run/resource-graph-definition-id`        |
| `kro.run/resource-graph-definition-name`      | `internal.kro.run/resource-graph-definition-name`      |
| `kro.run/resource-graph-definition-namespace` | `internal.kro.run/resource-graph-definition-namespace` |
| `kro.run/resource-graph-definition-version`   | `internal.kro.run/resource-graph-definition-version`   |

### Phase 1: Dual-Write (v0.x.0)

**Goal**: Every managed resource carries both label sets. No behavioral change.

- **Writes**: All labeler constructors (`NewInstanceLabeler`,
  `NewResourceGraphDefinitionLabeler`, `NewKROMetaLabeler`) write both the deprecated `kro.run/` label and the
  corresponding `internal.kro.run/` label.
- **Reads (point lookups)**: Use `LabelWithFallback(labels, internalKey,
deprecatedKey)` — prefers the internal key, falls back to deprecated. This is
  safe for individual object reads because both keys resolve to the same value
  once the resource has been reconciled.
- **Reads (label selectors)**: All server-side label selectors (e.g., `List`
  with `LabelSelector`) **must use the deprecated `kro.run/` keys**. Label
  selectors have no fallback mechanism — they match or they don't. Currently
  Resources only carry `kro.run/` labels, so selecting on
  `internal.kro.run/` would silently miss them.
- **Validation**: Both `kro.run/` and `internal.kro.run/` prefixes are reserved.
  Users cannot set labels with either prefix in RGD resource templates.

During this phase, natural reconciliation progressively stamps the new
`internal.kro.run/` labels onto all existing resources. No forced
re-reconciliation or migration job is needed — the normal controller loop
handles it.

### Phase 2: Cutover (v0.x+1.0)

**Goal**: Remove the deprecated labels entirely.

- **Writes**: Stop writing `kro.run/` labels. Only write `internal.kro.run/`.
- **Reads (point lookups)**: Remove `LabelWithFallback`. Read
  `internal.kro.run/` keys directly.
- **Reads (label selectors)**: Switch all label selectors to use
  `internal.kro.run/` keys.
- **Cleanup**: Remove deprecated label constants from `pkg/metadata/labels.go`.
  Remove the `LabelWithFallback` helper.

Resources that were never reconciled during the Phase 1 window will lose their
label-based association with kro. This is an acceptable trade-off: if a resource
was not reconciled across an entire release cycle, it is effectively orphaned,
unless the user decides to adopt them explicitly using Resource Lifecycles
https://github.com/kubernetes-sigs/kro/issues/542

### Assumptions

- **Natural reconciliation is sufficient.** The controller re-reconciles all
  instances on restart (full re-list from informer cache), and periodic resyncs
  ensure all managed resources are touched within a release cycle. No active
  migration is required.
- **No external consumers depend on `kro.run/` labels today.** This is an alpha
  project. If external tooling does depend on these labels, the Phase 1 release
  notes and upgrading guide provide advance notice.

## Scope

### In Scope

- Migration of all controller-owned labels from `kro.run/` to
  `internal.kro.run/`
- Dual-write implementation in Phase 1
- Fallback read logic for the transition period
- Label validation to reserve both prefixes
- Removal of deprecated labels in Phase 2

### Not in Scope

- Changes to the `kro.run/v1alpha1` API group or CRD schema
- Changes to label keys or semantics beyond the prefix migration
- Active migration tooling (jobs, scripts, forced re-reconciliation)

## Testing

### Phase 1 (v0.x.0)

- Unit tests verify all labeler constructors write both label sets
- Unit tests verify `LabelWithFallback` prefers internal, falls back to
  deprecated
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
simpler but risks silently orphaning resources in clusters where reconciliation
is slow or paused. The two-release window gives operators a full release cycle to
ensure all resources are stamped with the new labels before the old ones are
removed.
