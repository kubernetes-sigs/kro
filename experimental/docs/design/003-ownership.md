# Ownership

Every field in a Kubernetes resource can have many writers — HPAs scale replicas, admission
controllers set defaults, other operators manage their own fields. Resources exist before the Graph
touches them and may need to outlive it. An ownership model has to say who writes what, when
claims collide, and what happens to contested or abandoned fields at cleanup.

kro's model: nodes declare per-field ownership, dependencies order the operations, SSA enforces
exclusivity. Every field kro writes has exactly one kro writer. Include a field in a `template:`
or `patch:` and the Graph owns it — SSA tracks the claim, the API server enforces it with a 409
when claims collide. Omit a field and the Graph doesn't touch it. Claims wind forward in
dependency order, unwind in reverse.

## Field Manager

kro uses Kubernetes Server-Side Apply for all writes. Every Graph instance gets a dedicated SSA
field manager:

```
<name>.<namespace>.internal.kro.run
```

Applies default to non-force SSA. On 409, the Graph surfaces the conflict and stops reconciling
that resource — silent field takeover is a misconfiguration, not a feature.

A `template:` that specifies `replicas: 3` owns replicas. To delegate replicas to an HPA, omit the
field. Initial value and ongoing ownership are the same declaration.

## Semantics by Type

- **`template:`** — the Graph creates the resource if absent and claims the fields declared in
  the template. Other managers (HPAs, admission controllers) can own fields the template omits.
  On prune, the resource is deleted.
- **`patch:`** — the Graph claims the fields it wrote on a resource another actor manages. The
  target must exist — the node is Pending until it does. On prune, those fields are released; the
  resource itself is never deleted.
- **`ref:`** — read a resource outside this graph. No claim, nothing to clean up.
- **`watch:`** — observe a collection of resources. No claim, nothing to clean up.
- **`def:`** — computed values into scope. No Kubernetes resource.

### kro.run/apply

The `kro.run/apply` annotation on a `template:`'s metadata controls the SSA strategy:

- **Absent (default)** — non-force SSA. 409 on value conflicts with other managers.
- **`kro.run/apply: Force`** — force SSA. Takes contested fields.

Force is valid only on `template:`. Force takes contested fields *and* asserts the Graph as the
resource's identity owner — a combined move that only makes sense when the Graph is declaring the
whole resource. `patch:` nodes write specific fields without asserting identity; SSA 409 is the
correct signal when those fields collide.

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

### Identity Labels

Every resource written by a `template:` or `patch:` carries two labels per Graph-node pair:

    <node>.<graph>.<ns>.internal.kro.run/type       = template | patch
    <node>.<graph>.<ns>.internal.kro.run/generation = <graph.metadata.generation>

Stamped before every apply. Each Graph gets its own label key — multiple Graphs targeting the same
resource coexist without collision. `ref:`, `watch:`, and `def:` nodes stamp no labels — they do
not write to the cluster, so they have nothing to track.

The label value is the source of truth for a resource's prune action. On controller cold start the
current DAG may not contain the node that wrote the resource; the label decides whether cleanup
deletes the resource (`template`) or releases its fields (`patch` — see § Release Apply).

Before applying a `template:`, the controller checks for existing identity labels from other
Graphs. If present, the resource is managed by another kro Graph — the apply is rejected unless
`kro.run/apply: Force` is set. This catches accidental duplicates and makes kro-to-kro migration
explicit. The label check runs before SSA and rejects every kro-to-kro identity conflict — SSA
alone cannot, because it assigns silent co-ownership when two managers apply identical values. For
non-kro resources (no `*.internal.kro.run/type` label), the normal SSA flow applies.

The label check runs only for `template:`. Two `patch:` nodes on the same target — the
steady-state pattern — are allowed. When two `patch:` nodes write the same field, SSA 409 catches
the collision at the field layer.

### Status Subresource

The Kubernetes API server treats the main resource and status subresource as separate SSA endpoints
with independent managedFields — a single patch can't span both. When a node contains `.status`
fields, the controller splits the apply into two patches: metadata/spec via the main resource,
status via the status subresource. The identity label goes in the first patch — the resource is
tracked for cleanup even if the controller crashes before the second patch runs. On restart, the
reconcile loop re-applies the full node; SSA idempotency makes the completed patch a no-op and the
failed patch catches up. Both release variants target the main resource (full release clears the
identity label, partial release updates it); the status subresource release is additive when the
node wrote status fields.

### forEach

forEach expands into child nodes — each child manages one resource. The ownership model applies
per-child: each child's managed resource carries the child's identity label and follows the same
rules as any other node. The parent is a logical node with no managed resource and no ownership
semantics.

## Scenarios

**Coexistence.** The steady state. Multiple writers — Graphs, controllers, HPAs — each own disjoint
fields on the same resource. Each field manager owns its fields, no 409. A `patch:` on one Graph
writing status to a resource a `template:` on another Graph created is the standard pattern; so is
a Deployment where kro owns `spec.template` and an HPA owns `spec.replicas`.

**Conflict.** A claim collides. Two detection layers catch different collisions. Identity labels
catch kro-to-kro identity conflicts before SSA runs — a `template:` targeting a resource already
labeled by another Graph is rejected. SSA 409 catches every field-level collision: two non-kro
managers, a kro node and a non-kro manager, or two `patch:` nodes (same or different Graphs)
writing the same field. Both surface as error conditions on the Graph; reconciliation stops on
that resource. Resolution depends on the conflict: identity conflicts (between `template:` nodes)
clear by setting `kro.run/apply: Force` or removing one of the `template:` nodes; field conflicts
clear by removing the contested fields from one side (Force is not applicable to `patch:`).

**Migration.** Management transfers from one Graph to another. The importing side adds a
`template:` with `kro.run/apply: Force` and takes the fields. The exporting side's next reconcile
detects the takeover via the identity label check and errors — conflict state prevents accidental
deletion while ownership is contested. The user removes the node from the exporting side. Once
adopted, the importing side removes the Force annotation to return to cooperative non-force SSA.
Migration from a non-kro manager follows the same arc, except the exporting side detects via 409
rather than label check; if the new owner happens to apply identical values, shared ownership
persists silently until values diverge.

**Type change across revisions.** Swapping `template:` for `patch:` (or vice versa) on the same
node ID is a spec edit handled by revision supersession. The running resource is not reclassified
mid-flight; the next apply overwrites the identity label to match the new declaration. Two
invariants hold: a `patch:` → `template:` transition must not release fields the new `template:`
claims (the controller collapses the pair into a single `template:` reconcile, retiring the old
`patch:` entry without a release apply); a `template:` → `patch:` transition retains the resource,
releasing fields outside the new `patch:` body and re-claiming the fields inside it under the
`patch` label value.

## Release Apply

Releasing fields uses a release apply — an SSA apply from the Graph's field manager that omits the
fields being released. SSA interprets omitted fields as "no longer managed" and releases them.
Two variants:

- **Full release** — body contains only identity fields (`apiVersion`, `kind`, `metadata.name`,
  `metadata.namespace`). All previously-owned fields are released, including the identity label.
  Used when pruning a `patch:` node.
- **Partial release** — body contains the fields to retain. Fields the Graph previously owned but
  no longer declares are released; declared fields remain claimed; the identity label is updated.
  Used when a `template:` → `patch:` transition narrows the Graph's claim.

The release always targets the main resource; if the node wrote status fields, an additional
release targets the status subresource. If the release returns 404, the resource is already gone —
release succeeds. Other failures retry on the next reconcile.

## Blocked Deletion

Before deleting a `template:` resource, the controller checks managedFields for other field managers
(excluding the API server's own). If present, deletion is blocked — another actor depends on the
resource's existence. The condition message names the blocking manager. During prune, the resource
stays in the applied set until the other manager releases. During teardown, the Graph's finalizer
holds. Applied set tracking and teardown ordering are defined in
[004-graph-reconciliation](004-graph-reconciliation.md).

## Why Not

**Prune without delete.** Surprises the common case — removing a `template:` from the spec should
clean up its resource. `template:` deletes, `patch:` releases. Cleanup matches the declaration.

**OwnerReferences for managed resources.** Don't work across scopes — cluster-scoped resources and
cross-namespace references are common. Bind to UIDs that break on delete+recreate.

**managedFields inspection for delete decisions.** Introduces heuristics around substantive vs
administrative entries and stale managers. Breaks the clean rule: `template:` deletes, `patch:`
releases.
