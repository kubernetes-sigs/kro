# Resource lifecycles

## Problem statement

The problem we are trying to solve are resource adoption and resource orphaning. 

The goal of resource adoption is to enable customers to safely migrate resources created outside Kro to be managed by Kro. For example, you may have run `kubectl apply -f ...` on a database CR but now want to have Kro manage this resource without causing downtime or impact to existing services. The customer in this case would want the instance to error if it is not found instead of creating a duplicate resource. Customers may want to manage resources under Kro to enable things like RGD configuration updates to propagate to the resource.

The goal of resource orphaning is to allow customers to leave resources behind. An example could be you may want to switch from a Kro controller per namespace to a single Kro installation per cluster. You would want to Orphan resources to allow the other Kro installation to manage the resources.

## Proposal

The proposal is to add a `resourcePolicy` string enum option to the resource configuration with various configuration options.
```
resourcePolicy: Owner | Adopt | Orphan 
```

#### Overview

The default resource policy will be `Owner`. This matches the current behaviour today. 

`Adopt` will avoid creating resources and will take ownership of existing resources and manage them as if kro had created them. `Adopt` will throw an error if resources do not exist.

`Orphan` allows the instance to be deleted without deleting certain resources. Kro removes the ApplySet ownership labels, leaving the resources orphaned and unmanaged.

We will support configuring these with cel expressions. For example, a schema option can be used on the RGD to determine if the instance should be `Adopt` or `Owner`.
```
resources:
  - id: database
    kind: RDSInstance
    resourcePolicy: ${has(schema.spec.adoptResource) ? "Adopt" : "Owner"}
```

We would only initially support referencing schema fields in the cel expressions analogous to how `includeWhen` works today.

#### Design details

We would need to update the Resource config option to take in `resourcePolicy`. 

##### Adopt 

Currently, Kro handles most of the adoption use case. If you have an instance point to a preexisting Kubernetes object, Kro will take ownership of it. The current behaviour is essentially `AdoptOrCreate`.

To get the `Adopt` semantics, we will need to update the reconciliation loop to throw an error if the resource does not exist and has `resourcePolicy: Adopt`.

The one exception to this resource adoption is if another Kro instance is managing the resource. Then a `resource belongs to a different ApplySet` will be thrown. This is not planned to be changed to prevent Kro installations fighting over a resource.

#### Orphan

The only difference for resources with `Orphan` is deletion will remove ApplySet labels with an update call instead of deleting the Kubernetes resource.

When an instance with `resourcePolicy: Orphan` is deleted, Kro will remove the `applyset.kubernetes.io/part-of` label from the resource and skip issuing the delete call. The resource remains in the cluster without any ownership labels.

Removing the ownership labels allows other Kro instances to take ownership of this.

## Scoping

#### What is in scope for this proposal?

The scope for this proposal is just the `Orphan` and `Adopt` use case.

#### What is not in scope?

Resource lifecycles is a very broad concept with many directions we could take.

Here are some related problems that could provide value if solved but this design does not try to address.

##### adoption checking ownership with other systems

One situation that may come up is you adopt a pod that is being managed by a replicaset. This may lead to both the replicaset and Kro making updates to this resource.

This will be left up to the user to validate anything they adopt is not going to be managed by other resources in unexpected ways.

##### adoption doesn't cause updates

One problem could be you may want to have a migration of resources without any changes to those resources. To do this you may want a feature where you ensure a resource can be adopted without making any updates to the field.

This is out of scope to avoid adding extra complexity into this feature.

##### updatePolicy

Some resources like job spec are immutable and can not be updated by a typical patch command. Instead of issuing an update, a user may want to have the option of configuring a resource to be deleted and recreated.

A potential design could have considered this as a part of the resource policy.

For the given proposal `updatePolicy` does not fit in clearly and would end up being a separate field and topic.

##### createOnDelete

A useful option for Kro would be the ability to create resources to help clean up resources. An example could be creating a job that creates a database backup before the database is deleted.

This is a much larger item and would require having sub RGD graphs. This also does not fit in cleanly to the suggested design. The suggested resource policy does not prohibit taking an approach like Helm hooks.

##### Additional resource policies

This design does allow for the expansion of additional resource policies.

One idea discussed before is `SharedOwnership`. This would effectively be a singleton. You may have resources that should be created at most once per cluster. `PrivateEndpoint` is an example of this. Multiple instances would point to the same object and only create it if the object does not exist. They would only delete it when they are the last instance to manage it. This feature should be implemented eventually but carries enough complexity that decoupling this from this design is needed.

Another option could be `watcher`. This has significant overlap with externalRefs so is avoided for now. In the future we could consider deprecating externalRef and making `watcher` solve this use case.

## Other solutions considered

### Naming

The naming `Owner | Adopt | Orphan` has one noun and two verbs. This is to avoid the awkward label of `Own`. An alternative would be to name them all verbs as `Own | Adopt | Orphan`.

An alternative would be to name them all nouns as `Owner | Adopter | Orphaner` which may be unnecessarily verbose.

### Granular controls

An alternative would be letting users configure these options at a lower level.
```
lifecycle:
  # Create phase: How to obtain the resource
  # - Create (default): Create new resource
  # - Never: Error if resource does not exist, claim ownership (adoption), 
  create: Create | Never
  
  # Update phase: How to handle spec changes
  # - Patch (default): Apply changes via server-side apply
  # - Recreate: Delete and recreate resource with new spec
  # - Never: Don't update, error if spec changes (TODO is error here right)
  update: Patch | Never
  
  # Delete phase: What to do when instance is deleted
  # - Delete (default): Remove the resource
  # - Never: Don't delete (orphan the resource)
  delete: Delete | Never
```

An advantage to this design is it more natural to extend this to support more use cases.

Immutable resources can be handled with a new update policy. 
```
lifecycle:
  update: DeleteAndRecreate
```

Resources requiring resources to be created on deletion (for example a database which creates a job to run a backup) could be a delete policy.
```
lifecycle:
  delete: PostDeleteHook
PostDeleteHook:
  ... jobDefinition here
```

This design does have merit but a major pitfall is usability. Valid use cases like adoption can be expressed with `create: never`. Questionable use cases can also be expressed like 
```
lifecycle:
  create: Never
  update: Never
  delete: Delete
```

It is not clear why someone would want to have a resource that can't be updated and only deleted. It would be very difficult for a user to understand what is going on in these misconfigured use cases, especially since this could be configured on just a single resource in an RGD. The signal for resources not being changed would not be easy to surface to users. Named policies limit users to sensible policies and avoid having them run into a footgun.   

It's possible we could add validation and throw errors to prevent footgun cases, but this ends up having the configuration options be equivalent to having named policies with a less clear user interface.

Another disadvantage is that `SharedOwnership` doesn't fit this model. SharedOwnership requires reference counting across multiple owners, not just per-resource create/update/delete decisions. This would need a fundamentally different mechanism beyond lifecycle phase configuration.

Another option could be to allow both named policies and the low level configuration option. Having two ways to configure the same thing adds extra complexity. Focusing on one way that handles all the use cases we want to support is preferable.

## Testing strategy

#### Requirements

Testing will follow existing patterns using chainsaw for e2e tests. No special infrastructure needed.

#### Test plan

Unit tests will be added to cover changed code.

End to end tests will validate
- failing to adopt a resource because it is missing
- successfully adopting a resource
- validating the adopted resource can be updated
- orphaning a resource
- validating that another instance can adopt this resource
- failing to adopt a resource owned by another instance

## Discussion and notes

### Other tools allowing configurable resource policy

**Helm**
- `helm.sh/resource-policy: keep` - Annotation prevents deletion during uninstall/upgrade/rollback, orphans resource
- `helm.sh/hook-delete-policy: before-hook-creation | hook-succeeded | hook-failed` - Controls hook resource cleanup
- Adoption via annotations: `meta.helm.sh/release-name`, `meta.helm.sh/release-namespace`, `app.kubernetes.io/managed-by: Helm`

**ArgoCD**
- `argocd.argoproj.io/sync-options: Prune=false` - Prevent resource deletion when removed from Git
- `argocd.argoproj.io/sync-options: Delete=false` - Retain resource after Application deletion (orphan)
- `PrunePropagationPolicy: foreground | background | orphan` - Controls deletion propagation
- Auto-prune disabled by default, requires explicit enablement

**Crossplane**
- `deletionPolicy: Delete | Orphan` - Controls deletion behavior
- `managementPolicies: ["Create", "Update", "Delete", "LateInitialize"]` - Controls which lifecycle phases are active
- Supports resource adoption via `spec.forProvider` matching

**ACK (AWS Controllers for Kubernetes)**
- `services.k8s.aws/deletion-policy: delete | retain` - Annotation-based deletion control
- Supports per-resource, per-namespace, or controller-wide configuration
- Resource adoption via `services.k8s.aws/adoption-fields` annotation

**Terraform**
- `lifecycle.create_before_destroy` - Create replacement before destroying original
- `lifecycle.prevent_destroy` - Block resource deletion entirely
- `lifecycle.ignore_changes` - Ignore drift in specific attributes