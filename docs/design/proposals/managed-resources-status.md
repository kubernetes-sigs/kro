# Managed Resources Status via Graph Field

## Problem statement

When kro reconciles a ResourceGraphDefinition (RGD) instance, it needs to
track its own view of the resource graph: which managed resources exist, their
topological ordering, dependency relationships, and the current reconciliation
state. Today this data is mixed into the user-facing `status` block, which
creates two problems.

**API surface pollution.** The `status` subresource is the contract between
kro and the user. Fields such as `topologicalOrder` and
`resources []ResourceInformation` are kro implementation details based on its graph projection that should
not be part of that contract. Every time kro's internal bookkeeping changes,
the user-visible API changes with it.

**Scalability.** If managed resource objects (or even rich per-node detail) were
stored inside the instance status, the instance object would grow proportionally
to the number and size of its managed resources. For graphs with many nodes, or
with collection expansions, this quickly becomes an etcd size problem. The only
safe data to store is a compact identity reference, not full resource content.

## Proposal

Introduce a dedicated `graph` field at the top level of every kro-generated
Instance CRD, sitting alongside `spec` and `status`. The `graph` field is
written exclusively by kro using its own field manager, keeping it
operationally separate from user-controlled `status`. It contains only the
internal bookkeeping data kro needs to reconcile the instance.

The `graph` field stores a compact identity reference (`ManagedResourceRef`)
for each managed resource rather than the full resource object. Consumers who
need live resource data look it up directly using the identity fields.

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

Ideally `graph` would work the same way — a separate REST endpoint that kro
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
- kro applies the `graph` field using a dedicated field manager
  (`kro-graph-manager`). Server-side apply records ownership of every field
  under `graph` to that manager.
- No other actor — user, admission webhook, or GitOps controller — should claim
  ownership of `graph` fields. If a foreign manager attempts to set a field
  under `graph`, the API server will return a conflict error, making the
  boundary machine-enforceable.
- From a RBAC perspective, `graph` cannot be isolated at the verb level today
  (since there is no `/graph` subresource path). Access is controlled at the
  resource level. Operators who want to read graph data but not modify the
  instance can be granted `get`/`list`/`watch` on the instance resource; kro
  itself needs `update` (or `patch`) on the instance to write `graph`.

This is the same pattern used by tools such as cert-manager (which applies
`status` fields on generated `Secret` objects) and Argo CD (which writes
`metadata.annotations` under its own field manager without touching user
annotations). It is a well-understood, production-proven approach that does not
require infrastructure beyond what kro already uses.

If the Kubernetes ecosystem adds support for arbitrary named CRD subresources in
the future, migrating `graph` to a true subresource would be a backwards-
compatible change: the field would move from the main object body to its own
endpoint, and existing readers could be updated incrementally.

#### Overview

- Add a `graph` field to the schema of every kro-generated Instance CRD.
- kro writes `graph` with its own field manager (`kro-graph-manager`), never
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
        kro's view of the reconciliation state for this instance.
        Mirrors status.state so that consumers of the graph field do not
        need to cross-reference the status block.
    resources:
      type: array
      description: Identity references to all managed resources in the graph.
      items:
        type: object
        required: [ id, version, kind ]
        properties:
          id:
            type: string
            description: The resource ID as defined in the RGD.
          group:
            type: string
            description: API group of the managed resource (empty for core group).
          version:
            type: string
            description: API version of the managed resource.
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
            description: >
              Label selector (metav1.LabelSelector) identifying all members of a
              collection node. Set only for forEach collection nodes; absent for
              scalar resources.
            properties:
              matchLabels:
                type: object
                additionalProperties:
                  type: string
              matchExpressions:
                type: array
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

// ManagedGraphResourceRef is a compact identity reference to a managed Kubernetes
// resource. For scalar resources it identifies the specific object; for
// collection (forEach) nodes it carries a label selector instead.
type ManagedGraphResourceRef struct {
	// ID is the resource identifier as defined in the RGD spec.
	ID string `json:"id"`
	// Group is the API group of the managed resource. Empty for core group.
	Group string `json:"group,omitempty"`
	// Version is the API version of the managed resource.
	Version string `json:"version"`
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
	// Revision is the RGD generation under which this resource's desired state
	// was last computed. Populated from graph.Graph.RGDGeneration, a new field
	// to be added to graph.Graph by the builder. Allows detecting stale nodes
	// during rolling graph updates.
	Revision int64 `json:"revision,omitempty"`
	// Selector is the label selector identifying all members of a collection node.
	// Set only for collections; nil for scalar resources.
	// The selector always includes kro.run/instance-id and kro.run/node-id.
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
- `graph.state` mirrors `status.state` so that consumers needing kro's
  reconciliation state do not have to cross-reference two fields.

##### Collections

When a resource node uses `forEach`, kro expands it into multiple concrete
resources at reconcile time. Rather than listing each expanded object
individually — which would cause `graph.resources` to grow with the collection
size — a collection node is represented by a single `ManagedResourceRef` that
stores the label selector kro uses to target all of its members.

kro already stamps every resource it manages with a set of labels (see
`pkg/metadata/labels.go`). For collection members the two relevant labels are:

- `kro.run/instance-id`: the UID of the owning instance
- `kro.run/node-id`: the resource ID as defined in the RGD

Together these form a stable, unambiguous selector for all objects belonging to
a given collection node within a given instance. The `ManagedResourceRef` for a
collection node therefore uses a `selector` field instead of `name`/`uid`:

```yaml
graph:
  resources:
    - id: worker
      group: apps
      version: v1
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
items the collection expands to, and avoids having kro maintain a potentially
large and rapidly-changing list of individual object identities.

A node that is currently excluded (via `includeWhen`) is absent from
`graph.resources`. If it is later included it appears; if it is pruned it
disappears.

##### Population during reconciliation

The `graph` field is populated in `updateGraph()` during every status update,
using data already available in the reconciliation context — no additional API
calls are needed.

The assembly steps are:

1. **Node list**: `rcx.Runtime.Nodes()` returns `[]*runtime.Node` in topological
   order (instance node excluded).

2. **Node type filtering**: All four managed node types are included.
   `NodeTypeInstance` is the only exclusion — it represents the CR itself.

   | Node type                    | Representation in `graph.resources`                  |
      |------------------------------|------------------------------------------------------|
   | `NodeTypeResource`           | scalar: `name` + `uid` from observed                 |
   | `NodeTypeExternal`           | scalar: `name` + `uid` from observed                 |
   | `NodeTypeCollection`         | selector: kro's own `instance-id` + `node-id` labels |
   | `NodeTypeExternalCollection` | selector: the user-supplied `metav1.LabelSelector`   |

3. **Observed objects (scalar nodes)**: `node.observed` holds the
   `[]*unstructured.Unstructured` currently seen in the cluster. This field is
   private on `runtime.Node`; a `GetObserved() []*unstructured.Unstructured`
   getter must be added to expose it. For a scalar node there is at most one
   entry; name, namespace, and UID are read directly from it.

4. **Collection and external-collection nodes**: No per-object iteration is
   needed — each is represented by a single entry carrying a label selector.

    - **`NodeTypeCollection`**: the selector is assembled from two labels kro
      already stamps on every collection member (`pkg/metadata/labels.go`):
        - `kro.run/instance-id`: `rcx.Instance.GetUID()`
        - `kro.run/node-id`: `node.Spec.Meta.ID`

    - **`NodeTypeExternalCollection`**: the user-supplied `metav1.LabelSelector`
      is stored verbatim. It is extracted from `desired[0]` at
      `metadata.selector` — the same path `processExternalCollectionNode`
      (`pkg/controller/instance/resources.go`) reads during reconciliation. If
      no selector was specified (i.e. `labels.Everything()`), the `selector`
      field is omitted from the ref.

5. **GVK**: `node.Spec.Template.GroupVersionKind()` provides group, version, and
   kind. `node.Spec.Meta.Namespaced` indicates namespace-scope.

6. **Revision**: `graph.Graph.RGDGeneration` — a new `int64` field to be added
   to `graph.Graph` by the builder, set to the GraphRevision revision spec field at
   build time. When the RGD is updated and a new graph is built, the revision increments,
   making it visible in `graph.resources` which nodes are operating under the new
   graph and which are still converging. This allows n-1 to n migration logic.

A new `graph.resources` list is assembled fresh on every reconcile pass from the
current observed state. Nodes where `IsIgnored()` is true and nodes with no
observed objects need to be kept until the graph converges to avoid lost state.
The list is the authoritative live view at the time of the last write.

**Performance**: All inputs (`Nodes()`, `GetObserved()`, `GetName()`, etc.) are
in-memory operations on objects already fetched during the reconciliation cycle.
For a typical RGD with 5–10 nodes the assembled payload adds approximately
500 bytes to the object.

##### On RGD field cleanup

`api/v1alpha1/resourcegraphdefinition_types.go`:

Long-Term we can (but don't have to) Remove `TopologicalOrder []string` and `Resources []ResourceInformation` from
`ResourceGraphDefinitionStatus`, as the instance projection is more accurate than the one from the RGD.
The equivalent data for instances could now be held in `graph`.
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
  resources:
    # scalar resource — identified by name and uid
    - id: configmap
      version: v1
      kind: ConfigMap
      namespace: default
      name: my-app-config
      uid: "a1b2c3d4-..."
      revision: 3
    # collection node — identified by label selector
    - id: worker
      group: apps
      version: v1
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
and should not carry kro implementation details. Mixing the two makes the API
harder to understand and harder to evolve independently.

**Companion `GraphState` CRD per instance.** A separate namespaced CRD
(e.g. `InstanceGraph`) owned by each instance via OwnerReference would give
true API separation, but makes synchronization brittle.
This significant complexity: a new CRD, new RBAC, GC logic, and a lookup step for
every reconcile. Deferred until required.

**Store full resource objects.** Rejected. Full resource content duplicates
data already in etcd, causes unbounded object growth, and makes kro responsible
for keeping copies in sync, on top of providing easy boundary hits for the maximum object size for large collections.
Identity references are sufficient — consumers look
up live data via the Kubernetes API.

## Scoping

#### What is in scope for this proposal?

- Schema design for the `graph` field on Instance CRDs.
- Definition of `ManagedResourceRef` (identity-only reference).
- CRD synthesis changes to include the `graph` field.
- Write path changes to populate `graph` independently of `status`.
- Removal of `TopologicalOrder` and `Resources []ResourceInformation` from
  `ResourceGraphDefinitionStatus`.

#### What is not in scope?

- Per-node readiness state inside `graph` (tracked via conditions in `status`).
- Changes to `GraphRevision` internal types.
- Migration tooling for existing instances that already have `topologicalOrder`
  or `resources` in their `status` block from earlier kro versions.

## Testing strategy

#### Requirements

- A running kro controller (integration test environment via envtest is
  sufficient).
- Generated Instance CRDs that include the `graph` field.

#### Test plan

- **Unit tests**: Verify `defaultGraphType` schema is correct; verify
  `ManagedResourceRef` marshals/unmarshals correctly.
- **Integration tests** (`test/integration/`): After reconciling an instance,
  assert that `graph.resources` contains the expected identity refs and that
  `graph.resources` matches the RGD's dependency order together with the revision.
- **Field manager tests**: Verify that a user patching `status` does not
  overwrite `graph`, and a kro patch to `graph` does not overwrite user
  `status` fields.
- **E2E tests** (`test/e2e/`): Smoke-test that existing chainsaw tests still
  pass with the new schema; no `topologicalOrder` field in `status`.

## Discussion and notes

- **Why SSA and not a strategic merge patch?** SSA tracks per-field ownership,
  making it impossible for another manager to silently overwrite `graph` fields.
  A strategic merge patch offers no ownership guarantees and would require the
  caller to include the full `graph` object on every update, increasing the risk
  of accidental overwrites from concurrent writers.

- **Conflict between kro instances using the same field manager name.** If two
  kro controllers manage the same instance (e.g. during a controller rollout),
  both write `graph` under `kro-graph-manager`. SSA considers this a single
  manager and the last writer wins — consistent with how `status` is treated by
  controller-runtime today. Field manager conflicts can be detected on reconcile.

- **What about the `topologicalOrder` field?** The `graph` field currently does
  not include a `topologicalOrder` list. The order is implied by the position of
  entries in `graph.resources` (which are populated in topological order). An
  explicit list could be added later if consumers need it, but would likely
  become inaccurate as soon as we implement other topological syncing mechanisms.

- **`graph.state` vs `status.state`.** Both fields reflect kro's reconciliation
  state. `status.state` is the user-facing signal; `graph.state` is there so that
  a consumer status is kept separate from KRO's working state. Keeping
  them in sync is the responsibility of `updateGraph()`.

- **Adoption of pre-existing resources.** When kro adopts a resource it did not
  create (e.g. an external ref), the resource appears in `graph.resources` with
  its actual `name` and `uid`. The `revision` field indicates the RGD generation
  that first included this resource.

- **Open question: should `graph` be gated by a feature flag?** Rolling out a new
  top-level field on all generated CRDs is a wide-surface change. A feature gate
  would allow operators to opt in during the transition period. This is worth
  discussing before implementation. However this would mean other features
  cant safely depend on it.
