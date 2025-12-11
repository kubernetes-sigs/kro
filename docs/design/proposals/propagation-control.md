# KREP-006 Propagation Control

## Summary

KREP-006 introduces `propagateWhen`, a per-resource mechanism to conditionally gate mutation as
changes propagate through the graph. Both `propagateWhen` and `readyWhen` are complementary and
bookend when mutation for a node in the graph can start and is considered complete.

## Motivation

KREP-002 introduces Collections, a mechanism for managing resources based on data discovered at
runtime. It's important to recognize that each ResourceGraphDefinition is itself implicitly a
collection, where each instance of the ResourceGraphDefinition is a member of that collection. There
are risks associated with any change to a ResourceGraphDefinition or the explicit collections
defined within. For example, an organization that has used KRO to unify application deployment with
an Application CRD risks cluster-wide impact from a bad change to the ResourceGraphDefinition. A
ResourceGraphDefinition that loops over a collection of zones to deploy a set of zonal Deployments
risks regional impact from a bad change in the deployment's configuration.

Propagation risks are exacerbated by collections, but exist even for single instances of simple
ResourceGraphDefinitions. Any change to production involves risk, which creates opportunities to
manage that risk. This observation leads to us viewing propagation control for a single resource a
base case for propagation control over a collection. As KRO takes an increasingly large role for
deciding what is running in production, it must provide controls to decide when production can
change.

## Use Cases

1. **Rate Controls**: As an administrator, I want to limit the rate of change to collections of
   resources defined in my ResourceGraphDefinitions when the inputs to the graph change. As an
   administrator, I want to limit the rate of change to the instances of a ResourceGraphDefinition
   when the definition itself changes.
2. **Time Controls**: As an administrator, I want to prevent changes from happening outside of
   business hours, including evenings, weekends, and holidays.
3. **Reactive Controls**: As an administrator, I want to prevent changes when an alarm fires.
4. **Manual Controls**: As an administrator, I want to manually prevent changes while I root cause
   an incident.

## Proposed API and Behavior

1. Add `propagateWhen` to `ResourceGraphDefinitionSpec` to control propagation to graph instances
2. Add `propagateWhen` to `Resource` to control propagation to a resource in a graph instance
3. Add built-in CEL functions `exponentiallyUpdated` and `linearlyUpdated` for propagation control
4. The default `propagateWhen` for collections (including RGDs) is `[exponentiallyUpdated]`
5. The default `propagateWhen` for individual resources is `[]`

**CEL Functions:**

```cel
// exponentiallyUpdated(collection, each) -> bool
// Returns true when updated items grow exponentially: 1, 2, 4, 8, 16...
// An item is considered updated when its generation annotation matches the graph revision generation
exponentiallyUpdated(collection, each) =
  size(collection.filter(i, i.metadata.annotations['kro.run/generation'] == string(schema.metadata.generation))) >=
  int(math.pow(2.0, double(int(math.log(double(each.index + 1)) / math.log(2.0)))))

// linearlyUpdated(collection, each, batchSize) -> bool
// Returns true when updated items grow linearly by batchSize
// An item is considered updated when its generation annotation matches the graph revision generation
linearlyUpdated(collection, each, batchSize) =
  size(collection.filter(i, i.metadata.annotations['kro.run/generation'] == string(schema.metadata.generation))) >=
  ((each.index / batchSize) + 1) * batchSize
```

```go
type ResourceGraphDefinitionSpec struct {
    // ... existing fields ...

    // PropagateWhen defines CEL expressions that allow the object to be mutated when true
    PropagateWhen []string `json:"propagateWhen,omitempty"`
}

type Resource struct {
    // ... existing fields ...

    // PropagateWhen defines CEL expressions that allow the object to be mutated when true
    PropagateWhen []string `json:"propagateWhen,omitempty"`
}
```

## Design Questions

### Should propagateWhen be a separate concept from readyWhen?

Yes.

On each reconciliation, we compute a topological ordering of the DAG. As we move through the
ordering, we check that the dependencies of the node are satisfied, ensure the desired state of the
node, compute the `readyWhen` condition for the node, and when true, move to the next node in the
ordering. When all nodes are ready, the graph evaluation is considered complete.

This currently happens serially, such that only one node is propagating at a time. It's
straightforward to extend this algorithm to operate in parallel, and necessary for propagation
control to be meaningful. In essence, the existing serial approach is a simple method of propagation
control, where only one resource is mutating at a time.

We introduce a new concept, `propagateWhen`, which controls when a node's mutation can start. This
is in contrast to `readyWhen` which controls when a node's mutation ends. It is essential for these
concepts to be decoupled because they serve fundamentally different purposes in the graph
evaluation. When multiple resources depend on the same resource, none of them can proceed until that
dependency is ready. However, each dependent resource may have a different `propagateWhen`, allowing
different subgraphs to proceed without blocking.

### Should create and update be treated the same?

Yes.

We posit that `propagateWhen` should apply equally to create and update. It's possible to argue that
the risk of a creation is lower than the risk of a mutation, and thus `propagateWhen` should be
ignored on creation. However, it's possible for creation to cause side effects to other resources.
For example, using
[KRO Decorators](https://github.com/ellistarn/kro/blob/krep/docs/design/proposals/decorators.md) to
create a NetworkPolicy for every Deployment in a namespace could cause traffic disruption to every
Deployment. In effect, the NetworkPolicy's creation is updating the behavior of the Deployment's
network. Cases like these make the risk profile of create equivalent to update, and thus, lead us to
apply `propagateWhen` to both cases.

### Should propagation control support automatic rollback?

No.

Propagation control will prevent mutation, but we explicitly and intentionally avoid rolling back
state automatically. Instead, we follow established declarative Kubernetes conventions (i.e.
Deployment Rolling Update) and simply halt propagation if `propagateWhen` remains false. Operators
may choose to alert when ResourceGraphDefinitions remain in a propagating state for too long (see:
[Status Conditions](#status-conditions)) and manually revert the desired spec to trigger the inverse
propagation. We may explore automation to detect and revert changes in a higher order abstraction,
but we defer this discussion to a future KREP.

### Should we allow overlapping propagations?

Probably.

Changes can be made to the inputs of the graph while other changes are still propagating through.
This is similar to Kubernetes deployments, which can be mutated mid-rollout. A common use case for
this is Rollback, described above. A mutation is made to the graph, and then the inverse of the
mutation is made before it completes. Overlapping propagations can be more complicated, with up to
O(n) simultaneous propagations where n is the number of nodes in the graph.

This problem is not specific to propagation control, and KRO solves it through serial execution of
the graph, though the mechanics do not fundamentally change with parallel execution. When multiple
propagations are in flight, the latest takes precedence and the graph is reevaluated from the root.
This is similar to Kubernetes Deployments, which pivot mid-rollout towards the desired state.

Propagation control must reason about overlapping propagations when determining
`exponentiallyUpdated` and `linearlyUpdated` built-in functions. When determining whether or not a
node in the graph can mutate, it's not enough to measure `readyWhen`, we must also consider whether
or not a resource is outdated with respect to the latest desired state of the graph.

Take the following double mutation:

```
Graph: A → B, C, D (collection with linearlyUpdated)

T1: Mutation   T2: Mutation Propagates   T3: Rollback Starts   T4: Rollback Propagates
      A'                A'                       A''                     A''
    / | \             / | \                    / | \                   / | \
   B  C  D           B' C' D                  B' C' D                 B'' C' D

```

Node D can propagate when node C is ready, according to the `linearlyUpdated` function. However,
node C is yet not updated to the latest version of the graph. Should `propagateWhen` allow us to
update D to D' or D'' or should we wait for C' to reach C'' before propagating the change? This
question fundamentally boils down to the number of concurrent propagations allowed in the graph.

Kubernetes Deployments allow a single concurrent propagation. This is a reasonable position for KRO
to take, but it is not without tradeoffs. With this approach, if propagations are made on an
interval T and each propagation takes > T, then the tail of the topological ordering will become
increasingly stale. One clear example of this is when using KRO to model software release pipelines.
Given an RGD for `SoftwareReleaseEnvironment` and a pipeline that deploys this environment to many
stages and regions, each of which are dependent on each other, it may take O(days) to propagate a
change. If the `SoftwareReleaseEnvironment` is updating regularly (i.e. O(hours) for CD use cases),
the changes will never fully propagate.

Allowing for multiple concurrent mutations would introduce additional complexity into graphs that
impact graphs that can complete faster, but it does not introduce bad behavior. For example, in our
rollback example above, we would rely on the result of `propagateWhen` to prevent a bad change from
continuing to progress through the graph. Multiple concurrent changes also results in increased
mutation compared to single concurrent mutation, but we consider this an acceptable tradeoff. We
could explore exposing this tradeoff to customers via a `propagationPolicy`, but we consider this
out of scope for this KREP.

Mechanically, supporting concurrent mutations will require new machinery in KRO. We defer the exact
details of this discussion to the implementation phase, due to the magnitude of the change.

Directionally, we could introduce a new `ResourceGraphRevision` CRD for each unique set of inputs to
the graph. Each `ResourceGraphRevision` would include a monotonically increasing `spec.number`
across revisions, allowing for the order of mutations to be determined. The inputs and the
topological sort for the revision would be persisted in the status of a new CRD, allowing for
accurate reconciliation of the revision. Each resource managed by the `ResourceGraphDefinition`
would be annotated with the `ResourceGraphRevision`, and reconciliation of the graph's propagations
would be oriented around `ResourceGraphRevision`. Before reconciling a resource, the
`ResourceGraphRevision` would abandon progress if the dependencies of a resource have a revision
number greater than their own revision number, allowing for a newer propagation to overtake and
replace an older one.

## Status Conditions

Per KREP-001, KRO's status conditions are as follows:

```
  Ready
  ├─ GraphResolved - Runtime graph has been created and resources resolved
  ├─ ResourcesReady - All resources in the graph are created and ready
  └─ InstanceManaged - Instance finalizers and labels are properly set
```

Above, we assert that readiness and propagation are separate concepts, and thus we introduce a
fourth status condition `ResourcesPropagated`. A resource is propagated when it has been mutated to
match the graph's latest revision. When true, this condition indicates that all changes have been
propagated through the graph, even if the resources themselves are not passing their `readyWhen`
checks. This is similar to how `Deployment` models ready and updated. Depending on our decision to
support overlapping propagations, we will need more granular condition to support update status
throughout the graph.

## Examples

### Control Propagation for an RGD

Gradually roll out changes to an application abstraction

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: application
spec:
  schema:
    apiVersion: v1
    group: example.com
    kind: Application
  propagateWhen:
    - ${ exponentiallyUpdated(resourceGraphs, each)} # resourceGraphs is a new keyword
    - ${ linearlyUpdated(resourceGraphs, each, 10) } # resourceGraphs is a new keyword
  resources:
    - id: deployment
      template: ...
    - id: service
      template: ...
```

### Deployment Blocker

Prevent deployments during maintenance windows:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: webapp
spec:
  schema:
    apiVersion: v1
    group: example.com
    kind: WebApp
  resources:
    - id: maintenance
      externalRef:
        apiVersion: v1
        kind: MaintenanceWindow
        metadata:
          name: maintenance
    - id: deployment
      propagateWhen:
        - ${maintenance.allowed}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: webapp-${each.name}
          namespace: ${each.namespace}
        spec: ...
```

### Incrementally Deploying a Pipeline

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: Pipeline
spec:
  schema:
    apiVersion: v1
    group: example.com
    kind: Pipeline
    spec:
      stages: ["beta", "gamma", "prod"]
  resources:
    - id: "releaseBlockers"
      externalRef:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: release-blockers
    - id: stages
      forEach: ${ spec.stages }
      propagateWhen:
        - "${ linearlyUpdated(stages, each, 1) }" # Check that the previous index is updated before proceeding
        - "${ releaseBlockers[stage] }" # Check per stage release blockers
      template:
        apiVersion: example.com
        kind: SoftwareReleaseEnvironment
        metadata:
          name: ${each.value}-environment
        spec:
          stage: ${each.value}
    - id: tests
      forEach: ${ stages }
      template:
        apiVersion: example.com
        kind: SoftwareReleaseEnvironmentTest
        spec:
          endpoint: ${ environment.endpoint }
```

## Discarded Design Ideas

### Modeling propagation control as a separate CRD

We could explore a PDB like CRD to separate propagation from the definition of the RGD. It's not
clear what the advantages of this approach would be and it introduces a mapping challenge between
RGD and the PropagationControl CRD. KRO will also need to handle the eventual consistency challenges
associated with mutation, as we lose an atomic definition of propagation with respect to the graph.
