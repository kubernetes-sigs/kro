# KREP-026: Propagation control

## Problem statements

### Graph dependencies not rich enough

Updates to a Graph in kro don't block on any readiness signal from dependencies.
They only wait for the dependent resource to be applied. This is an issue because
many resources depend on an operator to act or call an external 
system before they're actually ready.

Take a Graph that manages the IAM role that a Deployment runs under. If a code change
needs new permissions and the updated code deploys before the permission update
lands, you can cause the application to crashloop. 

`readyWhen` doesn't help here because resources still get applied even when
their dependencies aren't ready. In RGD, `readyWhen` will block applying
but not in graph. 

`includeWhen` can help the first time an instance is created, but it has a bad
tradeoff. A temporary blip in another resource could drop it from the included
set and cause an outage, since it's no longer applied.


### Prevent updates during certain times for RGDs/Graphs

Some platform teams may want to restrict updates from happening outside of
business hours. You may want to prevent updating resources at 2am. You may want
the opposite: only update resources outside of peak business hours to lower the
number of customers impacted by a change.

Another use case that falls into this would be freezing changes during an alarm
or an event. You may want to stop rolling out updates if the error rate for
customers is above a threshold.

A maintenance window has 2 halves: a way to define the window and a way to block
updates while it is closed. Defining the window is a separate problem (KREP
#1376, `time.now()`). `propagateWhen` is the second half, the place where that
predicate can stop an apply.

### Throttling of updates to a collection for RGD/Graphs

Suppose kro is managing web servers for a set of tenants. kro could model this
as a single Graph with each tenant being an item in a collection.

When the number of tenants grows (1000+ objects), kro applies resources very
quickly, leading to high load on the API.

kro also ends up having a large blast radius. A bad update would be applied to
each element of the graph without waiting for previous tenants to be healthy.

In both RGD and Graph, an update is applied to the entire collection in a single
reconcile. kro sends up to 20 server-side apply requests in parallel
(configurable with `--apply-concurrency`), but it doesn't wait for any item to
become ready before applying the next. Downstream dependencies can wait for all
the resources to be ready, but each collection item has no way of waiting for
or checking the health of previous applies.

### Throttling of updates to RGD instances

A way to model kro managing web servers for tenants using an RGD would be to
have one RGD contain the definition of the server and each instance represent a
tenant.

The same problems that apply to large collections apply to RGD instances. Many
instances will cause high API traffic load and throttling.

There is no way for updates to an instance to wait for any health signal, so the
blast radius of a bad change is all tenants.

Conceptually these examples are identical, but in code and in how users
configure them there is a distinction.

## Non-goals

- Rollback. We don't intend to solve automated rollback between RGD instances or
  collection items.
- Manual pinning. We don't intend to allow a user to set the graph revision an
  RGD instance reconciles against. A held instance is pinned by the gate, not by
  the user.
- Versioning. We don't intend to solve the problem of breaking RGD changes or
  allowing some instances to be on one RGD major version for some time.

## Solution

A new `propagateWhen` field. Supported at the resource level, collection level,
or RGD level.

```yaml
propagateWhen:
  - ${example.a}
  - ${example.b}
```

Expressions are ANDed. All elements must be true for it to propagate.


### Examples

Wait for an IAM role to sync before updating a Deployment

```
- id: role
  template:
    apiVersion: iam.services.k8s.aws/v1alpha1
    kind: Role
    ...
  readyWhen:
    - ${role.status.conditions.exists(c, c.type == 'ACK.ResourceSynced' && c.status == 'True'
        && c.observedGeneration == role.metadata.generation)}

- id: app
  propagateWhen:
    - ${role.ready()}
  template:
    apiVersion: apps/v1
    kind: Deployment
    ...
```

Watch an alarm and prevent rolling out updates to resources

```
- id: errorRate
  ref:
    apiVersion: monitoring.example.com/v1
    kind: Alarm
    metadata: {name: payments-5xx, namespace: platform}

- id: app
  propagateWhen:
    - ${errorRate.status.state != 'ALARM'}
  template: {kind: Deployment ...}
```

Slow down propagation within a collection
```
- id: tenants
  ref: {apiVersion: platform.example.com/v1, kind: Tenant, metadata: {namespace: platform, selector: {}}}

- id: web
  forEach:
    - t: ${tenants}
  readyWhen:
    - ${each.status.observedGeneration == each.metadata.generation
        && each.status.updatedReplicas == each.spec.replicas
        && each.status.replicas == each.status.updatedReplicas
        && each.status.availableReplicas == each.spec.replicas}
  propagateWhen:
    - ${web.filter(w, w.updated() && !w.ready()).size() < 10}     # at most 10 mid-rollout
  template:
    ...
```

A selector ref without `namespace` lists across all namespaces. The `readyWhen`
must be false for the whole rollout: `availableReplicas == replicas` alone stays
true during a surge rollout, so the budget never engages.

Slow down propagation between RGD instances
```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata: {name: tenant-web}
spec:
  schema:
    kind: TenantWeb
    spec: {image: string, tier: string | default=prod}
  propagateWhen:                                   # evaluated per instance by the RGD controller
    - ${instances.filter(i, i.updated() && !i.ready()).size() < 10}
  resources:
    - id: deployment
      readyWhen:
        - ${deployment.status.observedGeneration == deployment.metadata.generation
            && deployment.status.updatedReplicas == deployment.spec.replicas
            && deployment.status.replicas == deployment.status.updatedReplicas
            && deployment.status.availableReplicas == deployment.spec.replicas}
      template:
        ...
```

### Resource level

Gating on another node's readiness is the most common use of `propagateWhen`,
and nothing in a Graph exposes it today. This does not compile:

```
- id: db
  template: ...
  readyWhen:
    - ${db.status.readyReplicas == db.spec.replicas}

- id: app
  template:
    ...
            env:
            - name: DB_READY
              value: ${string(db.ready())}   # db.ready() is not exposed
```

A template can read `db.status.readyReplicas`, but not the `readyWhen` verdict
the author already wrote for `db`. `propagateWhen` exposes that verdict as
`<id>.ready()`: true when node `<id>` has been applied and its `readyWhen`
holds. The same spelling is used for collection items and fleet instances
below.

`propagateWhen` creates ordinary dependencies on the resources it references,
identical to template references. The gate adds a new predicate that is
evaluated once those dependencies are in scope. If any of the `propagateWhen`
conditions are false, the node and every node that depends on it are held: not
rendered, not applied, not pruned. Their objects stay as last applied. Pruning
is per node: a hold on one node does not stop Kro from pruning objects retired
by other nodes in the same reconcile.

A condition is put on the Graph/RGD instance, giving users a way to understand
what has happened. An example of the condition is below:

```
status:
  conditions:
    - type: ResourcesConverged
      status: "False"
      reason: Held
      message: 'node "app": propagateWhen "errorRate.status.state != ''ALARM''" is false'
    - type: Ready
      status: "False"
      reason: Held
      message: 'node "app": propagateWhen "errorRate.status.state != ''ALARM''" is false'
```

An RGD instance held on an old revision stays `Ready=True`, because it is healthy
on that revision, and reports the hold on `RevisionReconciled`:

```
    - type: RevisionReconciled
      status: "False"
      reason: Held
      message: 'held on revision 5, latest is 6: propagateWhen "<expr>" is false'
```

For Graph, `propagateWhen` is supported on every node that writes or computes
(`template`, `patch`, `graph`, `def`) and rejected on `ref`, which has no
desired state to hold. Gate the consumer of a `ref`, not the `ref`.

A held node also retains contributions, the same as an already applied object
that does not change.

For RGD, `propagateWhen` is supported on `template` fields. RGD `externalRef`s
do not support `propagateWhen` for the same reason as Graph refs.

On RGD, dependency edges already wait for readiness, so `${a.ready()}` is
redundant there. The other gates are not.

The definition of "update" in kro spans various situations:

1. An update to the definition of an RGD/Graph
2. A change to the instance values of the RGD schema
3. A change in Kubernetes resources that an RGD/Graph is referencing through CEL
4. A change to the applied objects by an external system, away from what kro wants (drift detection)

A held node stops all 4, drift correction included. Re-applying a stale
definition to keep catching drift would be worse than pausing both.


### Collection

A collection in kro is one node. Everything in the resource-level section
applies to it: a false gate holds the collection and its dependents.

A collection gate that references neither the collection's own `id` nor `each`
is evaluated once for the whole node: either every item is applied or none is.
Referencing either switches the gate to per-item evaluation, described under
Implementation. The self-reference does not create a dependency edge.

Writing meaningful expressions requires a way to refer to the collection as a
whole. For example, updating a collection with a batch size requires knowing
whether other items are being updated before deciding whether to update the next
one.

The collection's resource `id` can be referenced in the `propagateWhen` clause of
that collection. It is a list type.

Each element in the collection also gets an `updated()` method, which helps
determine whether a resource is updated.

A collection `propagateWhen` can then be used to cap the number of updating
elements to 10.

```
- id: web
  forEach:
    - t: ${tenants}
  propagateWhen:
    - ${web.filter(w, w.updated() && !w.ready()).size() < 10}
```

You can prevent rollouts if there are existing issues.

```
- id: web
  forEach:
    - t: ${tenants}
  propagateWhen:
    - ${web.filter(w, !w.updated() && !w.ready()).size() == 0}
```

You can create exponential rollouts by allowing as many in-progress updates as
there are completed ones.

```
- id: web
  forEach:
    - t: ${tenants}
  propagateWhen:
    - "${cel.bind(done, web.filter(w, w.updated() && w.ready()).size(), web.filter(w, w.updated() && !w.ready()).size() < (done == 0 ? 1 : done))}"
```

Expressions containing `? :` must be quoted: an unquoted `: ` ends a YAML scalar.

The collection item can also be referenced in the evaluation through `each`.
This allows expressions that prefer certain collection items over others. For
example, roll out the canary tenants first:

```
- id: web
  forEach:
    - t: ${tenants}
  propagateWhen:
    - ${each.metadata.labels.tier == 'canary' || web.filter(w, w.metadata.labels.tier == 'canary' && !(w.updated() && w.ready())).size() == 0}
  template:
    metadata:
      labels:
        tier: ${t.spec.tier}
```

### RGD level

The third and final place kro can put `propagateWhen` is at the RGD level.

```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: webservice
spec:
  schema:
    apiVersion: v1alpha1
    kind: WebService
    spec:
      image: string
      tier: string | default="standard"

  # Fleet rollout policy. Evaluated per instance when a new revision of this
  # RGD is published. `instances` is every instance of this RGD; `each` is the
  # instance under decision. Excluded from the revision hash: changing this
  # field does not itself trigger a rollout.
  propagateWhen:
    - ${instances.filter(i, i.updated() && !i.ready()).size() < 10}

  resources:
    - id: deploy
      ...
```

`instances` is bound only inside `spec.propagateWhen`. It is not a reserved
word; an RGD may have a resource with that id. The fleet gate does not see the
RGD's resources, so there is nothing to shadow. It works very similarly to
collection `propagateWhen`.

Each element of `instances`, and `each`, is the full instance object
(`metadata`, `spec`, and `status`) plus two facts. `updated()` is true when the
instance's `status.observedRevision` is the latest revision. `ready()` is true
when the instance's kro `Ready` condition is `True`. 

A held instance is treated the same as a held node: it is not rendered or
applied, and its existing objects stay as they are. Spec edits and changes to
referenced data wait until the instance is admitted. Deleting a held instance is
not gated.


## Implementation

### Single resources

A resource that is not a collection is processed in one step per reconcile. The
executor walks nodes in dependency order. For each node, it:

1. Evaluates `propagateWhen`
2. Renders the template
3. Applies the template
4. Evaluates `readyWhen` against the object the api server returned

If `propagateWhen` is false, we skip rendering and applying it.

Because of this order, it's not possible for a dependent to read a single
resource at an older version. A dependent can only read a node after that node
has been applied with its current render, so "has kro's write landed" is always
yes at the point of the read. There is no need for an `updated()` method for
single resources because it would always return true at time it would be evaluated.

### Updated

`updated()` returns true if the resource has been applied with what kro would
render now.

This can be used with `ready()` to help determine the state of a resource.

| State | Expression |
|---|---|
| Converged | `w.updated() && w.ready()` |
| Updating / in flight | `w.updated() && !w.ready()` |
| Pending (healthy on old version) | `!w.updated() && w.ready()` |
| Stuck (broken on old version) | `!w.updated() && !w.ready()` |

This is tracked with an annotation, `kro.run/spec-hash`. The hash is taken from
kro's render before mutating webhooks run, so if a webhook adds a label, the
object won't show as perpetually different.

This also means that external writers will not cause the object to temporarily
become not updated. kro will still do drift detection, but it won't cause a blip
in the `updated()` status.

Graph currently records the UID of each object and a patch contribution ledger,
but it does not hash the state of the object or store any revision information.
RGDs have Graph Revisions, but these are not stored at a per-resource level.


### Collection

To evaluate `propagateWhen` for a collection, kro:

1. Renders every item and reads every item's live object.
2. Applies every item that already carries the current hash, in parallel at
   `--apply-concurrency`, without evaluating the gate. There is nothing to
   advance; this is drift correction.
3. For the remaining items, in order, evaluates `propagateWhen` with `each`
   bound to the item and the collection list including the items admitted
   earlier in this pass.
4. If it is true, admits the item. It immediately counts as
   `updated() && !ready()` in the collection list. Admitted items are applied
   in batches of `--apply-concurrency`; after each batch their facts are
   replaced by what the api server returned, and the loop resumes.
5. If it is false, holds the item. Holding writes nothing, so the collection
   list doesn't change and the gate would give the same answer for every
   remaining item. The rest of the pass holds them without evaluating again.
   The one exception is if `propagateWhen` refers to `each`, the item under
   decision. kro can't stop early if it does, because the answer may differ per
   item.

Every reconcile either admits at least one item or evaluates the gate once,
because in-flight items are already at the current hash and never evaluate.

Referencing `each` requires each reconcile to evaluate `propagateWhen` for all
non-current objects, but it enables customers to do things like roll out canary
tenants first.

```
- ${each.metadata.labels.tier == 'canary' || web.filter(w, w.metadata.labels.tier == 'canary' && !(w.updated() && w.ready())).size() == 0}
```

At 1000 pending items that is about 750 ms per reconcile
([benchmark](https://github.com/NicholasBlaskey/kro/blob/poc/propagate-when/pkg/graphengine/executor/krep026_bench_test.go),
which also pins the evaluation counts: 0 converged, 1 stalled budget, 999
stalled `each` gate).


### RGD instances

RGD instances work similarly to collections, with one added area of complexity.

A collection's admissions are decided in one loop, in one reconcile. Updates to
RGD instances happen across a number of workers in the dynamic controller.

This means race conditions need care. It also means there is no order for RGD
instances by default. Users can still control some ordering by defining waves
like this, but no other way will be provided to control the ordering.

```
propagateWhen:
  - ${each.spec.tier == 'canary' || instances.filter(i, i.spec.tier == 'canary' && !(i.updated() && i.ready())).size() == 0}
  - ${instances.filter(i, i.updated() && !i.ready()).size() < 10}
```

Supporting ordering would require a central admitter per RGD instead of
per-instance evaluation. It would also require an additional way to configure
the order, like an `orderBy` field.

To get around the race condition, kro keeps an in-memory admission ledger per
RGD: a lock and the set of instances admitted to the current revision.

When an instance reconciles and a newer revision exists, kro evaluates the
fleet gate under that lock. New instances and instances already on the latest
revision skip the gate entirely.

Workers evaluate the gate one at a time against a snapshot of all instances.
kro tracks admissions in memory, so a worker sees every admission made before
it, even if that instance's status write hasn't landed yet. Once admitted, an
instance records the new revision in `status.observedRevision` before applying
anything, so the in-memory record only has to outlive that one write and can be
lost on restart without consequence. The apply itself runs in parallel with
other workers.

A held instance is not applied and its readiness is not re-evaluated; only
`RevisionReconciled=False` with reason `Held` is written. It re-evaluates the
gate when another instance converges and frees a slot, or on the next periodic
requeue.

Because a held instance is never applied, it does not need the revision it is
on to stay resolvable. `status.observedRevision` is only compared to the latest
revision number. GraphRevision garbage collection is unchanged.

## Release plan

This requires CRD updates, but it will still be placed behind an alpha feature
gate. Removing the field would still be a breaking CRD change, but keeping it
alpha initially gives more room to change its semantics.

Note that this KREP proposes 4 features, and not all of them need to be
accepted.

1. `propagateWhen` on Graph resources
2. `propagateWhen` on RGD resources
3. `propagateWhen` on collections
4. `propagateWhen` on RGD instances

(1) is by far the most valuable feature. Without (1), it is challenging to build
safe complex graphs.

The other features are valuable but not as essential.

## Alternatives

### Sorting collections

An alternative is for kro to reorder the items of a collection on each
reconcile. It's possible we could sort healthy items first, random within each
class, so the best update candidates are reached before stuck ones.

We believe allowing the user to define the order of the collection will allow
flexibility and control, and avoid becoming overly opinionated.

### Propagation control implemented by Graph

An alternative would be to avoid editing RGD at all. We could instead let a user
reimplement parts of an RGD as a Graph, which would give them more control over
rollout.

While this could technically be done, providing the simpler interface of RGD
with primitives would help customers.

Moving from RGD to Graph is not a low lift today, so not supporting features
that could be on RGD becomes a hard edge. It either makes
customers go without features or undergo a costly migration process.

### Not having `updated()`

Propagation control introduces a new method for collection items, `updated()`,
to allow statements to be written about collections or RGD instances.

Instead, we could redefine what `ready()` means. `ready()` could be false when a
resource is not on the latest generation. Then statements like this are
possible:

```
propagateWhen:
  - ${web.filter(w, !w.ready()).size() < 10}
```

This statement means at most 10 items can be unready at any moment. This could
be a proxy for batch size, but it doesn't really get there, since 10 terminally
unready instances block your update.

We also do not save much implementation effort. This new definition of ready
would require tracking a resource hash too.

### Policies

Another approach could be to avoid letting users write their own expressions
for propagation control of collections, and instead offer built-in policies as
list elements.

Some examples could be:

```
propagateWhen: linear(list, batchSize=10)
propagateWhen: exp(list, rate=2)
```

These could make it simpler and make it easier for us to optimize them. The
issue is a lack of flexibility and committing to certain methods.

### Allow configuring dependencies as ready-blocking or not

In RGDs, a dependency not being ready will block the graph from progressing.

In Graph, a dependency not being ready won't.

`propagateWhen` solves this, but an alternative could be allowing you to define
dependencies as ready-blocking or not.

Example:

```
waitForReady: ${resource1}
```

Or alternatively, allow RGD to configure dependencies that only wait for apply:

```
dependencyOverrides:
  resource1: waitForReady
  resource2: waitForApply
```

This might be challenging for users to understand. It also does not solve the
use case of maintenance windows or halting updates.

## Extensions

### Reduce kro write throughput

The `kro.run/spec-hash` annotation can be used to eventually stop needing to
issue writes every reconcile.

It is not trivial because kro supports drift detection. Another mechanism will
be needed to handle this.

I intend to write a follow-up KREP around this optimization.

## Prior work

### KREP-006: Propagation Control

[#861](https://github.com/kubernetes-sigs/kro/pull/861) by ellistarn, opened
2025-11-24. Still open and marked stale. It introduced `propagateWhen` and the
pairing with `readyWhen`: `propagateWhen` opens a node for mutation and
`readyWhen` closes it. It also proposed `linearlyReady` and `exponentiallyReady`
helpers built on item index, which were later dropped in favor of filter
expressions.

The implementation lives on the `krocodile` branch of `ellistarn/kro`. It moved
the gate to the consumer, added `ready()` and `updated()` per item, and sorts
collections Ready-first with a random tiebreak.

This KREP keeps the consumer-side gate, hold-not-prune, and filter-form budgets.
It differs in 3 ways. `updated()` is not defined on single resources, and on
collection items it compares a render hash instead of a generation, so changes
to referenced objects are caught. kro does
not reorder items. The RGD-level fleet gate is new.

### KREP-024: Instance Propagation Control

[#1259](https://github.com/kubernetes-sigs/kro/pull/1259) by michaelhtm, opened
2026-04-17, closed unmerged 2026-08-06. It proposed a structured `propagate`
field on the RGD with a keyword rate (`linear(N)`, `exponential(N)`) and CEL
over aggregate counters. The final revision exposed per-instance objects with
`ready()`. It kept an in-memory registry of enqueued instances, and review
asked how that survives a controller takeover.

This KREP uses CEL predicates instead of keyword rates. The admission ledger
answers the takeover question by writing `status.observedRevision` before the
apply. #1259 let instance spec changes through while an instance was held.
This KREP holds them.

### KREP-025: Propagation Rate Limit

[#1267](https://github.com/kubernetes-sigs/kro/pull/1267) by michaelhtm, opened
2026-04-23, closed unmerged 2026-05-13. It proposed a time-based rate limit for
instance rollout. It was closed in favor of solving rate limiting through #1259
plus `time.now()` in CEL. The CEL time library is now
[#1376](https://github.com/kubernetes-sigs/kro/pull/1376) (open).

### Explicit dependencies

[#110](https://github.com/kubernetes-sigs/kro/issues/110) (open) asks for a
`dependsOn` field. [#331](https://github.com/kubernetes-sigs/kro/pull/331) by
rushmash91 implemented it and was closed unmerged on 2025-09-28. On Graph, a
bare ordering edge does not wait for readiness, so it would not solve the
problem in this KREP. `propagateWhen: [${x.ready()}]` covers the same use case.

[#764](https://github.com/kubernetes-sigs/kro/pull/764), merged 2025-11-04,
made RGD instances wait for dependency readiness before applying. This is why
`${x.ready()}` is redundant on RGD.

### Related

- [#883](https://github.com/kubernetes-sigs/kro/issues/883) (open) tracks
  versioning and rollout of RGD changes. This KREP covers the rollout half.
- [#1132](https://github.com/kubernetes-sigs/kro/pull/1132), merged 2026-03-12,
  blocks graph propagation on terminating managed resources.
- [#1215](https://github.com/kubernetes-sigs/kro/pull/1215) (KREP-023, closed)
  proposed level-by-level apply for the instance controller.
