# Resource lifecycles

## Problem statement

Resource lifecycles and ownership have a wide potential scope. A further discussion on other potential items is listed in the not-in-scope section.

This document addresses the following user stories:
- I want to avoid deleting resources because some resources like databases are too risky to be deleted through Kro.
- I want to safely do migrations into Kro from existing resources because I want to manage my cluster in Kro and want to avoid downtime and risk of updating resources poorly.
- I want to safely do migrations between Kro RGDs or instances because I want to be able to refactor and evolve my Kro definitions over time and avoid downtime.

## Proposal

The proposal is to let adoption be handled by the current behaviour.
The current behaviour applies over other resources as long as they
are not being managed by a Kro instance.

To avoid deleting resources we will add a new `lifecycle` field that accepts a CEL expression:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
spec:
  resources:
    - id: database
      lifecycle: "${policy().withRetain()}"
```

The `policy()` function provides methods to configure lifecycle behavior:
- `policy().withRetain()` - Retain resource when instance is deleted
- `policy().withDelete()` - Delete resource (default)

The CEL expression evaluates to a map. Static maps are also supported but not recommended to avoid future breakages:
```yaml
lifecycle:
  deletePolicy: retain  # or "delete"
```

For migration or refactoring use cases, you would add the delete policy field,
then delete or update the RGD to orphan the resources, and finally adopt them
into another RGD or instance. 

#### Overview

##### Adoption

While adopting resources and migrating them safely is a major concern, 
this is a subset of the more general problem of trying to understand
what updates to Kro resources will update your cluster state. We will 
recommend using future CLI commands 
like `kro preview` to help customers understand the actions that will be taken on their cluster.

The general workflow will be to design your RGD and run the CLI command `kro preview` to
go through an interactive diff of the resources the RGD will expect to create.
The user will be responsible for understanding and confirming this diff is correct.

There will be some RGDs that will be non-deterministic (a job that fails if rand() > 0.5), 
but these will be the minority of RGDs, especially in migration use cases. There is also 
the potential that you run a dry-run and the cluster state changes when you go to apply the resource.
This is not a unique problem to Kro adoption. In system migrations it is typical to avoid making
updates to resources that are in the process of undergoing a migration.

The goal here is to leverage an already planned feature of the CLI 
and enable safer adoption in the majority of cases. 

We will update documentation to clarify this as a stated feature of Kro.

##### Orphan

For the purpose of orphaning, we consider both deleting an RGD or instance and 
pruning (where a resource is deleted because the instance or RGD definition no longer requires it) 
to apply the same mechanism. 

When orphaning, we will evaluate the lifecycle expression and check if deletePolicy is set to retain, 
then remove Kro-applied labels instead of deleting the object.

We will leave the Kro field manager for simplicity. Parsing and patching out the managed field
is extra complexity. Leaving the field manager also gives clarity about what Kro contributed to
the object. It will require other systems to run with force=true but this is pretty common, Kro 
does it.

CEL expressions that reference the schema will be allowed. This will allow creating 
non-prod vs production RGDs that may have different requirements on retaining resources.
We will avoid allowing dependencies to other resources for simplicity initially.

Example with conditional CEL expression:
```yaml
resources:
  - id: database
    lifecycle: '${schema.spec.environment == "production" ? policy().withRetain() : policy()}'
    template:
      apiVersion: v1
      kind: PersistentVolumeClaim
      # ...
``` 

##### Migration

An example migration use case is refactoring your RGDs. You may have 
initially bundled a webserver and a backend API into one RGD but now want to organize
them separately so different teams can own their own infrastructure.

You may have more complicated migrations. You may have an application that was an instance
per namespace and want to refactor it to be a single instance utilizing collections. 

Example migration steps
```yaml
# Step 1: Add retain to old RGD
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
spec:
  resources:
    - id: database
      lifecycle: "${policy().withRetain()}"

# Step 2: Delete old RGD (resources get orphaned)
kubectl delete rgd monolith

# Step 3: Create new RGDs that reference the same resources
# Kro will adopt them since they're no longer managed
```

## Other solutions considered

### Adoption safety policies

There are various levels of safety we could build into Kro for adoption. We could add policies like `AdoptOrCreate`,
`AdoptOnly`, `CreateOnly` to give finer control over adopt behaviour. 

The issue with these policies is that while they do provide some safety, they are drastically less safe than 
a preview command like `kro preview`. The preview command would let you understand not just if resources will be
created but also if any updates are potentially disruptive. If you successfully adopt a database resource, but during 
that adoption set memory to 10 bytes, you just caused an outage.

### Adoption matching

We could guarantee that adopted resources will not change initially. We could do a Kubernetes server side 
dry run for resources with an `Adopt` policy and try to validate that there will be no differences.

Performing the diff will be non-trivial since we will intend to make some updates like adding labels.
This approach is also not perfect, admission webhooks with side effects could be skipped. We would also run into
an issue if any updates happen between our dry run and applying the object.

This idea has merit and could be a potentially valuable feature. However, this would be 
prematurely solving the problem of adoption safety. We should see gaps from the CLI preview
command before adding this complexity.

### CRD field vs annotation

We could choose an annotation `alpha.kro.run/delete-policy: retain` instead of a CRD field.

Choosing an annotation over a field has the following benefits:
1. A CRD field is harder to make breaking changes to. An alpha annotation will allow us to evolve the project quicker and easier than committing to an RGD CRD field now.
2. Annotations are more easily understood by the Kubernetes ecosystem. It would be very easy for a platform team to automatically add annotations or require all database CRs have this annotation present.
3. Easier to inspect and have confidence a resource will not be deleted.
4. Allows templating mechanisms to transfer over.

The reason to use a CRD field instead of an annotation is type safety. It would be too easy for a customer to accidentally 
typo the annotation then delete their production database. We could partially type check the annotation
but nothing could stop a user from accidentally specifying something like `rko.run`. 

### Static CRD fields vs CEL library

We could have defined lifecycle as a static CRD schema field instead of a CEL expression evaluated by the `policy()` library.

Using CEL allows adding new policy behaviors by extending the library without requiring CRD schema migrations.

### Top level

A potential enhancement would be to orphan all resources in an RGD. We could add a top level policy field then allow overriding
at the lower level. This is reasonable but not initially necessary. It is very possible the idea of defaulting CRD fields
per RGD will be solved more generally (users already want to default a list of labels to apply to everything).

## Scoping

#### What is in scope for this proposal?

The scope of this proposal is minimal; all this document proposes is adding the lifecycle field with policy() CEL function support.

#### What is not in scope?

##### kro CLI

While the kro CLI preview is referenced, exact details will be left to the CLI KREP.

##### Other CRUD policies

- I want to run resources on deletion because I may want to snapshot a database before deleting it.
- I want to be able to update immutable fields in Kro because I want to change a job spec.
- I want to create a resource and never update it because some resources like PVC might be expensive or difficult to update.
- I want to only read a resource because I don't want to use external refs and want to reference the resource.
- I want to control deletion order because I may have implicit dependencies or a more complicated teardown workflow needed for resource cleanup.

These use cases will need to be tackled separately. 

##### Singletons and resource contribution

- I want to define applications with resources that are defined only once across all Kro instances because resources like PrivateEndpoint are one per cluster.
- I want to create a controller that updates a subset of fields on a resource because it is a useful pattern to control a small number of fields in Kubernetes.

A separate KREP will address these. 

## Testing strategy

#### Requirements

Testing will follow existing patterns using chainsaw for e2e tests. No special infrastructure needed.

#### Test plan

Unit tests will be added to cover changed code.

End-to-end tests will validate
- orphaning resources with policy().withRetain()
- validating we can adopt a resource that has been orphaned
- pruning behavior respects lifecycle policies