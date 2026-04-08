# Ownership

The Graph isn't the only thing writing to the cluster. HPAs scale replicas. Multiple systems write
to pod and node status. Resources exist before the Graph touches them and may need to outlive it.

## Server-Side Apply

Kubernetes SSA tracks per-field ownership via managedFields. Every SSA apply includes a field manager
identity string. The API server records which fields each manager owns. The key semantics:

- **Non-force apply**: if the apply would change a field owned by another manager, the API server
  returns a 409. Same-value applies grant shared ownership — no conflict.
- **Force apply**: takes ownership of contested fields, removing them from other managers. Always
  succeeds (for ownership conflicts).
- **Shared ownership**: two managers applying the same value to the same field co-own it. The 409
  only fires when values disagree.
- **Field release**: an apply that omits a previously managed field releases it from that manager.

The Kubernetes docs recommend controllers always use force SSA. kro does not — kro defaults to
non-force because it operates in a multi-actor environment where silent field takeover is a
misconfiguration, not a feature.

## kro's Model

The model assumes one writer per field. Include a field in a template and the Graph owns it. Omit it
and the Graph doesn't. The API server's managedFields is the ownership registry. No additional
bookkeeping.

Every Graph instance uses a dedicated SSA field manager:

```
kro.run/<namespace>/<name>
```

By default, kro applies use non-force SSA. A 409 means another manager owns a field the Graph is
trying to set to a different value — the Graph surfaces the conflict and stops reconciling that
resource.

A template change (removing contested fields, changing values) clears the conflict state and
triggers a new apply attempt.

A Deployment template that specifies `replicas: 3` owns replicas. To delegate replicas to an HPA,
omit the field. Initial value and ongoing ownership are the same declaration.

### Template Shapes

Four shapes, determined by template content:

- **Owns** — specifies fields beyond identity. Creates the resource if absent. Applied via SSA.
  Tracked for cleanup. Deletes the resource on prune.
- **Watch** — specifies only identity. Read-only GET. Not tracked. Pending if absent.
- **Collection Watch** — specifies a kind with optional selector, no name. Read-only list. Not
  tracked.
- **Contribute** — specifies fields on a resource the Graph does not create. Applied via SSA.
  Tracked for cleanup. Releases fields on prune, never deletes. Pending if target absent.

Identity-only templates are Watch — always. A template that specifies anything beyond identity
(`apiVersion`, `kind`, `metadata.name`, `metadata.namespace`) is an Owns or Contribute. For
resources with no spec (like Namespace), owning means specifying at least one field beyond
identity — typically a label.

### kro.run/apply

The `kro.run/apply` annotation on the template's metadata controls the SSA strategy:

- **Absent (default)** — non-force SSA. 409 on value conflicts with other managers.
- **`kro.run/apply: Force`** — force SSA. Takes contested fields silently.

The annotation flows to the managed resource — anyone inspecting the cluster sees the apply strategy.

```yaml
# Force — take ownership from another manager
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

### kro Label Check

Every managed resource carries an `internal.kro.run/graph-name` label (injected during
materialization). Before applying an Owns template, the controller checks this label on the existing
resource. If the label is present and doesn't match this Graph's name, the resource is owned by
another kro Graph. The controller requires `kro.run/apply: Force` to proceed — without it, the apply
is rejected before SSA is attempted.

This catches accidental duplicates (same resource in two Graphs without Force) and makes kro-to-kro
migration explicit. SSA's shared-ownership blind spot (same values, no 409) doesn't apply between
kro Graphs because the label check runs before SSA. For non-kro resources (no
`internal.kro.run/graph-name` label), the normal SSA flow applies.

The label check does not run for Contribute templates. Contribute targets someone else's resource by
design — the label indicating another Graph's ownership is expected, not a conflict.

```yaml
resources:
  # Owns — default non-force SSA
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: my-app
      spec:
        replicas: 3

  # Watch — read into scope, no apply
  - id: webapp
    template:
      apiVersion: kro.run/v1alpha1
      kind: WebApp
      metadata:
        name: my-app

  # Contribute — writes status fields to another Graph's resource
  - id: webappStatus
    template:
      apiVersion: kro.run/v1alpha1
      kind: WebApp
      metadata:
        name: ${webapp.metadata.name}
        namespace: ${webapp.metadata.namespace}
      status:
        deploymentReady: ${deployment.status.availableReplicas == deployment.spec.replicas}
```

### Status Subresource

When a template contains `.status` fields, the controller splits the apply into two operations —
metadata/spec via the main resource, status via the status subresource. Each subresource has its own
managedFields entries. Releases only target the subresources the template actually applied to — a
status-only Contribute releases only the status subresource.

### forEach

forEach stamps its template once per item. Each stamped resource follows the same rules. The
ownership model applies per-stamp.

## Actions

The rows below represent the apply sequence for Owns and Contribute templates. Watch and Collection
Watch are read-only — only the Resource absent and Apply rows apply to them.

| Action | Owns | Contribute | Watch | Collection Watch |
|--------|------|------------|-------|------------------|
| **Resource absent** | Create | Pending | Pending | Empty array |
| **Label check** | Reject if another kro Graph (unless Force) | — | — | — |
| **Apply (default)** | Non-force SSA | Non-force SSA | GET | List |
| **Apply (Force)** | Force SSA | Force SSA | — | — |
| **Apply — 409** | Error, stop reconciling | Error, stop reconciling | — | — |
| **Template change** | Clear conflict, re-apply | Clear conflict, re-apply | — | — |
| **Hash match** | Skip apply | Skip apply | — | — |
| **Prune** | Delete resource | Release fields (skeleton apply) | No action | No action |
| **Prune — conflict** | Clear tracking, no delete | Clear tracking, no release | — | — |
| **Teardown** | Delete resource | Release fields (skeleton apply) | No action | No action |
| **Teardown — conflict** | Skip | Skip | — | — |

## What Falls Out

**Conflict detection.** Two layers. The kro label check catches kro-to-kro ownership conflicts
before SSA runs — explicit, immediate, with a clear error message. SSA 409 catches conflicts with
non-kro actors after the apply — the API server's enforcement. Both surface as error conditions on
the Graph's status. Both are blocking. A template change clears the conflict and triggers a
re-apply.

**Import.** Add an Owns template with `kro.run/apply: Force`. The Graph force-applies, taking
ownership from whatever manager previously held the fields. Once adopted, remove the annotation to
return to cooperative non-force SSA.

**Export.** The new owner force-applies, taking the fields. For kro-to-kro export, the label check
on the exporting Graph's next reconcile catches the ownership change immediately. For non-kro
export, the Graph's next reconcile gets a 409 when the applied values differ. If the new owner
applies identical values, shared ownership persists — the export requires value differences to
trigger the 409. The user removes the template. Conflict state prevents deletion.

**Migration.** One mechanism for all cases: the importing side adds `kro.run/apply: Force` and takes
the fields. The exporting side detects the change (label check for kro-to-kro, 409 for non-kro) and
errors. The user removes the template from the exporting side. Conflict state prevents deletion.

**Multi-graph coexistence.** Two Graphs can manage different fields on the same resource. Each
Graph's field manager owns its fields. No 409 because the fields are disjoint. A Contribute template
on one Graph and an Owns template on another is the standard pattern.

**Shared ownership.** Two non-kro managers applying the same value to the same field silently co-own
it. The 409 fires when values diverge. Between kro Graphs, the label check prevents this — the
second Graph must use Force.

## Mechanics

### Tracking

The controller tracks every resource it has applied to — the cleanup index. On prune or teardown,
the controller iterates this set to know which resources need cleanup and how (delete for Owns,
release for Contribute). The tracking index records per resource: the template shape, conflict state,
and which subresources were applied to.

Template hash annotations on resources provide change detection. Hash match → skip the apply. This
is a performance optimization — the controller converges on spec change, not continuously.

### Skeleton Apply

Releasing fields uses a skeleton apply — identity-only fields (`apiVersion`, `kind`,
`metadata.name`, `metadata.namespace`). SSA interprets omitted fields as "no longer managed" and
releases them from this manager. The skeleton apply only targets the subresources the template
actually applied to: main resource, status subresource, or both.

### Teardown

Teardown unwinds in reverse dependency order. Dependents are cleaned up before the resources they
depend on. Resources in conflict state are skipped — the Graph no longer holds the fields. If the
active revision is unavailable during teardown (e.g., manually deleted), the controller falls back
to unordered cleanup using the tracking index.

### Finalizer and Recovery

The Graph carries a finalizer that prevents API server removal until teardown completes. State
needed for teardown is persisted: the resource tracking index (including template shape, conflict
state, and subresource set per resource) and template hashes on each resource for idempotent
re-entry.

Controller crash mid-teardown: the finalizer prevents Graph removal. Next reconcile re-enters
teardown. Deleting an already-deleted resource returns 404 — remove from tracking and move on.

## Why Not

**ExternalRef as a separate field.** A watch template achieves the same result. One field type
instead of two.

**Explicit contribution flag.** SSA field managers track ownership per-field. No boolean needed.

**Force SSA everywhere.** Silent field takeover is a misconfiguration in a multi-actor environment.
Non-force SSA makes conflicts visible. `kro.run/apply: Force` is the opt-in.

**OpenAPI schema comparison for shape detection.** Requires schema discovery. Many CRDs have no
required fields. Per-field ownership via managedFields is more precise.

**Runtime existence detection.** Makes ownership a function of timing.

**Prune without delete.** Surprises the common case — removing a Deployment from the spec should
clean it up. Owns deletes, Contribute releases.

**OwnerReferences for managed resources.** Don't work across scopes. Bind to UIDs that break on
delete+recreate.

**managedFields inspection for delete decisions.** Introduces heuristics (substantive entries, stale
managers) and breaks the rule: owners delete, contributors release.
