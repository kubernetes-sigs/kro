# Graph Execution

How the controller reconciles a Graph. The DAG is the dependency structure between nodes.
Watches bring external state in. The walk converges the DAG toward the active revision's desired
state. Performance is structural — work is proportional to change, not to DAG size.

## The DAG

A directed acyclic graph of nodes. Most nodes declare a Kubernetes resource. forEach parents and
definition nodes are logical — forEach expands into child nodes at walk time (see [forEach](#foreach)),
definition nodes put values into scope without creating resources. Edges are dependencies inferred from
CEL expression references. A revision materializes the DAG.

### Nodes

A node has:

- **Identity** — node ID within the Graph's scope
- **Template** — desired state declaration with `${...}` CEL expressions referencing other nodes
- **Dependencies** — nodes this node's CEL expressions reference (edges in the DAG)

Template structure determines how the node is processed and cleaned up:

- **Owns** — creates and manages the resource. Deletes on prune.
- **Watch** — reads the resource into scope. No management.
- **WatchesKind** — discovers matching resources by selector. Enters an array into scope.
- **Contribute** — writes partial state to another actor's resource. Releases fields on prune.
- **Defines** — defines values in scope as `map[string]any`. No API
  calls, no drift timer, nothing to clean up on prune or teardown.

### Dependencies

Dependencies are inferred from CEL expression references. If node B's template contains
`${A.metadata.name}`, B depends on A. The dependency graph must be acyclic — cycles are rejected at
compile time.

Declaration order in the spec is irrelevant. The dependency graph determines execution order.
Nodes with no dependency relationship are independent and can be processed in parallel.

## The Walk

Each reconcile walks every node in topological order. At each node: should I evaluate?

A node evaluates when it has an external trigger or a propagation trigger.

External triggers — events outside the current walk:

- A watch event fired for this node
- The node's drift timer expired
- The node is in a transient error state (SystemError) — backoff retry with a low cap,
  then wait for drift timer
- First reconcile or revision transition (all nodes)

Propagation triggers — events from within the current walk:

- A dependency was invalidated during this walk (propagation hash changed at step 7)

Watch events on managed resources (Owns, Contribute) are routed by the identity label key, which
encodes the node ID. Watch events on unowned resources (Watch, WatchesKind) are routed by GVK +
namespace + name (scalar) or GVK + selector (collection) — the controller maintains a mapping from
these keys to node IDs, populated at graph compilation. A watch event on a GVK the controller
monitors triggers all nodes that declare a Watch or WatchesKind matching that resource; the
propagation hash skips downstream evaluation if the node's referenced paths didn't actually change.

Deterministic errors (4xx) are not retried — same inputs produce the same failure. They resolve via
propagation (upstream input changes), revision transition (user fixes the spec), or drift timer (the
consistency floor).

Otherwise — skip. O(1) per skipped node.

After evaluation, the node's output is hashed — only the specific field paths that downstream CEL
expressions reference (determined statically at graph compilation from expression ASTs). The hash
input is the union of all downstream-referenced paths. Absent paths hash to a fixed sentinel value
that is not a valid Kubernetes field value — the transition from absent to present is a change, not
a skip. If the hash changed — or no previous hash exists — dependents evaluate when visited later in
topological order. If not, propagation stops. Changes flow forward through the DAG and stop when they
stop mattering.

Each node has an in-memory drift timer with a jittered interval (default 30 minutes). On expiry, the
node runs the full pipeline (steps 1-7). The drift timer bypasses the template-hash check — apply
unconditionally, because server-side defaulters and mutating webhooks can change fields without
changing the desired state hash. SSA is idempotent; the apply corrects drift as a side effect.
An SSA apply resets the drift timer. A skipped write during normal evaluation (hash match from a
watch event or propagation trigger) does not — the timer still fires to catch server-side drift that
the hash cannot detect. Jitter decorrelates timers across nodes. On restart, timers reset with fresh
jitter — bounded burst (10k nodes over a 30-minute interval produces ~5.5 applies/sec). Drift timer
state is in controller metrics, not on managed resources.

The controller uses metadata-only informers — labels are visible, annotations are not. Full object
reads happen only during evaluation (step 5). When an evaluated node needs data from a skipped
dependency, the full object is read from the API server on demand. If absent (deleted externally,
not yet created), the node is Pending.

**Storage model.** Each managed resource carries two labels per Graph-node pair. The Graph-node
identity is encoded in the label prefix using DNS subdomain structure. Metadata-only informers can
read labels without fetching the full object. The controller's operational inputs are the informer
store and the DAG. Revision status is a write-only observation surface — not an operational input.

| Label key | Value | Purpose |
|---|---|---|
| `<node>.<graph>.<ns>.internal.kro.run/reference` | `owns` or `contributes` | Identity, selection, prune reference |
| `<node>.<graph>.<ns>.internal.kro.run/generation` | `graph.metadata.generation` | Observational |

Each Graph gets its own label keys. Multiple Graphs targeting the same resource coexist without
collision — a Contribute template on one Graph and an Owns template on another produce independent
labels. Typically 1-2 Graphs per resource; label count scales linearly with managing Graphs (2N
labels for N Graphs).

The identity label enables selection: the applied set is all resources where the Graph's identity
label exists. The value encodes the relationship (`owns` or `contributes`) and is read at prune time
to determine the cleanup action (delete vs release fields). The label key encodes the node ID,
which routes watch events to the correct node.

Change detection uses two in-memory hashes per node. The template-hash is computed over the desired
state — match skips the SSA write. The propagate-hash is computed over the specific field paths
dependents reference plus propagateWhen state — match stops downstream evaluation. On restart, no
cached hashes exist — the first reconcile evaluates all nodes, applies unconditionally (no hash to
compare against), and populates the hashes for steady-state. The label prefix is a DNS subdomain
(253-character limit). Graph names and non-forEach node IDs are single DNS labels. forEach children
extend the prefix with additional labels encoding the resource key (see
[Child Identity](#child-identity)).

### Wind

Forward topological walk. Every dependency is visited before its dependents — steps read
current-reconcile state for dependencies, which is always available.

1. **Skip check** — no external trigger and no propagation trigger → skip. Retains previous evaluation.
2. **Dependency check** — any dependency Excluded → Excluded, regardless of other dependencies'
   states (definitive absence propagates; the node is structurally non-viable). Any dependency in an
   error state (Conflict, Error, SystemError, or Blocked) → inherit Blocked. Any dependency Pending
   → inherit Pending. Precedence where multiple apply: Excluded > Blocked > Pending — a Blocked
   dependency's error signal takes precedence over a sibling that is merely Pending.
3. **propagateWhen** — any dependency's propagateWhen unsatisfied → gate. Template not evaluated.
   Previous evaluation and state retained. If never evaluated, the node remains Pending — dependents
   see a dependency that hasn't produced data and inherit Pending. This is correct: gated-and-never-
   evaluated means the node's output is genuinely unavailable, not merely stale. Gate takes precedence
   over triggers — including revision transitions. A revision transition marks all nodes as triggered,
   but a gated node still waits for its dependency's propagateWhen to be satisfied before evaluating.
   When the gate opens on a later reconcile, the node evaluates with current informer
   state — changes during the gate period are visible at that point.
4. **includeWhen** — false → Excluded.
5. **Dispatch:**
   - Watch: GET full object. Data enters scope. Pending if absent.
   - WatchesKind: GET matching objects. Array enters scope.
   - forEach parent: evaluate collection, determine children, dispatch changed children
     (see [forEach](#foreach)).
   - Defines: resolve all values in the template (literals and CEL expressions) against the current
     scope. Result enters scope as `map[string]any`. No API calls. CEL evaluation failure → Error.
   - Owns: evaluate template, hash desired state, compare against in-memory template-hash from
     previous evaluation. Match → skip write (unless drift timer triggered — apply unconditionally).
     Differs → SSA apply. 409 → Conflict.
   - Contribute: same as Owns. 409 → Conflict. Auto-splits status subresource.
   
   When a template targets both the main resource and the status subresource, the controller
   splits the apply into two operations. If the status subresource apply fails, the controller
   reverts the in-memory template-hash — the next reconcile sees a mismatch and retries both.
   The failure mode is an unnecessary re-apply, which is idempotent.
6. **readyWhen** — Ready or NotReady. Data is in scope regardless.
7. **Propagation check** — hash the specific field paths dependents reference (union of downstream CEL
   access chains) + propagateWhen state, compare against in-memory propagate-hash from previous
   evaluation. Differs → node invalidated. Matches → propagation stops.

Node's data enters scope after processing. For Owns and Contribute, the full object is always read
during evaluation regardless of whether the write is skipped — the template hash governs the write
decision, not the read. Two hashing boundaries: template hash (step 5) skips the write, propagation
hash (step 7) skips downstream evaluation.

### Node Evaluation

Each node's evaluation resolves to exactly one state:

| State       | Meaning                        | Dependents | Resolution                |
|-------------|--------------------------------|------------|---------------------------|
| Ready       | Applied, readyWhen satisfied   | Proceed    | —                         |
| NotReady    | Applied, readyWhen unsatisfied | Proceed    | Converges via watch       |
| Pending     | Data not yet available         | Pending    | Upstream resolves         |
| Excluded    | includeWhen false              | Excluded   | includeWhen inputs change |
| Blocked     | Dependency in error state           | Blocked    | Dependency resolves       |
| Conflict    | Field ownership contested      | Blocked    | Propagation, revision, or drift timer |
| Error       | Client request failed (4xx)    | Blocked    | Propagation, revision, or drift timer |
| SystemError | Server/infra failure (5xx)     | Blocked    | Backoff retry, then drift timer  |

Ready and NotReady are both "applied and in scope." readyWhen is a health signal — it does not gate
dependents. Pending and Blocked both represent uncertain absence — previous applied keys are
retained, not safe to prune. Excluded propagates as Excluded (definitive absence — safe to prune).

Definition nodes can be Ready, NotReady (readyWhen unsatisfied), or Error (CEL evaluation failure).
They cannot be Pending (no resource to wait for), Conflict (no SSA), or SystemError (no API calls).

### Prune

After wind, diff the current key set against the applied set. Absent resources are prune candidates
if their absence is definitive (Excluded, removed from revision). Uncertain absence (Pending,
Blocked, Error, SystemError) blocks pruning — the resource might reappear once the blocker resolves.

Conflict is deliberately excluded from the prune gate: a 409 is positive evidence that the resource
exists (another actor owns contested fields). Prune is the designed recovery mechanism for
conflicts during revision transitions — the old revision's resource is removed from the applied
set, and the new revision's template creates it fresh without the contested field ownership. A
template change clears the conflict state — the new template doesn't contest the same fields.

Prune in reverse dependency order. Owns → delete. Contribute → release fields via skeleton apply
(SSA apply with Contribute fields omitted, relinquishing field manager ownership; field values
persist under the remaining manager). Watch/WatchesKind → no action. If reverse ordering is
unavailable, prune is blocked — never degrade to unordered deletion. If a prune candidate has `finalizes` references, finalization runs
first (see [Teardown](#teardown)).

### Applied Set

The applied set is derived from the watch cache — all resources where the Graph's identity label
exists in the controller's informer stores. Not persisted. The controller already watches every GVK
it manages; every managed resource carries the identity label. The applied set is a view over
data that already exists in memory, hydrated on startup from informer list and kept current by watch
events. No crash window, no stale state, no status size limits. Both Owns and Contribute targets are
in the applied set — one mechanism. The identity label value (`owns` or `contributes`) determines the
prune action (delete vs release fields).

Prune candidates are the set difference: resources in the applied set minus the current walk's output
set. forEach scale-down, includeWhen toggles, and revision transitions all produce the same diff —
one mechanism. Superseded revisions provide reverse dependency ordering and finalizes metadata for
their resources.

forEach children participate in the applied set like any other node — each child's managed resource
carries the identity label keyed by resource key (GVK + namespace + name). The parent is a logical
node with no applied set entry. See [forEach](#foreach) for child identity, node ID encoding, and
error handling.

## forEach

A forEach node is a parent that expands into child nodes — one per item in a collection. The parent
is a logical node (no managed resource). Children are nodes — all existing per-node machinery
applies.

```
                                forEach parent
                                ┌──────────┐
    ${namespaces} ─────────────▶│ policies │  (logical — no managed resource)
                                │ (parent) │
                                └────┬─────┘
                  ┌──────────────────┼──────────────────┐
                  ▼                  ▼                  ▼
     ┌──────────────────┐ ┌──────────────────┐ ┌──────────────────┐
     │ default-deny/ns-a│ │ default-deny/ns-b│ │ default-deny/ns-c│
     │     (child)      │ │     (child)      │ │     (child)      │
     └──────────────────┘ └──────────────────┘ └──────────────────┘
```

### Child Identity

A child's identity is scoped to its parent and encodes the full resource key as DNS subdomain
labels within the label key:

    <parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/reference

For the concrete example — NetworkPolicy `default-deny` in namespace `ns-a`, parent `policies`,
graph `mygraph` in namespace `default`:

    policies.default-deny.ns-a.networkpolicy.networking.k8s.io.mygraph.default.internal.kro.run/reference

This is the same label key structure as any node — the parent ID is the first label, followed by
the resource key components as additional DNS labels before the graph identity. A non-forEach node
`deploy` produces `deploy.mygraph.default.internal.kro.run/reference`. Uniqueness is across the full resource key (GVK + namespace + name). If the rendered key changes, that's a new child — the old one is a prune
candidate. Resource keys must be unique across children of the same parent — validated at expansion
time.

### Parent Expansion

The parent evaluates the collection and determines which children exist. This is the one new
operation the DAG supports: a logical node that expands into children at walk time.

1. **Evaluate collection** — CEL expression produces `[]any`. An empty collection produces zero
   children — the parent enters scope as `[]` and is Ready (readyWhen is per-child; zero children
   means vacuously satisfied).
2. **Render identity** — per item, bind the iterator variable and resolve the identity fields:
   `apiVersion`, `kind`, `metadata.name`, and `metadata.namespace`. Any of these may be CEL
   expressions. If any identity expression cannot evaluate — upstream dependency Pending, CEL type
   error, nil dereference — the parent is Pending (upstream not ready) or Error (expression failure).
   Expansion does not proceed and existing children persist. Partial expansion is never attempted.
3. **Dispatch children** — each child evaluates its template like any node, with the iterator
   variable bound to the collection item. readyWhen expressions are evaluated per-child — within
   readyWhen, `${policies}` binds to the individual child's managed resource. In all other contexts
   (downstream templates, scope), `${policies}` is the parent's aggregated `[]any`.
4. **Aggregate** — the parent collects child scope entries into `[]any` and enters scope. The parent
   does not wait for readyWhen — downstream nodes proceed as soon as child data is in scope.

### Parent State

The parent's state is derived from its children:

- **Pending** — any child has not yet been dispatched or is awaiting its first result
- **Ready** — all children Ready
- **NotReady** — any child NotReady, none in error states
- **Error/Conflict/SystemError** — any child in an error state. Error states take precedence over
  Pending — a child that attempted apply and got a Conflict is in Conflict state, not Pending.
  Deterministic errors (Error) take precedence over transient errors (SystemError, Conflict) — if
  any child's failure is deterministic, retrying cannot resolve the parent. Per-child detail surfaces
  in Graph status.

### Collection Ordering

The `[]any` array downstream nodes receive matches expansion order. The expansion is deterministic
given the same input collection — same items in the same order produce the same children in the same
order. Observed state (resources read back from the cluster) is aligned to expansion order by
resource key during aggregation.

Ordering stability depends on the input collection's stability. If the input reorders (API server
returns namespaces in a different order), the downstream `[]any` reorders — but each child's
identity (resource key) is unchanged. Index-sensitive downstream CEL (`${policies[0]}`) is fragile
unless the collection is explicitly sorted.

### Prune (Scale-Down)

A removed collection item means a child that existed last cycle is absent this cycle. The child's
managed resource is in the previous applied set but not the current one — standard prune candidate.
Prune in reverse dependency order. If the forEach node itself is removed from the spec (revision
transition), children are pruned before the parent is removed from the DAG.
No forEach-specific scale-down logic — the applied set diff handles it.

If the collection expression cannot evaluate (upstream is Pending), the intended child set is
unknown — prune is blocked and existing children persist until the next successful evaluation.

### forEach Strategies

forEach expands a single template per item. If you need multiple resources per item, two options:

**Sibling forEach blocks** — two forEach entries with the same collection expression. Dependencies
between them are expressed in the outer DAG. This is the default. Use it when each item needs a
small number of resources with straightforward dependencies between them.

```yaml
- id: quotas
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: v1
    kind: ResourceQuota
    metadata:
      name: default
      namespace: ${ns.metadata.name}
    spec:
      hard:
        pods: "10"

- id: policies
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    metadata:
      name: default-deny
      namespace: ${ns.metadata.name}
    spec:
      podSelector: {}
```

**Nested Graphs** — forEach creates a child Graph per item. Each child Graph is independently
reconciled by its own watches. Use nested Graphs when per-item isolation matters — a failure in one
item shouldn't affect others' reconcile cadence — or when each item's subgraph is complex
enough that you'd write a separate Graph for it by hand.

```yaml
- id: perNamespace
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: experimental.kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${ns.metadata.name}-resources
    spec:
      nodes:
        - id: nsRef
          template:
            apiVersion: v1
            kind: Namespace
            metadata:
              name: ${ns.metadata.name}
        - id: quota
          template: ...
        - id: policy
          template: ...
```

Flat forEach is simpler — fewer objects, one reconcile loop. Nested Graphs cost more objects but
decouple per-item reconciliation entirely. Start with flat forEach. Move to nested when you need
per-item isolation or the per-item subgraph outgrows a single template.

## Concrete Example

A Graph that creates a Deployment from a watched ConfigMap, a Service for the Deployment, and a
NetworkPolicy per Namespace:

```yaml
spec:
  nodes:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: app-config

    - id: namespaces
      template:
        apiVersion: v1
        kind: Namespace
        selector: {}

    - id: deploy
      readyWhen:
        - ${deploy.status.availableReplicas > 0}
      propagateWhen:
        - ${deploy.status.updatedReplicas == deploy.spec.replicas}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: my-app
        spec:
          replicas: 3
          selector:
            matchLabels:
              app: my-app
          template:
            metadata:
              labels:
                app: my-app
            spec:
              containers:
                - name: app
                  image: ${config.data.image}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${deploy.metadata.name}-svc
        spec:
          selector: ${deploy.spec.selector.matchLabels}
          ports:
            - port: 80

    - id: policies
      forEach:
        ns: ${namespaces}
      template:
        apiVersion: networking.k8s.io/v1
        kind: NetworkPolicy
        metadata:
          name: default-deny
          namespace: ${ns.metadata.name}
        spec:
          podSelector: {}
          policyTypes:
            - Ingress
```

This produces the following DAG:

```
Level 0:  config (Watch)      namespaces (WatchesKind)
              │                       │
Level 1:  deploy (Owns)       policies (forEach parent)
              │                  ┌────┼────┐
Level 2:  service (Owns)     default-deny  default-deny  default-deny  (forEach children)
                               /ns-a        /ns-b         /ns-c
```

Two independent branches. A change to config propagates through the left branch — deploy and service
are evaluated, the right branch skips. A change to a Namespace propagates through the right branch —
policies and children are evaluated, the left branch skips.

## Revision Transitions

A revision materializes a DAG. There is always exactly one target DAG — the latest revision's. The
controller winds toward it.

When the Graph's spec changes, a new revision is materialized. The controller abandons any
in-progress convergence and begins converging toward the new one. Same walk, same prune — but a
revision change treats all nodes as triggered. The DAG structure changed; the complete key set must
be recomputed.

If revision N is propagating when N+1 arrives, N is abandoned. Resources that N partially created
either match N+1's templates (kept) or don't (pruned). Resources that N never created either appear
in N+1's DAG or don't (nothing to prune). There is no concurrent convergence across revisions.

### Prune Ordering from Superseded Revisions

Pruning requires reverse dependency ordering — if B depends on A and both are being pruned, B must be
deleted before A. The ordering comes from the most recent revision that defined each pruned resource.

A superseded revision is retained until its unique resources are pruned. This is its only purpose —
providing prune ordering, reference type (Owns vs Contribute), and finalizes metadata for each
pruned resource. The old revision's `finalizes` declarations govern the prune of its resources — if a
new revision changes or drops `finalizes`, the old revision's metadata still applies to resources
being pruned from it. Fast spec
mutations cause superseded revisions to accumulate. This is bounded by the mutation rate — a natural
bottleneck users manage by not mutating faster than the controller converges.

## Worked Example

Tracing the concrete example through a sequence of events: a config change, a forEach scale-up, and
a revision transition.

**Initial state:** fully converged. `data.image: nginx:1.25`. Namespaces a, b, c.

---

**Event 1: ConfigMap updated** — `data.image` changed to `nginx:1.26`.

Watch event fires for node `config`. Walk iterates all nodes; changes propagate through the
left branch:

| Node       | Triggered? | Dep invalidated? | Action                                                                    | Evaluation |
|------------|------------|-------------------|---------------------------------------------------------------------------|------------|
| config     | Yes        | —                 | GET full object → hash referenced paths → differs → invalidated               | Ready      |
| namespaces | No         | No                | Skip                                                                      | —          |
| deploy     | No         | Yes (config)      | Evaluate template (config in scope) → hash differs → SSA apply            | NotReady   |
| service    | No         | Yes (deploy)      | propagateWhen unsatisfied (rollout in progress) → retain previous state   | Ready      |
| policies   | No         | No                | Skip                                                                      | —          |
| policies/*  | No         | No                | Skip                                                                      | —          |

1 apply. Service gated by propagateWhen. Right branch untouched — no trigger, no propagation.

When the rollout completes, a Deployment status watch fires for `deploy`:

| Node       | Triggered? | Dep invalidated? | Action                                                                         | Evaluation |
|------------|------------|-------------------|--------------------------------------------------------------------------------|------------|
| config     | No         | No                | Skip                                                                           | —          |
| namespaces | No         | No                | Skip                                                                           | —          |
| deploy     | Yes        | No                | GET full object → propagateWhen now satisfied → propagation (gate changed)    | Ready      |
| service    | No         | Yes (deploy)      | Evaluate template (deploy in scope) → hash matches → skip write           | Ready      |
| policies   | No         | No                | Skip                                                                           | —          |
| policies/*  | No         | No                | Skip                                                                           | —          |

0 applies. deploy evaluated because status changed; propagateWhen now satisfied. service
evaluates but template output unchanged — write skipped.

---

**Event 2: Namespace added** — Namespace `d` created.

Watch event fires for node `namespaces`. Changes propagate through the right branch:

| Node                         | Triggered? | Dep invalidated?   | Action                                     | Evaluation |
|------------------------------|------------|--------------------|--------------------------------------------|------------|
| config                       | No         | No                 | Skip                                       | —          |
| namespaces                   | Yes        | —                  | GET matching objects → 4 namespaces → differs | Ready      |
| deploy                       | No         | No                 | Skip                                       | —          |
| service                      | No         | No                 | Skip                                       | —          |
| policies                     | No         | Yes (namespaces)   | Expand children: new child for `d`. a, b, c unchanged | Ready      |
| policies/default-deny/ns-d   | —          | —                  | New child → evaluate → SSA apply           | Ready      |
| policies/default-deny/ns-{a,b,c} | —     | —                  | Input hash match → skip                    | Ready      |

1 apply. Left branch untouched. Three existing children skipped.

---

**Event 3: Spec change** — `monitoring` added (depends on `deploy`), `service` removed. New revision.

All nodes treated as triggered (revision transition — DAG structure changed). Propagation hashes are
computed but do not gate evaluation — every node passes the skip check because all are triggered:

| Node       | Action                                                          | Evaluation |
|------------|-----------------------------------------------------------------|------------|
| config     | GET full object → template hash unchanged → skip write              | Ready      |
| namespaces | GET matching objects → template hash unchanged → skip write         | Ready      |
| deploy     | GET full object → template hash unchanged → skip write              | Ready      |
| monitoring | New node → evaluate template → SSA apply                        | Ready      |
| policies   | Evaluate collection → same children as before                   | Ready      |
| policies/*  | Template hash unchanged → skip write                            | Ready      |

Active revision's applied set: {deploy, monitoring, policies/default-deny/ns-a,
policies/default-deny/ns-b, policies/default-deny/ns-c, policies/default-deny/ns-d}.
Previous revision's applied set included `service`. Prune: delete service.

1 apply + 1 delete.

## Teardown

When a Graph is deleted, the controller unwinds in reverse topological order through the active
revision's DAG. If the revision was deleted (ownerReference cascade race), the controller regenerates
it from the current spec. Teardown is blocked until ordering is available — never degrade to
unordered deletion.

Owns nodes delete the resource. Contribute nodes release fields via skeleton apply. Watch and
WatchesKind take no action. If resources persist (child Graphs with finalizers, external
finalizers), requeue. Once all managed resources are processed, remove the Graph's finalizer.

### Finalization

When a resource is a prune candidate and another node declares `finalizes` pointing at it, the
deletion is gated on the finalizer resource completing. `finalizes` introduces two behaviors that
do not emerge from the DAG:

- **Creates during prune** — the finalizer resource does not exist during normal operation. It
  materializes when the target becomes a prune candidate.
- **Inverts deletion ordering** — normally, dependents are deleted before dependencies. `finalizes`
  inverts this for the target/finalizer pair: the target is deleted before the finalizer resource.

The sequence within a prune walk:

1. The prune walk encounters the target. The controller creates the finalizer resource — the target
   is still fully operational, no `metadata.deletionTimestamp`. This matters: setting
   `deletionTimestamp` can trigger the target's own controller to start destroying underlying
   infrastructure before the finalizer resource has a chance to act. The finalizer resource's key is
   added to the applied set.
2. The finalizer resource reaches readyWhen. If multiple finalizer nodes target the same
   resource, dependencies among them determine ordering — all must be Ready before proceeding.
3. The controller issues DELETE on the target.
4. The prune walk continues. The finalizer resources are in the applied set but not in the desired
   state — they are prune candidates. The walk picks them up and deletes them in reverse dependency
   order.

Finalization state is fully recoverable from the Graph spec, applied set, and cluster state — no
additional state machine is needed. If the controller crashes at any point, the next reconcile
re-derives position: the applied set identifies which finalizer resources were created, the cluster
reveals whether they exist and satisfy readyWhen, and the spec provides the `finalizes` relationships.
SSA idempotency covers re-creation.

The prune ordering must account for finalizer resource dependencies beyond the target. If a finalizer
resource's template references a ConfigMap, that ConfigMap must not be deleted until finalization
completes — even if the ConfigMap is in a different branch of the normal DAG. Resources referenced by
an in-flight finalizer resource are deferred until finalization completes.

Side effects from completed finalizer resources are not rolled back on partial failure. If one
finalizer reaches Ready but a sibling fails, the completed finalizer's effects persist. Finalization
is not transactional.

If the target resource does not exist in the cluster (creation failed, already deleted externally),
there is nothing to finalize. The controller skips finalization and proceeds with cleanup. The
Graph's status surfaces this: `FinalizerSkipped` with a message naming the resource.

If the target exists but the finalizer resource cannot be created — dependency failure, admission
webhook rejection, quota exhaustion, or invalid rendered template — the target's deletion is blocked.
This is `TeardownBlocked`, not a skip — the target has data that the user intended to finalize.

If the finalizer resource is created but never reaches readyWhen (permanent failure), the target's
deletion is blocked. This is also `TeardownBlocked`. The condition message distinguishes between
creation failure and readyWhen failure. To unblock, update the Graph spec to remove or fix the
finalizer resource. The revision transition prunes the orphaned finalizer resource and deletes the
target without finalization.

## Why Not

**Periodic full-graph resync.** Informer resyncs trigger all nodes simultaneously — correlated,
expensive. Per-node drift timers with jitter amortize resync across reconciles. Simpler scheduling,
but more moving parts than a single resync interval.

**Bespoke object caching for partial evaluation.** Caching full objects per node between reconciles
enables skipping nodes entirely, but introduces coherence obligations — the cache can go stale, and
the controller is responsible for invalidation. Dirty propagation through a full topological walk
achieves the same performance (work proportional to change) without maintaining object state.
Informer stores provide the read path using standard Kubernetes watch machinery.

**BFS-scoped walks from triggers.** Walking only the downstream subgraph of a triggered node avoids
iterating over unrelated branches. But iterating is O(1) per skipped node (a boolean check), so the
savings are negligible. The cost is architectural complexity — maintaining walk scopes and restoring
previous state for out-of-scope nodes. A full iteration with O(1) skip is simpler and effectively
equivalent.

**Continuous drift correction.** Apply unconditionally on every reconcile. Catches drift immediately
but imposes an N-write steady-state tax. Drift timers with jitter correct drift within the interval
at near-zero steady-state cost.

**forEach as a single node with internal iteration.** The forEach handler reimplements scheduling,
state tracking, readyWhen, and change detection inside a mini-coordinator — a parallel system
alongside the DAG walk. Parent-child makes children real nodes. One system.

**forEach children as dynamic siblings.** Children appear as peers in the DAG rather than under a
parent. No natural aggregation point for downstream consumers — the parent-child relationship has to
be reinvented as a convention. Aggregation is structural, not conventional.

**Index-based forEach identity.** Child identity derived from position in the collection. If the
collection reorders (API server returns namespaces in a different order), children churn — deletes
and recreates for what should be no-ops. Resource-key identity is stable under reordering.

**Content-hash forEach identity.** Child identity derived from a hash of the collection item. Any
field mutation changes the hash — a label change on a namespace produces a new child identity,
triggering delete and recreate. Resource-key identity changes only when the template's identity
fields (namespace, name) change.

**forEach as a subgraph stamper.** forEach expands into a subgraph of multiple resources per item.
Sibling forEach blocks and nested Graphs already compose for multi-resource-per-item cases — one
template per forEach keeps the primitive simple.
