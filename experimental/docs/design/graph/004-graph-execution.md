# Graph Execution

How the controller reconciles a Graph. The DAG is the dependency structure between resources.
Watches bring external state in. The walk converges the DAG toward the active revision's desired
state. Performance is structural — work is proportional to change, not to DAG size.

## The DAG

A directed acyclic graph of nodes. Each node is a single resource. Edges are dependencies inferred
from CEL expression references. A revision materializes a DAG — the static structure the walk
operates on.

### Nodes

A node has:

- **Identity** — resource ID within the Graph's scope
- **Template** — desired state declaration with `${...}` CEL expressions referencing other nodes
- **Dependencies** — nodes this node's CEL expressions reference (edges in the DAG)

Template shape determines how the node is processed and cleaned up:

- **Owns** — creates and manages the resource. Deletes on prune.
- **Watch** — reads the resource into scope. No management.
- **Collection Watch** — discovers matching resources by selector. Enters an array into scope.
- **Contribute** — writes partial state to another actor's resource. Releases fields on prune.

### Dependencies

Dependencies are inferred from CEL expression references. If node B's template contains
`${A.metadata.name}`, B depends on A. The dependency graph must be acyclic — cycles are rejected at
compile time.

Declaration order in the spec is irrelevant. The dependency graph determines execution order.
Resources with no dependency relationship are independent and can be processed in parallel.

## The Walk

A watch event triggers a walk. A cluster change to a resource the Graph cares about enqueues a
reconcile. No watch event, no reconcile. Steady-state cost is zero when nothing changes.

Each reconcile walks a scope of the DAG in two phases: wind (forward topological) and prune (reverse
topological on the diff set). Within each phase, nodes at the same topological level are independent
and can be processed in parallel. Nodes outside the scope are untouched.

### Scoped Walks

Without scoping, every reconcile walks the entire DAG — every node runs a change check even on
branches unrelated to the triggering event. Scoped walks restrict the reconcile to the affected
subgraph.

Two reverse indexes map watch events to Graphs:

- **Scalar index** — GVR + namespace + name → Graph(s). O(1) per event.
- **Collection index** — GVR + selector → Graph(s). The watch registration carries the node ID.

A watch event fires for a resource. The node that corresponds to the changed resource is the
starting point. Walking downstream — to nodes that depend on the starting point — produces the
walk scope.

In the common case (a leaf or mid-level node changed), the scope is a small subset of the DAG. In
the worst case (a root changed), it's the entire DAG. There is no scenario where scoped walks are
more expensive than full walks.

### Plan States

Each node in the walk scope lands in exactly one state per reconcile:

| State    | Meaning                        | Dependents | Resolution                          |
|----------|--------------------------------|------------|-------------------------------------|
| Ready    | Applied, readyWhen satisfied   | Proceed    | —                                   |
| NotReady | Applied, readyWhen unsatisfied | Proceed    | Converges via watch                 |
| Pending  | Data not yet available         | Blocked    | Upstream resolves                   |
| Excluded | includeWhen false              | Blocked    | includeWhen inputs change           |
| Conflict | Field ownership contested      | Blocked    | Template change or external release |
| Error    | Transient failure              | Blocked    | Retries next reconcile              |

Ready and NotReady are both "applied and in scope." readyWhen is a health signal that feeds the
Graph's aggregated status — it does not gate dependents. Dependents proceed as soon as the resource's
data is in scope.

Blocked states propagate through the DAG. If A is Pending, B depends on A, B's CEL expressions
cannot resolve — B is also Pending. No explicit propagation mechanism is needed; CEL evaluation
failure IS the propagation. Independent branches are unaffected.

Conflict is distinct from Error. Error retries automatically. Conflict persists until resolved:
change the template to remove contested fields, or wait for the external actor to release them.

### Wind

Forward topological walk through the scoped nodes. For each node:

1. **Dependency check** — if any dependency is in a blocked state, skip. The node inherits the
   blocked state.
2. **propagateWhen check** — if a dependency has `propagateWhen` unsatisfied, the node retains its
   previous plan state and data in scope. It is not re-evaluated. propagateWhen gates data flow
   during transitions — a Deployment mid-rollout has data in scope, but propagateWhen prevents that
   transitional state from flowing to dependents. readyWhen is a health signal. propagateWhen is a
   data flow gate.
3. **Change check** — hash the node's inputs and compare against the previous reconcile's hash. If
   the hash matches, the node's inputs haven't changed — evaluation would produce the same result.
   The node retains its previous state, data in scope, and tracked set entries. Skip to the next
   node.

   The hash is scoped to the sections of each dependency that the node's CEL expressions actually
   reference. At compile time, the referenced top-level sections are identified from CEL field access
   paths (`.spec`, `.status`, `.data`, `.metadata`). At runtime, only those sections are hashed.
   `metadata.resourceVersion` changes on every update, so full-object hashing would always differ and
   the check would never fire. Section scoping is the correct mechanism, not an optimization.

   For Watch nodes, the input is the observed object itself — GET (or informer cache read), then hash
   the referenced sections. For all other nodes, the inputs are the dependency data already in scope.
   For Owns and Contribute nodes, the inputs also include sections of the node's own observed resource
   that readyWhen and propagateWhen reference — these change independently of upstream dependencies.

4. **includeWhen** — evaluate. If false, mark Excluded.
5. **Dispatch by shape:**
   - Watch: data enters scope. Pending if absent.
   - Collection Watch: array enters scope.
   - forEach parent: evaluate collection, diff items, process changed children (see
     [forEach](#foreach)).
   - Owns: evaluate template, hash the output, compare against previous. Match → skip write.
     Differs → SSA apply.
   - Contribute: same as Owns but force-apply. Auto-splits status subresource.
6. **readyWhen** — evaluate. Sets Ready or NotReady. Data is in scope regardless.

After processing, the node's data enters scope — the full Kubernetes object including server-assigned
fields. Downstream nodes can reference it.

Every boundary in this pipeline hashes its input to avoid unnecessary work: the change check (step 3)
hashes the node's inputs to skip evaluation, the dispatch (step 5) hashes the evaluated template to
skip the write. One mechanism at each level — skip when the input hasn't changed.

### Prune

After wind, the controller diffs the current key set against the tracked set. Resources tracked but
absent from the current set are prune candidates.

Not all absent resources should be pruned:

- **Definitive absence** — excluded by includeWhen, or removed from the active revision. Safe to
  prune.
- **Uncertain absence** — a dependency is Pending, Conflict, or Error. The resource might appear
  once the blocker resolves. Not safe to prune.

Prune candidates are removed in reverse dependency order. Owns nodes delete the resource. Contribute
nodes release fields via skeleton apply. Watch and Collection Watch take no action. If reverse
dependency ordering is unavailable, prune is blocked — the controller never degrades to unordered
deletion.

forEach scale-down, includeWhen toggles, and revision transitions all produce the same kind of diff.
The pruning mechanism is uniform.

### Tracked Set

The tracked set is the set of resource keys (GVK + namespace + name) that a revision has applied. It
lives on the GraphRevision — the revision is the single source of truth for both what was applied
(tracked set) and in what dependency structure (DAG topology). The Graph object carries no tracking
state.

Each revision starts with an empty tracked set and populates it during wind. The tracked set is
committed atomically after wind completes successfully. If wind fails partway, the tracked set is
unchanged — next reconcile retries from scratch. Writing per-resource during wind would mean a
partial failure leaves the tracked set reflecting an incomplete state, and the next reconcile's prune
diff would be based on corrupt data.

The tracked set is only written when the key set actually changes, preventing spurious
resourceVersion bumps and re-reconcile loops in steady state. A forEach that stamps 100 resources
produces 100 expanded keys.

**Scoped walks and the tracked set.** A scoped walk evaluates a subset of nodes, but the tracked set
must be complete. The update is a merge: for visited nodes, replace their previous entries with the
walk's output. For unvisited nodes, retain previous entries. Three cases for visited nodes:

- **Node produced a key** (Ready, NotReady) — the key enters the tracked set.
- **Node produced no key** (Excluded, or forEach with zero items) — the node's previous keys are
  **removed**. Excluded means "this node's contribution to the tracked set is the empty set," not
  "this node is invisible to the tracked set."
- **Node was skipped** (change check match, propagateWhen gate) — previous keys retained.

This merge is correct within a stable revision — the DAG structure hasn't changed, so unvisited
nodes' entries are still valid. **Revision transitions trigger full walks** because the DAG structure
changed and stale entries would never be cleaned up by a scoped walk.

**Prune formula:** the prune candidate set is the union of all superseded revisions' tracked sets
minus the active revision's tracked set. Each superseded revision's DAG topology provides reverse
dependency ordering for its tracked resources.

## forEach

A forEach node is a parent that stamps child nodes — one per item in a collection. Each child is an
independent node in the DAG with its own template, hash, and dependency edges.

```
                                forEach parent
                                ┌──────────┐
    ${namespaces} ─────────────▶│ policies │
                                │ (parent) │
                                └────┬─────┘
                          ┌──────────┼──────────┐
                          ▼          ▼          ▼
                     ┌─────────┐ ┌─────────┐ ┌─────────┐
                     │ pol/ns-a│ │ pol/ns-b│ │ pol/ns-c│
                     │ (child) │ │ (child) │ │ (child) │
                     └─────────┘ └─────────┘ └─────────┘
```

**The parent node** evaluates the collection expression and caches each item's last-known state,
keyed by the item's identity (stable name or UID, not array position). The parent completes when all
children have been processed — applied and in scope. Downstream nodes that depend on the forEach
proceed once the parent completes. They do not wait for every child to satisfy readyWhen — data
availability is the gate, not health.

**Child nodes** bind the iterator variable to their item and evaluate the template independently.
Each child is identified by the collection item it's bound to. If the collection returns items in a
different order, the children are the same — same identities, same templates, no churn.

**On reconcile,** the parent diffs the current collection against the cached state:

1. Re-evaluate the collection expression
2. Diff item identities against cached identities — detect adds, removes, changes
3. Changed items: update cached state, re-evaluate that child's template
4. Unchanged items: skip template evaluation

forEach evaluates only changed items, not the entire collection. The identity diff is negligible.
Template evaluations — the real cost — only run for items whose data actually changed. New items
create child nodes. Removed items are pruned. If the collection expression cannot evaluate (upstream
is Pending), the intended item set is unknown — prune is blocked and existing children persist until
the next successful evaluation.

### forEach Strategies

forEach stamps a single template per item. If you need multiple resources per item, two options:

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

**Nested Graphs** — forEach stamps a child Graph per item. Each child Graph is independently
reconciled by its own watches. Use nested Graphs when per-item isolation matters — a failure in one
item shouldn't affect others' reconcile cadence — or when each item's resource graph is complex
enough that you'd write a separate Graph for it by hand.

```yaml
- id: perNamespace
  forEach:
    ns: ${namespaces}
  template:
    apiVersion: kro.run/v1alpha1
    kind: Graph
    metadata:
      name: ${ns.metadata.name}-resources
    spec:
      resources:
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
  resources:
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
Level 0:  config (Watch)      namespaces (Collection Watch)
              │                       │
Level 1:  deploy (Owns)       policies (forEach parent)
              │                  ┌────┼────┐
Level 2:  service (Owns)     pol/a  pol/b  pol/c  (forEach children)
```

Two independent branches. A change to config triggers a scoped walk of the left branch only. A
change to a Namespace triggers a scoped walk of the right branch only.

## Revision Transitions

A revision materializes a DAG. There is always exactly one target DAG — the latest revision's. The
controller winds toward it.

When the Graph's spec changes, a new revision is materialized. The controller abandons any
in-progress convergence and begins converging toward the new one. Same walk, same prune — but a
revision change triggers a full walk. The DAG structure changed; the complete key set must be
recomputed.

If revision N is propagating when N+1 arrives, N is abandoned. Resources that N partially created
either match N+1's templates (kept) or don't (pruned). Resources that N never created either appear
in N+1's DAG or don't (nothing to prune). There is no concurrent convergence across revisions.

### Prune Ordering from Superseded Revisions

Pruning requires reverse dependency ordering — if B depends on A and both are being pruned, B must be
deleted before A. The ordering comes from the most recent revision that defined each pruned resource.

A superseded revision is retained until its unique resources are pruned. This is its only purpose —
providing prune ordering and template shape (Owns vs Contribute) for each pruned resource. Fast spec
mutations cause superseded revisions to accumulate. This is bounded by the mutation rate — a natural
bottleneck users manage by not mutating faster than the controller converges.

## Worked Example

Tracing the concrete example through a sequence of events: a config change, a forEach scale-up, and
a revision transition.

**Initial state:** fully converged. `data.image: nginx:1.25`. Namespaces a, b, c.

---

**Event 1: ConfigMap updated** — `data.image` changed to `nginx:1.26`.

Scope: scalar index → `app-config` → node `config`. BFS: config → deploy → service. Right branch
not in scope.

| Node    | Action                                                                    | Result   |
|---------|---------------------------------------------------------------------------|----------|
| config  | GET → change check (`.data`) → differs → new data in scope               | Ready    |
| deploy  | Change check → differs → evaluate → output hash differs → SSA apply      | NotReady |
| service | propagateWhen unsatisfied (rollout in progress) → retain previous state   | Ready    |

1 GET + 1 apply. Service skipped by propagateWhen. Right branch untouched.

When the rollout completes, a Deployment status watch fires. Next reconcile walks {deploy, service}.
deploy: change check — dependency inputs (`config.data`) unchanged, but own observed state
(`deploy.status`) changed → hash differs → re-evaluate readyWhen/propagateWhen (template unchanged,
no apply). propagateWhen now satisfied. service: change check — inputs (`deploy.metadata.name`,
`deploy.spec.selector.matchLabels`) unchanged. Skip entirely. 0 applies.

---

**Event 2: Namespace added** — Namespace `d` created.

Scope: collection index → `namespaces` node. BFS: namespaces → policies → children. Left branch
untouched.

| Node                         | Action                                     | Result |
|------------------------------|--------------------------------------------|--------|
| namespaces                   | List → 4 namespaces                        | Ready  |
| policies                     | Diff items: new key `d`. a, b, c unchanged | Ready  |
| pol/ns-d                     | New child → evaluate → SSA apply           | Ready  |
| pol/ns-a, pol/ns-b, pol/ns-c | Unchanged → skip                           | Ready  |

1 List + 1 apply. Three existing children untouched.

---

**Event 3: Spec change** — `monitoring` added (depends on `deploy`), `service` removed. New revision.

Full walk (DAG structure changed).

| Node       | Action                                    | Result |
|------------|-------------------------------------------|--------|
| config     | Change check (`.data`) → match → skip     | Ready  |
| namespaces | Change check → match → skip               | Ready  |
| deploy     | Change check → match → skip               | Ready  |
| monitoring | New node → evaluate → SSA apply           | Ready  |
| policies   | Change check → match → skip               | Ready  |
| pol/ns-*   | Change check → match → skip               | Ready  |

Active revision's tracked set: {deploy, monitoring, pol/ns-a, pol/ns-b, pol/ns-c, pol/ns-d}.
Previous revision's tracked set included `service`. Prune: delete service.

1 apply + 1 delete. Everything else skipped.

## Teardown

When a Graph is deleted, the controller unwinds in reverse topological order through the active
revision's DAG. If the revision was deleted (ownerReference cascade race), the controller regenerates
it from the current spec. Teardown is blocked until ordering is available — never degrade to
unordered deletion.

Owns nodes delete the resource. Contribute nodes release fields via skeleton apply. Watch and
Collection Watch take no action. If resources persist (child Graphs with finalizers, external
finalizers), requeue. Once all managed resources are processed, remove the Graph's finalizer.

## Why Not

**Full walks on every reconcile.** Scoped walks and change checks together make steady-state cost
proportional to change, not DAG size. Full walks are the degenerate case.

**Full-object change checks.** `metadata.resourceVersion` changes on every update. Full-object
hashing always differs. Section-scoped hashing is the correct mechanism — only changes to referenced
sections trigger evaluation.

**forEach as a monolithic node.** Re-evaluating all children when one item changes is wasted work
proportional to the collection size. Parent-with-children evaluates only changed items.

**Incremental tracked set updates.** Per-resource writes during wind mean partial failure corrupts
the set. Atomic commit after complete wind is safe.

Also rejected: index-based forEach identity (reordering causes churn), forEach as a subgraph stamper
(sibling blocks and nested Graphs compose better), separate rollforward algorithm (one walk handles
all cases), unordered deletion (stuck finalizers), continuous drift restoration (N-write steady-state
tax).
