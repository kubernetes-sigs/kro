# Resource lifecycles

## Problem statement

Resource lifecycles and ownership have a wide potential scope. Here are some user stories grouped by broad use case.

CRUD policies
- I want to run resources on deletion because I may want to snapshot a database before deleting it.
- I want to be able to update immutable fields in Kro because I want to change a job spec.
- I want to create a resource and never update it because some resources like PVC might be expensive or difficult to update.
- I want to only read a resource because I don't want to use external refs and want to reference the resource.
- I want to avoid deleting resources because some resources like databases are too risky to be deleted through Kro.
- I want to control deletion order because I may have implicit dependencies or a more complicated teardown workflow needed for resource cleanup.

Migration into Kro and between Kro resources
- I want to safely do migrations into Kro from existing resources because I want to manage my cluster in Kro and want to avoid downtime and risk of updating resources poorly.
- I want to safely do migrations between Kro RGDs or instances because I want to be able to refactor and evolve my Kro definitions over time and avoid downtime.

Singletons
- I want to define applications with resources that are defined only once across all Kro instances because resources like PrivateEndpoint are one per cluster.

Resource contribution
- I want to create a controller that updates a subset of fields on a resource because it is a useful pattern to control a small number of fields in Kubernetes.

## Proposal

The proposal is to add a new mode of operating resources
through an annotation called `kro.run/ownership=shared`.

Shared mode will be a new primitive that solves migrating
between Kro resources, Singletons, and resource contribution.

#### Overview

##### Exclusive

The default mode is exclusive. A Kro resource applied with exclusive
will cause errors for any other Kro instance trying to use it. Otherwise,
exclusive will work the same as it does today. Exclusive resources will
adopt resources from other systems but raise errors if they have Kro
applyset labels.

##### Shared

On create shared mode will error if the resource is exclusively owned. If it is not,
we will apply and start sharing ownership of the fields. In shared mode we will always
SSA with force=false. If we do not conflict we will just add ourselves as managing the field.

If we do conflict we will check if this is a terminal conflict that needs to be
surfaced to users. We consider conflicts terminal if we do not own the field and
another Kro field manager does. Otherwise, we consider these conflicts overridable.

| We own field | Another Kro owns field | Conflict Type |
|--------------|------------------------|---------------|
| true | false | No conflict |
| true | true | Overridable |
| false | false | Overridable |
| false | true | **Terminal** |

Terminal conflicts will be surfaced in the instance status condition until the conflict is resolved.

Our mechanism for overriding conflict will be a merge-patch editing the field
managers directly with a precondition checking that the resource version of
the object has not changed to avoid race conditions.

This mechanism ensures we can make updates to fields without leading into
deadlock. For example, if we have instance A and B apply the same object
with replicas=3, any attempt to update either A or B will run into a conflict.
If we just forced over the conflict then we would run into the two objects
fighting each other. When we update A with replicas=5, we will kick B out of
field ownership. When B tries to apply again it will see a conflict, realize
that it is no longer an owner of replicas and then report a terminal
conflict. Users would be expected to see this conflict and resolve it
on a case by case basis.

On delete, if there are no other owners we just delete the object as is. If
there are other owners we do an empty SSA releasing ownership of all of our
fields.

These semantics allow partial objects. With this you can define a Kro resource
that just changes a single field or a few fields.

These also allow singletons and shared dependencies between Kro installations.
Using these shared dependencies can accomplish migrating a resource safely
between Kro instances and RGDs.

#### Design details

There are two main complexities with this design

##### Label updates

We have a large amount of labels that only support a single Kro instance managing
the resource like `applyset.kubernetes.io/part-of={applySetID}`.

We need to keep the old labels around for compatibility but start adding additional
labels to be of the form `applyset-{applySetID}` to support multiple owners.

We will track an annotation called `internal.kro.run/migrated-to-new-labels` that we will look
at to know if we should use the new or old labels in label selectors.

##### Field manager

The actual parsing and updating of field managers is non-trivial. We will need to be very careful to
correctly parse the error message and be able to successfully update field managers.

## Scoping

#### What is in scope for this proposal?

This proposal addresses resource contribution, singletons, and migrating resources between Kro installations safely.

#### What is not in scope?

CRUD policies are not in scope. One could imagine modeling `delete: retain` as setting a dummy
owner like `applyset-retain: true`. This would not work however since our semantics for delete
include doing an empty SSA. This empty SSA would fail if there is a dummy owner that does not
own any fields.

Migrating into Kro is not in scope. To handle migration into Kro, a robust dry run would be ideal
to preview what would happen.

## Other solutions considered

The most controversial part of the solution will be handling conflicts.

There are a couple of alternatives that we could take to avoid fiddling with field managers manually.

The fundamental issue that makes this hard is SSA operates on a binary force=true or force=false. We really
want to force=true for fields we can overwrite and force=false for fields we don't own but another Kro instance owns.

### Accept some race conditions

Simplest idea could be

1. apply with force=false
2. if we run into conflicts check if they are all on fields we own, then apply with force=true

Issue is if object state is
```
replicas (owned by A, owned by B): 4
```

1. A attempts to apply replicas=5, image=aNewImage, fails with conflict over replicas
2. B successfully applies image=bNewImage
3. A only sees replicas conflict, then thinks it's good to force apply

With this we just overwrote another Kro's image field that we don't own. We only thought it was safe to
force because we saw the single conflict but there was another one that we would not have decided to override.
We don't thrash from here, A would own both fields and B would report a conflict
but, we don't have any strong guarantees in the system

### Apply twice

We could try and split the patch into overridable fields and apply with force=true while we
apply non overridable fields with force=false.

This is mainly listed to be exhaustive for possibilities but this solution feels like it adds complexity
with no strong guarantees.

## Testing strategy

#### Requirements

Testing will follow existing patterns using chainsaw for e2e tests. No special infrastructure needed.

#### Test plan

Unit tests will be added to cover changed code.

End-to-end tests will validate
- resource contribution example
- shared ownership with conflict cases
- shared ownership failing to use exclusive resource 
