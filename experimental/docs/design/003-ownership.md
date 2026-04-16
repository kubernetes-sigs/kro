# Ownership

Every field in a Kubernetes resource can have many writers — HPAs scale replicas, admission
controllers set defaults, other operators manage their own fields. Resources exist before the Graph
touches them and may need to outlive it. An ownership model has to say who writes what, when
claims collide, and what happens to contested or abandoned fields at cleanup.

kro's model: templates declare per-field ownership, dependencies order the operations, SSA enforces
exclusivity. Every field has exactly one writer. Include a field in a template and the Graph owns
it — SSA tracks the claim, the API server enforces it with a 409 when claims collide. Omit a field
and the Graph doesn't touch it. Claims wind forward in dependency order, unwind in reverse.

## Field Manager

kro uses Kubernetes Server-Side Apply for all writes. Every Graph instance gets a dedicated SSA field
manager:

```
<name>.<namespace>.internal.kro.run
```

Applies default to non-force SSA. On 409, the Graph surfaces the conflict and stops reconciling that
resource — silent field takeover is a misconfiguration, not a feature.

A Deployment template that specifies `replicas: 3` owns replicas. To delegate replicas to an HPA,
omit the field. Initial value and ongoing ownership are the same declaration.

## Reference Types

The reference type determines what the Graph does with a resource — creation, reads, field
contributions, or cleanup. Four types (defined in [001-graph](001-graph.md)):

- **Own** — creates the resource if absent. Applied via SSA. Tracked for cleanup. Deleted on prune.
- **Watch** — read-only GET. Not tracked. Pending if absent.
- **WatchKind** — read-only list by kind and optional selector. Not tracked.
- **Contribute** — writes fields on a resource the Graph does not create. Applied via SSA. Tracked
  for cleanup. Fields released on prune — resource never deleted. Pending if absent.

### kro.run/apply

The `kro.run/apply` annotation on the template's metadata controls the SSA strategy:

- **Absent (default)** — non-force SSA. 409 on value conflicts with other managers.
- **`kro.run/apply: Force`** — force SSA. Takes contested fields. Also bypasses the existence check
  during reference type detection — the resource is classified as Own regardless of whether it
  already exists.

Force always implies Own semantics — force-writing fields on a resource without taking full
ownership (Contribute + Force) is not supported.

The annotation flows to the managed resource — anyone inspecting the cluster sees the apply strategy.

```yaml
- id: migrated
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: existing-app
      annotations:
        kro.run/apply: Force
    spec:
      replicas: 3
```

### Detection

Detection has two phases. Template structure determines Watch and WatchKind at compile time.
Resource existence determines Own vs Contribute at first reconcile. A new revision re-evaluates.

1. **WatchKind** — no `metadata.name`.
2. **Watch** — identity-only fields (`apiVersion`, `kind`, `metadata.name`, optionally
   `metadata.namespace`). No other fields.
3. **Own** — resource absent, or `kro.run/apply: Force` is set. The Graph creates or adopts it.
4. **Contribute** — resource exists, no Force. The Graph did not create it.

### Identity Labels

Every managed resource carries an identity label:

    <node>.<graph>.<ns>.internal.kro.run/reference = own | contribute

Stamped before every apply. Each Graph gets its own label key — multiple Graphs targeting the
same resource coexist without collision. Before applying an Own template, the controller checks for
existing identity labels from other Graphs. If present, the resource is managed by another kro
Graph — the apply is rejected unless `kro.run/apply: Force` is set.

This catches accidental duplicates and makes kro-to-kro migration explicit. SSA's shared-ownership
blind spot (same values produce co-ownership, no 409) does not apply between kro Graphs — the label
check runs before SSA. For non-kro resources (no `*.internal.kro.run/reference` label), the normal
SSA flow applies.

The label check does not run for Contribute templates — another Graph's ownership label is expected,
not a conflict.

### Status Subresource

The Kubernetes API server treats the main resource and status subresource as separate SSA endpoints
with independent managedFields — a single patch can't span both. When a template contains `.status`
fields, the controller splits the apply into two patches: metadata/spec via the main resource,
status via the status subresource. The identity label goes in the first patch — the resource is
tracked for cleanup even if the controller crashes before the second patch runs. On restart, the
reconcile loop re-applies the full template; SSA idempotency makes the completed patch a no-op and
the failed patch catches up. Releases always include the main resource (clearing the identity
label); the status subresource release is additive when the template wrote status fields.

### forEach

forEach expands into child nodes — each child manages one resource. The ownership model applies
per-child: each child's managed resource carries the child's identity label and follows the same
Own/Contribute rules as any other node. The parent is a logical node with no managed resource and
no ownership semantics.

## Scenarios

**Coexistence.** The steady state. Multiple writers — Graphs, controllers, HPAs — each own disjoint
fields on the same resource. Each field manager owns its fields, no 409. A Contribute on one Graph
writing status to a resource Own-ed by another is the standard pattern; so is a Deployment where
kro owns `spec.template` and an HPA owns `spec.replicas`.

**Conflict.** A claim collides. Two detection layers catch different collisions. Identity labels
catch kro-to-kro conflicts before SSA runs — an Own template targeting a resource already labeled by
another Graph is rejected. SSA 409 catches conflicts with non-kro actors — two managers trying to
set the same field to different values. Both surface as error conditions on the Graph; reconciliation
stops on that resource. Resolution is a template change: remove the contested fields, or set
`kro.run/apply: Force` to take them. Either clears the conflict and triggers a re-apply.

**Migration.** Ownership transfers from one Graph to another. The importing side adds an Own template
with `kro.run/apply: Force` and takes the fields. The exporting side's next reconcile detects the
takeover via the identity label check and errors — conflict state prevents accidental deletion while
ownership is contested. The user removes the template from the exporting side. Once adopted, the
importing side removes the Force annotation to return to cooperative non-force SSA. Migration from a
non-kro manager follows the same arc, except the exporting side detects via 409 rather than label
check; if the new owner happens to apply identical values, shared ownership persists silently until
values diverge.

## Release Apply

Releasing fields uses a release apply — an SSA apply containing only identity fields (`apiVersion`,
`kind`, `metadata.name`, `metadata.namespace`). SSA interprets omitted fields as "no longer managed"
and releases them, including the identity label. The release always targets the main resource; if
the template wrote status fields, an additional release targets the status subresource. If the
release returns 404, the resource is already gone — release succeeds. Other failures retry on the
next reconcile.

## Blocked Deletion

Before deleting an Own resource, the controller checks managedFields for other field managers
(excluding the API server's own). If present, deletion is blocked — another actor depends on the
resource's existence. The condition message names the blocking manager. During prune, the resource
stays in the applied set until the other manager releases. During teardown, the Graph's finalizer
holds. Applied set tracking and teardown ordering are defined in
[004-graph-reconciliation](004-graph-reconciliation.md).

## Why Not

**Prune without delete.** Surprises the common case — removing a Deployment from the spec should
clean it up. Own deletes, Contribute releases. Cleanup matches the declaration.

**OwnerReferences for managed resources.** Don't work across scopes — cluster-scoped resources and
cross-namespace references are common. Bind to UIDs that break on delete+recreate.

**managedFields inspection for delete decisions.** Introduces heuristics around substantive vs
administrative entries and stale managers. Breaks the clean rule: owners delete, contributors
release.
