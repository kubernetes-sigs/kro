# Managed Resources Status via Graph Field

## Problem statement

When Kro reconciles a ResourceGraphDefinition (RGD) instance, it needs to
track its own view of the resource graph: which managed resources exist, their
topological ordering, dependency relationships, and the current reconciliation
state. Today this data lives in the `status` block of the *RGD*
(`ResourceGraphDefinitionStatus.TopologicalOrder` and `.Resources`); instances
record none of it. That creates two problems.

**Wrong object, and API surface pollution.** An RGD is a template. The graph is
a property of each instance, which may differ per instance through collection
expansions, `includeWhen` outcomes, and adoption. On top of that,
`ResourceGraphDefinitionStatus` is a published `v1alpha1` Go type, so evolving
Kro's bookkeeping means changing a versioned API that users read.

**Scalability.** Whatever replaces this must not grow with the number or size of
the managed resources — for graphs with many nodes, or with collection
expansions, that becomes an etcd size problem. The only safe data to store is a
compact identity reference, not full resource content.

## Proposal

Introduce a dedicated `graph` field at the top level of every kro-generated
Instance CRD, sitting alongside `spec` and `status`. The `graph` field is
written exclusively by Kro using its own field manager, keeping it
operationally separate from the user-facing `status` (which Kro also writes,
including the RGD-projected fields). It contains only the internal bookkeeping
data Kro needs to reconcile the instance.

The `graph` field stores a compact identity reference
(`ManagedGraphResourceRef`) for each managed resource rather than the full
resource object. Consumers who need live resource data look it up directly
using the identity fields.

#### graph as a Kubernetes subresource

Kubernetes CRDs natively support two subresource types: `status` and `scale`.
When a CRD declares `subresources: status`, the API server splits the resource
into two distinct REST endpoints:

```
/apis/<group>/<version>/<plural>/<name>         # main object (spec + metadata)
/apis/<group>/<version>/<plural>/<name>/status  # status subresource
```

A write to `/status` only updates `status`; the spec is left untouched. This
separation exists so that controllers can update status without accidentally
racing with user writes to spec, and so RBAC can grant status-write permission
independently of spec-write permission.

Ideally `graph` would work the same way — a separate REST endpoint that Kro
updates independently, with its own RBAC verb (`update` on
`myapps/graph`). However, the `apiextensions.k8s.io` CRD API only supports
`status` and `scale` as named subresource slots; [**arbitrary named subresources
on CRDs are not possible without an aggregated API server
** (AA)](https://github.com/kubernetes/kubernetes/issues/72637). An AA is a
significant operational burden and out of scope here.

The approach taken in this proposal is to emulate subresource semantics using
[server-side apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/)
field ownership:

- `graph` is declared as a top-level field in the CRD OpenAPI schema, alongside
  `spec` and `status`.
- Kro applies the `graph` field using a dedicated field manager
  (`kro-graph-manager`). `graph.resources` is `x-kubernetes-list-type: atomic`,
  so ownership is recorded for the list as a unit (see Discussion and notes).
- No other actor — user, admission webhook, or GitOps controller — should claim
  ownership of `graph` fields. This is not machine-enforced: SSA returns a
  conflict only for a non-forced *apply* by another manager, while an `Update`,
  a merge patch, or `--force-conflicts` take the field silently. Kro rewrites
  `graph` on the next reconcile, so the failure mode is a transient wrong value.
- From a RBAC perspective, `graph` cannot be isolated at the verb level today
  (since there is no `/graph` subresource path). Access is controlled at the
  resource level. Operators who want to read graph data but not modify the
  instance can be granted `get`/`list`/`watch` on the instance resource; Kro
  itself needs `update` (or `patch`) on the instance to write `graph`.

This is the same pattern used by tools such as cert-manager (which applies
`status` fields on generated `Secret` objects) and Argo CD (which writes
`metadata.annotations` under its own field manager without touching user
annotations). It is a well-understood, production-proven approach that does not
require infrastructure beyond what Kro already uses.

**GitOps.** Because `graph` is in the object body, `kubectl get -o yaml`
displays it, so an instance checked into git has a `graph` block and the
GitOps controller either reports permanent drift or fights Kro for the field.
Argo CD's fix is `ignoreDifferences.managedFieldsManagers:
[kro-graph-manager]`; Flux's is `spec.driftDetection.ignore` with
`fromFieldPath: graph`. Both must be documented user-facing, not only here. Kro
never treats a foreign write to `graph` as an error, so a misconfigured setup
degrades to churn rather than a broken instance.

If the Kubernetes ecosystem adds support for arbitrary named CRD subresources in
the future, migrating `graph` to a true subresource would be a backwards-
compatible change: the field would move from the main object body to its own
endpoint, and existing readers could be updated incrementally.

#### Overview

- Add a `graph` field to the schema of every kro-generated Instance CRD.
- Kro writes `graph` with its own field manager (`kro-graph-manager`), never
  touching `status` for bookkeeping data.
- `graph.resources` holds only identity fields per managed resource — no spec,
  no status mirroring. it is efficient also for large collections.

#### Design details

##### graph field schema

The `graph` field added to every Instance CRD version:

```yaml
graph:
  type: object
  x-kubernetes-preserve-unknown-fields: false
  properties:
    state:
      type: string
      description: >
        Kro's view of the reconciliation state for this instance.
        Mirrors status.state so that consumers of the graph field do not
        need to cross-reference the status block.
    observedGeneration:
      type: integer
      format: int64
      description: >
        The instance metadata.generation this graph was computed from. graph
        and status are written by two separate calls, so consumers use this to
        detect a graph that has not caught up yet.
    resources:
      type: array
      # Atomic: Kro is the only writer, so per-element ownership buys nothing
      # and would add one metadata.managedFields entry per node.
      x-kubernetes-list-type: atomic
      description: Identity references to all managed resources in the graph.
      items:
        type: object
        required: [ id, nodeType, apiVersion, kind ]
        properties:
          id:
            type: string
            description: The resource ID as defined in the RGD.
          nodeType:
            type: string
            enum: [ Resource, External, Collection, ExternalCollection ]
            description: >
              Which kind of graph node this entry represents, so consumers can
              switch on it rather than inferring from the presence of selector,
              and can filter out nodes Kro references but does not own.
          apiVersion:
            type: string
            description: >
              Group/version of the managed resource, in the form used by
              ownerReferences ("apps/v1", or "v1" for the core group).
          kind:
            type: string
            description: Kind of the managed resource.
          namespace:
            type: string
            description: Namespace of the managed resource (empty for cluster-scoped).
          name:
            type: string
            description: >
              Name of the managed resource. Set for scalar resources;
              absent for collection nodes (use selector instead).
          uid:
            type: string
            description: >
              UID of the managed resource. Set for scalar resources;
              absent for collection nodes.
          revision:
            type: integer
            description: >
              The RGD generation under which this resource's desired state was
              last computed. Allows detecting which nodes are stale during a
              rolling graph update.
          selector:
            type: object
            x-kubernetes-map-type: atomic   # matches upstream metav1.LabelSelector
            description: >
              Label selector (metav1.LabelSelector) identifying all members of a
              collection node. Set when nodeType is Collection or
              ExternalCollection; absent for scalar resources.
            properties:
              matchLabels:
                type: object
                additionalProperties:
                  type: string
              matchExpressions:
                type: array
                x-kubernetes-list-type: atomic
                items:
                  type: object
                  required: [ key, operator ]
                  properties:
                    key:
                      type: string
                    operator:
                      type: string
                    values:
                      type: array
                      items:
                        type: string
```

##### ManagedGraphResourceRef Go type

A new `ManagedGraphResourceRef` type in `api/v1alpha1/`:

```go

package v1alpha1

// GraphNodeType mirrors the internal graph.NodeType values reachable from an
// instance. NodeTypeInstance is never emitted.
type GraphNodeType string

const (
	GraphNodeTypeResource           GraphNodeType = "Resource"
	GraphNodeTypeExternal           GraphNodeType = "External"
	GraphNodeTypeCollection         GraphNodeType = "Collection"
	GraphNodeTypeExternalCollection GraphNodeType = "ExternalCollection"
)

// ManagedGraphResourceRef is a compact identity reference to a managed Kubernetes
// resource. For scalar resources it identifies the specific object; for
// collection (forEach) nodes it carries a label selector instead.
type ManagedGraphResourceRef struct {
	// ID is the resource identifier as defined in the RGD spec.
	ID string `json:"id"`
	// NodeType is the kind of graph node this entry represents. Consumers
	// switch on this rather than on Selector != nil, and filter on it to
	// separate resources Kro owns from ones it only references.
	NodeType GraphNodeType `json:"nodeType"`
	// APIVersion is the group/version of the managed resource, in the form used
	// by ownerReferences. Split with schema.ParseGroupVersion when a
	// GroupVersionKind is needed.
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the managed resource.
	Kind string `json:"kind"`
	// Namespace is the namespace of the managed resource. Empty for cluster-scoped.
	// Set for scalar resources; omitted for collection nodes.
	Namespace string `json:"namespace,omitempty"`
	// Name is the name of the managed resource.
	// Set for scalar resources; omitted for collection nodes (see Selector).
	Name string `json:"name,omitempty"`
	// UID is the UID of the managed resource at the time of last reconciliation.
	// Set for scalar resources; omitted for collection nodes.
	UID string `json:"uid,omitempty"`
	// Revision is the RGD metadata.generation under which this resource's
	// desired state was last computed. Populated from graph.Graph.RGDGeneration,
	// a new field to be added to graph.Graph by the builder. Allows detecting
	// stale nodes during rolling graph updates.
	Revision int64 `json:"revision,omitempty"`
	// Selector is the label selector identifying all members of a collection node.
	// Set when NodeType is Collection or ExternalCollection; nil otherwise.
	// For Collection it is always kro.run/instance-id and kro.run/node-id.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}
```

##### Instance reconciler changes

`pkg/controller/instance/status.go`:

- The existing `updateStatus` path continues to write `status` (conditions,
  state, user-projected fields).
- Add a new `updateGraph` function that writes only the `graph` field, using
  server-side apply with field manager `kro-graph-manager`. Because `graph` is
  a distinct top-level field, this update never conflicts with user writes to
  `status`.
- `graph.state` mirrors `status.state` so that consumers needing Kro's
  reconciliation state do not have to cross-reference two fields.

This is second call is necessarily because `status` is a subresource, so a write
to `/status` persists only `status` and a write to the main endpoint ignores it.

##### Collections

When a resource node uses `forEach`, Kro expands it into multiple concrete
resources at reconcile time. Rather than listing each expanded object
individually, which would cause `graph.resources` to grow with the collection, size a
collection node is represented by a single `ManagedGraphResourceRef` that
stores the label selector Kro uses to target all of its members.

Kro already stamps every resource it manages with a set of labels (see
`pkg/metadata/labels.go`). For collection members the two relevant labels are:

- `kro.run/instance-id`: the UID of the owning instance
- `kro.run/node-id`: the resource ID as defined in the RGD

Together these form a stable, unambiguous selector for all objects belonging to
a given collection node within a given instance. The `ManagedGraphResourceRef` for a
collection node therefore uses a `selector` field instead of `name`/`uid`:

```yaml
graph:
  resources:
    - id: worker
      nodeType: Collection
      apiVersion: apps/v1
      kind: Deployment
      selector:
        matchLabels:
          kro.run/instance-id: "e5f6g7h8-..."
          kro.run/node-id: worker
```

A consumer wanting to enumerate the collection members issues a standard
label-selector LIST against the Kubernetes API:

```bash
kubectl get deployments \
  -l "kro.run/instance-id=e5f6g7h8-...,kro.run/node-id=worker"
```

This approach keeps the `graph` object size constant regardless of how many
items the collection expands to, and avoids having Kro maintain a potentially
large and rapidly-changing list of individual object identities.

A node that is currently excluded (via `includeWhen`) is absent from
`graph.resources` once its objects are gone. See the merge rule below for why
it stays listed while they are still being pruned.

##### Population during reconciliation

The `graph` field is assembled in `updateGraph()` during every status update,
using data already available in the reconciliation context — no additional
*reads* are needed (the extra write is covered above).

The assembly steps are:

1. **Node list**: `rcx.Runtime.Nodes()` returns `[]*runtime.Node` in topological
   order (instance node excluded).

2. **Node type filtering**: All four managed node types are included.
   `NodeTypeInstance` is the only exclusion — it represents the CR itself.

   | Node type                    | `nodeType`           | Representation in `graph.resources`                  | Kro owns it |
   |------------------------------|----------------------|------------------------------------------------------|-------------|
   | `NodeTypeResource`           | `Resource`           | scalar: `name` + `uid` from observed                 | yes         |
   | `NodeTypeExternal`           | `External`           | scalar: `name` + `uid` from observed                 | no          |
   | `NodeTypeCollection`         | `Collection`         | selector: Kro's own `instance-id` + `node-id` labels | yes         |
   | `NodeTypeExternalCollection` | `ExternalCollection` | selector: the user-supplied `metav1.LabelSelector`   | no          |

   External refs are included but tagged: `nodeType` makes the distinction
   machine-readable, so a consumer wanting only kro-owned objects filters on
   `nodeType in (Resource, Collection)`. Dropping them would hide the nodes most
   likely to be the cause when an instance is stuck waiting.

3. **Observed objects (scalar nodes)**: `node.observed` holds the
   `[]*unstructured.Unstructured` currently seen in the cluster. This field is
   private on `runtime.Node`; a `GetObserved() []*unstructured.Unstructured`
   getter must be added to expose it. For a scalar node there is at most one
   entry; name, namespace, and UID are read directly from it.

4. **Collection and external-collection nodes**: No per-object iteration is
   needed — each is represented by a single entry carrying a label selector.

    - **`NodeTypeCollection`**: the selector is assembled from two labels Kro
      already stamps on every collection member (`pkg/metadata/labels.go`):
        - `kro.run/instance-id`: `rcx.Instance.GetUID()`
        - `kro.run/node-id`: `node.Spec.Meta.ID`

    - **`NodeTypeExternalCollection`**: the user-supplied `metav1.LabelSelector`
      is stored verbatim. It is extracted from `desired[0]` at
      `metadata.selector` — the same path `processExternalCollectionNode`
      (`pkg/controller/instance/resources.go`) reads during reconciliation. If
      no selector was specified (i.e. `labels.Everything()`), the `selector`
      field is omitted from the ref.

5. **GVK**: `node.Spec.Template.GroupVersionKind()` provides the GVK;
   `gvk.GroupVersion().String()` yields `apiVersion`.
   `node.Spec.Meta.Namespaced` indicates namespace-scope.

6. **Revision**: `graph.Graph.RGDGeneration` — a new `int64` field to be added
   to `graph.Graph` by the builder, set at build time from the RGD's
   `metadata.generation` (not the `GraphRevision` object's own counter, which
   numbers graph builds rather than spec changes). Every entry written in one
   pass has the same revision. Entries differ only when one is taken over
   from an earlier pass, which is what makes a partially converged instance
   visible and why the field is per-entry.

**Merge rule.** `graph.resources` is not rebuilt purely from observed state. A
node can be temporarily unobservable (`includeWhen` unresolved, dependencies
missing, `IsIgnored()`), and dropping it would flap the field and lose the
identity of resources Kro still owns. Instead: a node with observed objects
replaces its entry, and a node without one keeps its previous entry unchanged,
including the older `revision` that marks it unconverged. An entry is dropped
only when its node leaves the graph, or is excluded by `includeWhen` *and* its
objects are confirmed pruned.

**Performance**: All inputs (`Nodes()`, `GetObserved()`, `GetName()`, etc.) are
in-memory operations on objects already fetched during the reconciliation cycle.
For a typical RGD with 5–10 nodes the assembled payload adds approximately
500 bytes to the object.

##### On RGD field cleanup

`api/v1alpha1/resourcegraphdefinition_types.go`:

`TopologicalOrder []string` and `Resources []ResourceInformation` on
`ResourceGraphDefinitionStatus` become redundant once instances carry `graph`,
since the per-instance projection is more accurate than the RGD-level one.
Removing them is **out of scope here**: they are fields on a published
`v1alpha1` type, so deleting them breaks existing readers and nothing proposed
here requires it. This KREP marks them deprecated in godoc, pointing at instance
`graph`, and keeps populating them; removal waits for an API version bump.
The `GraphRevision` internal type already holds this information for the graph-build phase and is unaffected.

##### Example

After this change, a kro-managed instance object with a scalar resource and a
collection node looks like:

```yaml
apiVersion: kro.run/v1alpha1
kind: MyApp
metadata:
  name: my-app
spec:
  replicas: 3
status:
  state: ACTIVE
  conditions:
    - type: Ready
      status: "True"
      reason: AllReady
      observedGeneration: 2
  # user-projected fields from the RGD schema
  endpoint: "https://my-app.example.com"
graph:
  observedGeneration: 2
  resources:
    # scalar resource — identified by name and uid
    - id: configmap
      nodeType: Resource
      apiVersion: v1
      kind: ConfigMap
      namespace: default
      name: my-app-config
      uid: "a1b2c3d4-..."
      revision: 3
    # collection node — identified by label selector
    - id: worker
      nodeType: Collection
      apiVersion: apps/v1
      kind: Deployment
      revision: 3
      selector:
        matchLabels:
          kro.run/instance-id: "e5f6g7h8-..."
          kro.run/node-id: worker
```

##### kubectl integration

The `graph` field is part of the instance object body, so it is visible in any
output format that returns the full object:

```bash
# Full object — graph appears at the top level alongside spec and status
kubectl get myapp my-app -o yaml

# Extract just the graph field
kubectl get myapp my-app -o jsonpath='{.graph}'
```

**Printer columns.** `kubectl get` without `-o` shows only the columns defined
in the CRD's `additionalPrinterColumns`. A priority-1 column showing the
managed resource count would be useful for `kubectl get -o wide`, but
Kubernetes printer columns use JSONPath expressions and JSONPath has no
`length()` or `count()` function.
The `graph` data remains fully accessible via `-o yaml` and `-o jsonpath` for
users and tooling that need it.

## Other solutions considered

**Keep everything in `status`.** Rejected. `status` is the user-facing contract
and should not carry Kro implementation details. Mixing the two makes the API
harder to understand and harder to evolve independently.

**Companion `GraphState` CRD per instance.** A separate namespaced CRD
(e.g. `InstanceGraph`) owned by each instance via OwnerReference would give
true API separation, but makes synchronization brittle.
This significant complexity: a new CRD, new RBAC, GC logic, and a lookup step for
every reconcile. Deferred until required.

**Store full resource objects.** Rejected. Full resource content duplicates
data already in etcd, causes unbounded object growth, and makes Kro responsible
for keeping copies in sync, on top of providing easy boundary hits for the maximum object size for large collections.
Identity references are sufficient — consumers look
up live data via the Kubernetes API.

## Scoping

#### What is in scope for this proposal?

- Schema design for the `graph` field on Instance CRDs.
- Definition of `ManagedGraphResourceRef` (identity-only reference).
- CRD synthesis changes to include the `graph` field.
- Write path changes to populate `graph` independently of `status`, including
  the merge rule and the skip-if-unchanged check.
- Deprecation godoc on `ResourceGraphDefinitionStatus.TopologicalOrder` and
  `.Resources`, and user-facing docs for the GitOps interaction.

#### What is not in scope?

- Removal of `TopologicalOrder` and `Resources []ResourceInformation` from
  `ResourceGraphDefinitionStatus` — deprecated here, removed at an API bump.
- Per-node readiness state inside `graph` (tracked via conditions in `status`).
- Changes to `GraphRevision` internal types.
- Migration tooling for existing instances that already have `topologicalOrder`
  or `resources` in their `status` block from earlier Kro versions.

## Testing strategy

#### Requirements

- A running Kro controller (integration test environment via envtest is
  sufficient).
- Generated Instance CRDs that include the `graph` field.

#### Test plan

- **Unit tests**: Verify `defaultGraphType` schema is correct; verify
  `ManagedGraphResourceRef` marshals/unmarshals correctly.
- **Integration tests** (`test/integration/`): After reconciling an instance,
  assert that `graph.resources` contains the expected identity refs and that
  `graph.resources` matches the RGD's dependency order together with the revision.
- **Field manager tests**: Verify that a user patching `status` does not
  overwrite `graph`, and a Kro patch to `graph` does not overwrite user
  `status` fields. That a reconcile changing nothing issues no graph write, and
  that `metadata.managedFields` gains exactly one `kro-graph-manager` entry
  whose size does not scale with node count (the atomic-list regression guard).
- **E2E tests** (`test/e2e/`): Smoke-test that existing chainsaw tests still
  pass with the new schema. RGD `status.topologicalOrder` is still populated,
  since removal is out of scope.

## Discussion and notes

- **Why SSA?** SMP is not available for custom resources at all, since it relies on
  `patchStrategy`/`patchMergeKey` tags compiled into the API server. The real
  alternatives are JSON merge patch, which records no ownership, and `Update`,
  which is read-modify-write and forces Kro to take fields it does not own. SSA
  writes only Kro's fields and records that it owns them but there is no enforcement.

- **Why `x-kubernetes-list-type: atomic` on `resources`?** `listType: map` +
  `listMapKey: id` would give per-element ownership, but Kro is the only writer,
  so there is no second manager to arbitrate against. SSA would then record
  one `metadata.managedFields` entry per node, on every instance. `atomic` keeps
  a single entry regardless of graph size, which is important because `managedFields`
  size counts against the ~1.5 MiB object limit.

- **A user can still clobber `graph`.** It lives in the object body, so
  `kubectl replace` or any read-modify-write `Update` omitting it will drop it;
  client-side `kubectl apply` will not, since `graph` is never in
  `last-applied-configuration`. Kro restores it on the next reconcile.

- **Conflict between Kro instances using the same field manager name.** If two
  Kro controllers manage the same instance (e.g. during a controller rollout),
  both write `graph` under `kro-graph-manager`. SSA considers this a single
  manager and the last writer wins — consistent with how `status` is treated by
  controller-runtime today. Field manager conflicts can be detected on reconcile.

- **What about the `topologicalOrder` field?** The `graph` field currently does
  not include a `topologicalOrder` list. The order is implied by the position of
  entries in `graph.resources` (which are populated in topological order). An
  explicit list could be added later if consumers need it, but would likely
  become inaccurate as soon as we implement other topological syncing mechanisms.

- **`graph.state` vs `status.state`.** Both fields reflect Kro's reconciliation
  state. `status.state` is the user-facing signal; `graph.state` is there so that
  a consumer status is kept separate from KRO's working state. Keeping
  them in sync is the responsibility of `updateGraph()`. Because they are written
  by two calls they can disagree briefly, hence `graph.observedGeneration`; if
  review would rather not duplicate state at all, dropping `graph.state` and
  keeping only `observedGeneration` is a reasonable simplification.

- **Adoption of pre-existing resources.** When Kro adopts a resource it did not
  create (e.g. an external ref), the resource appears in `graph.resources` with
  its actual `name` and `uid`. The `revision` field indicates the RGD generation
  that first included this resource.

- **Open question: should `graph` be gated by a feature flag?** Rolling out a new
  top-level field on all generated CRDs is a wide-surface change. A feature gate
  would allow operators to opt in during the transition period. This is worth
  discussing before implementation. However this would mean other features
  cant safely depend on it.
