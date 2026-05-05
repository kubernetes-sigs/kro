# Ownership

Every field in a Kubernetes resource can have many writers — HPAs scale replicas, admission
controllers set defaults, other operators manage their own fields. Resources exist before the Graph
touches them and may need to outlive it. An ownership model has to say who writes what, when
claims collide, and what happens to contested or abandoned fields at cleanup.

## Invariant

**Every field kro writes has exactly one owner.** Include a field in a `template:` or `patch:` and
the Graph owns it — solely. Omit a field and the Graph doesn't touch it. Claims wind forward in
dependency order, unwind in reverse.

Why sole ownership matters:

- **Deletion.** Template prune deletes the resource. If another Apply-type manager co-owns fields,
  the delete preflight blocks — the Graph's teardown is stuck until the co-owner releases. Sole
  ownership guarantees teardown can complete.
- **Release.** Patch prune releases fields. If kro is the sole owner, the released field is deleted
  from the live object (correct — kro is going away, the field should not persist ownerless). If a
  co-owner exists, the field persists under their ownership — surprising when the user believed
  their force-apply established exclusive control.
- **Migration.** A common pattern is: force-apply to take ownership, then relax to non-force once
  stable. If force-apply leaves co-ownership intact (because values agreed), relaxing to non-force
  inherits a latent conflict that surfaces as a surprise 409 when values eventually diverge.

Sole ownership eliminates all three failure modes. The mechanism differs by node type — templates
evict entirely, patches evict surgically — but the invariant is the same.

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

### lifecycle

The invariant is sole ownership, always. Two mechanisms enforce it:

**Detection (dry-run)** — On the non-force path, the controller dry-runs the SSA apply before
committing. The dry-run response reveals whether co-ownership would result. If so, the controller
surfaces Conflict — the real apply does not proceed. This catches most conflicts before the write,
but is not atomic with it.

**Enforcement (post-apply release)** — On all paths, after the real apply, the controller inspects
managedFields on the response. If any other Apply-type manager co-owns fields with kro, the
controller issues a release:

- Template nodes: eviction release (identity-only body impersonating co-owner — removes their
  entire entry and all solely-owned fields)
- Patch nodes: surgical release (body impersonating co-owner containing only their non-overlapping
  fields — preserves their other claims)

This handles two cases: (1) the force path, where agreed-value fields are not stripped by SSA, and
(2) the non-force path, where a race between dry-run and apply created co-ownership that the
dry-run did not predict. After the release, kro is the sole owner of every declared field.

`lifecycle.apply: Force` adds ForceOwnership to the SSA apply — taking contested fields where values
disagree (the server strips the other manager). Without Force, value disagreement surfaces as
Conflict (409 from the real apply). Force does not affect the post-apply release — both paths
enforce sole ownership identically.

```yaml
- id: deploy
  lifecycle:
    apply: Force
  template:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-app
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

Before applying a `template:`, the controller includes the identity label in the SSA payload. If
another Graph's identity label already exists on the resource, SSA detects the conflict on the label
field — the apply is rejected unless `lifecycle.apply: Force` is set. This makes ownership checking
atomic with the write — no separate read-then-check step. Identity labels catch
template-to-template conflicts atomically — label values are unique per Graph, so a second Graph's
apply always disagrees on the label field, producing a 409 on the real apply itself. Field-level
co-ownership (same values, different managers) is caught by post-apply managedFields inspection.

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
failed patch catches up. Release variants target the main resource; if the node wrote status
fields, an additional release targets the status subresource.

### forEach

forEach expands into child nodes — each child manages one resource. The ownership model applies
per-child: each child's managed resource carries the child's identity label and follows the same
rules as any other node. The parent is a logical node with no managed resource and no ownership
semantics.

## Scenarios

**Coexistence.** The steady state. Multiple writers — Graphs, controllers, HPAs — each own disjoint
fields on the same resource. Each field manager owns its fields, no 409. A `patch:` on one Graph
writing status to a resource that a `template:` on another Graph created is the standard pattern; so is
a Deployment where kro owns `spec.template` and an HPA owns `spec.replicas`.

**Conflict.** A claim collides. The dry-run detection step catches field-level co-ownership — two
managers writing the same field, whether values agree or disagree. Identity labels provide an
additional check for template-to-template conflicts — the label field itself is contested, surfacing
the conflict atomically with the SSA write. Both surface as Conflict on the Graph; reconciliation
stops on that resource. Resolution: set `lifecycle.apply: Force` to take ownership, or remove the
contested fields from one side.

**Migration.** Management transfers from one actor to another. The importing side adds a node with
`lifecycle.apply: Force`. Force SSA takes contested fields; the post-apply release strips the
co-owner's claim on agreed-value fields. After one reconcile, kro is the sole owner. The old
manager's next reconcile finds its fields taken — it surfaces an error or gives up. The user
removes the node from the exporting side. Once stable, the importing side can relax to non-force —
the state is clean, no latent co-ownership.

**Type change across revisions.** Swapping `template:` for `patch:` (or vice versa) on the same
node ID is a spec edit handled by revision supersession. The running resource is not reclassified
mid-flight; the next apply overwrites the identity label to match the new declaration. Two
invariants hold: a `patch:` → `template:` transition must not release fields the new `template:`
claims (the controller collapses the pair into a single `template:` reconcile, retiring the old
`patch:` entry without a release apply); a `template:` → `patch:` transition retains the resource,
releasing fields outside the new `patch:` body and re-claiming the fields inside it under the
`patch` label value.

## Release Apply

Releasing fields uses a release apply — an SSA apply that omits the fields being released. SSA
interprets omitted fields as "no longer managed" and releases them. Four variants:

- **Full release** — body contains only identity fields (`apiVersion`, `kind`, `metadata.name`,
  `metadata.namespace`). All previously-owned fields are released, including the identity labels.
  Used when pruning a `patch:` node.
- **Partial release** — body contains the fields to retain. Fields the Graph previously owned but
  no longer declares are released; declared fields remain claimed; the identity label is updated.
  Used when a `template:` → `patch:` transition narrows the Graph's claim.
- **Eviction release** — a full release issued under a *different* field manager's identity to
  evict that manager from the resource. Fields the evicted manager solely owned are deleted from
  the object; fields kro also claims persist under kro's sole ownership. The evicted manager's `managedFields` entry
  is garbage-collected. Used after force-apply on `template:` nodes.
- **Surgical release** — an apply issued under a co-owner's identity containing only their
  non-overlapping fields. The co-owner's claim on overlapping fields is released; their claim on
  non-overlapping fields is preserved; the overlapping field values persist under kro's sole
  ownership. Used after force-apply on `patch:` nodes when co-ownership is detected.

SSA field manager names are unauthenticated strings — any client with write access can apply under
any manager identity. This enables both eviction and surgical release.

The release always targets the main resource; if the node wrote status fields, an additional
release targets the status subresource. If the release returns 404, the resource is already gone —
release succeeds. Other failures retry on the next reconcile.

## Blocked Deletion

Before deleting a `template:` resource, the controller checks managedFields for other field managers
(excluding the API server's own). If present, deletion is blocked — another actor depends on the
resource's existence. The condition message names the blocking manager. During prune, the resource
stays in the applied set until the other manager releases. During teardown, the Graph's finalizer
holds. Applied set tracking and teardown ordering are defined in
[005-reconciliation](005-reconciliation.md). For force-managed templates, the post-apply release
eliminates co-owners before deletion is attempted — so this block only fires for non-force templates
where an external SSA manager appeared independently.

## Why Not

**Prune without delete.** Surprises the common case — removing a `template:` from the spec should
clean up its resource. `template:` deletes, `patch:` releases. Cleanup matches the declaration.

**OwnerReferences for managed resources.** Don't work across scopes — cluster-scoped resources and
cross-namespace references are common. Bind to UIDs that break on delete+recreate.

**managedFields inspection for delete decisions.** Introduces heuristics around substantive vs
administrative entries and stale managers. Breaks the clean rule: `template:` deletes, `patch:`
releases.

**Co-ownership as steady state.** Two managers writing the same field with the same value creates
SSA co-ownership silently (no 409). This is a timebomb: values agree today, diverge tomorrow, and
the 409 surprises the operator. Worse, co-ownership blocks template deletion (third-party
managedFields entry persists) and causes released fields to persist unexpectedly (co-owner retains
them). Sole ownership, enforced by post-apply release, eliminates the class of failure.

**Always force on patches.** Eliminates 409s entirely, but also eliminates the diagnostic signal.
The 409 tells the operator "something else writes this field — is that intentional?" Force is an
opt-in declaration of intent, not a default behavior.
