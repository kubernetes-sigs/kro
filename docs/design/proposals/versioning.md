# KRO: Versioning and Rollouts

# Problem Statement

As the KRO project matures, platform teams (RGD authors) need a structured way to evolve their \`ResourceGraphDefinition\` (RGD) resources over time. Currently, any change to an RGD in a cluster is immediately propagated to all existing instances across all namespaces upon their next reconciliation. This "rollout-on-reconcile" behavior is fast but lacks the control necessary for some production environments. The goal is to allow RGD authors to introduce changes while giving control over how the changes are safely rolled out in the cluster. This aligns with GitOps principles, where version pinning and controlled automation are crucial for stability.

# Background

To understand the scope of the problems to be addressed, we need to understand a few things that will be used as inputs in the proposal.

## What can change in an RGD

When we say an RGD author makes a change, it could mean one or more of the following:

1. **Resource Adjustments:** Changes to `.spec.resources`:  
   1. Resources are added.  
   2. Resources are removed.  
   3. Resources are modified (e.g., changing a container image, updating a label).  
2. **Schema Evolution:** Changes to the `.spec.schema`:  
   1. Backward-compatible changes:  
      1. A new optional field is added.  
      2. A new field with a default value is added.  
      3. A default value is removed  
   2. Backward-incompatible changes:  
      1. A field is removed.  
      2. Adding/Changing defaults.  
      3. ~~A field is renamed. This is done by removing the old field and adding a new field.~~  
3. **CEL Environment Dependency:** The version of KRO determines the available CEL environments and functions.  
   1. An RGD author could change the CEL snippets used in RGD that require a different CEL environment.  
   2. KRO authors could fix CEL functions that would cause behavioural changes in the resulting resources.

## Challenges in current model

Key challenges with the current model include:

* **Lack of Controlled Rollouts:** There is no mechanism to gradually roll out RGD changes. All instances are updated at once, which is risky.  
* **No Version Pinning**: Application teams (instance authors) cannot pin their instances to a specific, known-good version of an RGD. This makes them vulnerable to breaking changes introduced by the platform team and complicates bug remediation.  
* **Handling Breaking Changes:** There is no formal process for managing backward-incompatible changes to an RGD's schema or resource composition.  
* **GitOps Integration:** The current model complicates GitOps workflows. GitOps relies on declarative state, but the effective state of an instance can change implicitly due to an RGD update, without any change to the instance's own manifest in git.

## Common questions

1. How should modifications to the ResourceGraphDefinition itself be managed ?  
2. Should RGDs explicitly reference a specific kro-cel environment version ?  
3. **Rollout Authority:** Who retains control over the rollout process: the infrastructure team (RGD author) or the application teams (instance authors)?  
4. **Rollout Methodologies:** What distinct rollout strategies should be supported (e.g., granular rollouts by namespace or label)?

# Proposal

We may have to pick one or more mechanisms based on the changes needed.

* We propose using the [Replicaset pattern](https://docs.google.com/document/d/1-9lWOJEDDN2Qsl6KVfrEbEZHgLm6Twk06L6-OsVJuho/edit?resourcekey=0-lLY3WBJ-0vlHfi5l0nypxg&tab=t.0#heading=h.mva37kvbhn51) for non-breaking schema changes. This allows authors to make changes without worrying about breaking users. Also allows users to select which version of RGD to use.  
* For breaking schema changes or behavioural changes, we recommend creating  a [new RGD with a different CRD](#new-rgd-for-the-change). We shall support migration tooling for such use cases. A Cli helper `kro migrate` can be added.

# Design Options

This section outlines various design options for implementing versioning. The discussion of these approaches aims to provide a better understanding of the available solutions, enabling a robust discussion that will inform the final design proposal.

## Representing RGD changes

In Kubernetes, RGD is an object. When any change is made to an RGD, we lose the previous configuration. There is no notion of the history of an object. Only the current view exists. 

To deal with this we have these options:

* **Single canonical RGD**: Only one instance of RGD exists, latest.  
* **Different RGD for each change**: Each change requires a new RGD and Instance CRD.  
* **Replicaset pattern**: Replicas automatically created and instances pinned to a replica  
* **Sections with RGD for each iteration number:** Schema and Resources are defined per iteration. Instances are pinned to a section.

## Replicaset Pattern

Introduce a new `ResourceGraphDefinitionReplica` CRD that is a point-in-time snapshot of a RGD.  This is automatically created and managed by KRO reconciler. Instances reconcile against a specific version of the replica.  
**Mechanism**

* Crucially, every modification to the RGD specification automatically triggers the creation of a new `ResourceGraphDefinitionReplica` by the KRO.   
* Instance reconciliation picks current `ResourceGraphDefinitionReplica` during first reconciliation and is pinned to that replica (via annotation).  
* We can have a mode where pinning is not done and all instances are updated to the latest replica always.  
* To mimic existing behavior we can set the history depth to 1 and enable no pinning mode where instances are always updated to the latest replica.

**Migration**

* **User-Controlled Opt-in:** Users have control over when they migrate their instance to a new version. Instance can be moved to a different version/replica by changing the annotation.  
* **Automated Migration:** A mechanism can be built to automate the migration process that changes the annotation. No special handling of resource migration is required since we are dealing with the same instance object.

**Pros**

* **Easy rollback**: This automated process ensures that a historical record of all RGD spec versions is maintained. Each `ResourceGraphDefinitionReplica` essentially represents a snapshot of the RGD at a particular point in time, encapsulating all the details of that specific version. This automatic generation is fundamental to enabling versioning and allowing for the tracking and potential rollback to previous configurations.  
* **Version Pinning:** During its initial reconciliation cycle, an instance will identify and select a `ResourceGraphDefinitionReplica` to which it will be "pinned." This pinning is achieved through an annotation applied to the instance. The primary purpose of this pinning is to ensure stability and predictability. Once an instance is pinned to a particular replica, it will continue to operate based on the definition provided by that specific `ResourceGraphDefinitionReplica`, even if newer replicas are subsequently created. This behavior is critical for preventing unexpected disruptions to running instances due to RGD spec changes. It allows for a controlled rollout of new RGD versions, where existing instances continue to use their original, validated configurations.  
* **Velocity:** While the default behavior emphasizes pinning for stability, the system also offers a configurable mode where this pinning is deliberately *not* performed. In this alternative mode, all instances are designed to continuously update themselves to the latest available `ResourceGraphDefinitionReplica`. This dynamic updating ensures that instances are always running with the most current RGD specification. This mode would be particularly useful in environments where rapid adoption of the latest RGD definitions is paramount, or for stateless services that can tolerate frequent configuration updates without significant impact. The choice between pinning and continuous updates provides flexibility, allowing users to select the behavior that best suits their application's requirements and deployment strategy.  
* **No Concept of immediate rollout**: Any change to a RGD results in a new replica. So automated rollout does not happen.

**Cons**

* **Breaking schema change handling:** If the schema has a breaking change across versions,  it would require conversion webhooks to be defined. There is no mechanism to do it in KRO.  
* 

## Single Canonical RGD

This approach maintains the current model where only one version of the RGD exists, which is always the most recently applied one. This simplifies reasoning, mirroring how most Kubernetes resources function, as changes are immediately visible across all RGD instances. The RGD contains a single schema definition and a single resources section.

**Mechanism**

* **Resource Updates:** Changes (additions, removals, modifications) are directly updated within the RGD's resource section.  
* **Schema Compatibility:** Only backward-compatible schema changes are allowed.  
* **Automatic Rollout:** When the RGD is modified, these changes are automatically rolled out across all instances upon reconciliation.

**Pros**

* **Simplicity:** This is the most straightforward method for managing RGD changes, easy to understand and implement.  
* **Single Source of Truth:** Maintains one definitive source for the RGD definition.  
* **Automatic Propagation:** Changes are automatically propagated, which is beneficial for rapid updates.

**Cons**

* **Uncontrolled Rollouts:** All instances update simultaneously, which can be risky, especially for critical applications. Users cannot pin to a specific version to avoid a buggy rollout.  
* **Irreversible Changes:** This model has a "unidirectional and irreversible" change model, which can be a drawback for rollbacks.  
* **Breaking schema change handling:** Handling backward-incompatible schema changes is challenging and likely necessitates downtime or complex migrations.  
* **No Version Pinning**: Since changes are immediately rolled out, there is no way to pin instances to a specific revision of RGD.

## RGD with version sections

This approach integrates versioning directly into a single RGD (Resource Group Definition) resource. A single RGD exists for multiple versions with sections within the RGD.

**Mechanism**

* The RGD `spec` will include a map or list of versions (e.g., `v1`, `v2`).  
* Each version will have its own schema and set of resources, as shown in the example below:

```
spec:
   - version: v1
     schema:
        # schema for v1...
     resources:
        # resources for v1...
   - name: v2
     schema:
       # schema for v2...
     resources:
       # resources for v2...
  servedVersion: v2
```

* When a new version is added to the RGD, the KRO controller will create a new version of the corresponding Custom Resource Definition (CRD) (e.g., `MyWebApp/v2`).  
* Instances are not automatically migrated. Users must explicitly change their instance's `apiVersion` (e.g., from `my.api/v1` to `my.api/v2`) to opt into the new version.  
* This option requires supporting a "migrate" lifecycle for instances and an "adoption" functionality for new instances.  
* Changes within a version are immediately applied across instances using that version.

**Migration**

* **User-Controlled Opt-in:** Users have control over when they migrate their instance to a new version.  
* **Automated Migration:** A mechanism can be built to automate the migration process. This would involve creating a new instance object at the new version, copying relevant fields, and deleting the old one. This requires careful handling of resource adoption and state.

**Pros**

* **Controlled Rollouts:** Users can adopt new versions at their own pace. Provides fine-grained control over when instances adopt new versions, crucial for critical systems or phased rollouts.  
* **Version Pinning**: Instances are by default pinned to a specific RGD. Migration is explicit.  
* **Clear Separation:** Versions are clearly defined and managed within a single RGD.  
* **GitOps Friendly:** Users can pin their instance to a specific version in their Git repository.  
* **No Impact on Existing Instances:** Introducing a new version has no effect on consumers of older versions.

**Cons**

* **Increased Complexity:** The RGD resource becomes more complex.  
* **Breaking schema change handling:** If the schema has a breaking change across versions,  it would require conversion webhooks to be defined. There is no mechanism to do it in KRO.  
* **CRD Management:** The controller needs to manage the lifecycle of multiple CRD versions.  
* **Migration Tooling:** Requires building robust mechanisms for instance migration and adoption.  
* Changes within a version are immediately applied across instances using that version.  
* **KRM Object size limitation**: With multiple versions in the same RGD, it is more likely to hit the KRM object size limitations.   
* **Immediate rollout within version**: Changes within a version are immediately rolled out with no control.  
* **Instance Migration for Minor Changes:** Instances need to be migrated to the new CRD even for minor changes in the RGD.  
* **Resource Abandonment and Adoption:** Resources managed by the old CRD must be abandoned and then adopted by the new CRD.

## New RGD for the change {#new-rgd-for-the-change}

This approach treats each version of a definition as a completely distinct ResourceGraphDefinition resource.

**Mechanism**

* To create a new version, the platform team generates a new RGD resource with a versioned name (e.g., `my-web-app-v1`, `my-web-app-v2`).  
* Each RGD (e.g., `my-web-app-v2`) defines its own Custom Resource Definition (CRD) (e.g., `MyWebAppV2`), with the CRD name itself being versioned.  
* Existing instances of the older RGD are entirely unaffected by the new version.  
* Users must create new instances using the new CRD to adopt the updated version.  
* This necessitates a well-defined migration strategy. This involves supporting a lifecycle for migrating an instance from one RGD/CRD to another, including the adoption of underlying resources.

**Migration**

* **User-Controlled Opt-in:** Users have control over when they migrate their instance to a new version.  
* **Automated Migration:** A mechanism can be built to automate the migration process. This would involve creating a new instance object at the new version, copying relevant fields, and deleting the old one. This requires careful handling of resource adoption and state.

**Pros**

* **Clear Separation:** Each version exists as a distinct, immutable resource, offering a very clean and easily understandable structure.  
* **No Impact on Existing Instances:** Introducing a new version has no effect on consumers of older versions.  
* **Version Pinning**: Instances are by default pinned to a specific RGD. Migration is explicit.  
* **Controlled Rollouts:** Users can adopt new versions at their own pace. Provides fine-grained control over when instances adopt new versions, crucial for critical systems or phased rollouts.  
* **GitOps Friendly:** Users can pin their instance to a specific version in their Git repository.  
* **Breaking schema change handling:** Since new CRD is created there is no concept of breaking changes in schema. It's a new CRD that we need to migrate to.

**Cons:**

* **Resource Proliferation:** This method can lead to a significant increase in the number of RGD and CRD resources within the cluster.  
* **Discovery Challenges:** Users may find it more difficult to discover the latest or recommended version of a definition, potentially requiring a new grouping mechanism.  
* **Migration Tooling:** Substantial effort may be required to develop tooling that supports instance migration and resource adoption.  
* **Impractical for Velocity for small changes:** This approach essentially makes an RGD immutable. A new RGD is created for every change, using a different Instance CRD. This can be impractical for RGD authors or organizations that prioritize rapid development.  
* **Instance Migration for Minor Changes:** Instances need to be migrated to the new CRD even for minor changes in the RGD. Making easy things harder.  
* **Resource Abandonment and Adoption:** Resources managed by the old CRD must be abandoned and then adopted by the new CRD.  
* **Immediate rollout within version**: Changes within a version are immediately rolled out with no control.

## Rollouts

Regardless of the chosen versioning strategy, it's critical to define who controls the rollout:

* **Infrastructure Team (RGD Author):** This team is responsible for defining and maintaining the RGD. They would have the authority to initiate and manage rollouts of RGD changes. This is suitable for scenarios where changes are broad and impact many users.  
* **User Teams (Instance Author):** These teams create and manage instances of the RGD. They would control when their specific instances adopt new versions, empowering them with autonomy over their deployments.

### Rollout Patterns

To accommodate diverse deployment needs, the following rollout patterns can be supported:

* **Rollout by Namespace:** This allows for rolling out changes to instances within specific Kubernetes namespaces, useful for phased rollouts across different environments or teams.  
* **Rollout by Labels:** This enables targeted rollouts based on labels applied to instances, providing flexibility for canary deployments, A/B testing, or rolling out to a subset of instances based on specific criteria.

### Other Considerations

* **CEL Environment:** How should changes in the CEL environment, tied to the KRO controller version, be handled? Should a field be added to the RGD spec to specify a required `kro-cel-env` version, allowing the controller to report unmet dependencies?

# Scope

This proposal outlines a comprehensive strategy for versioning ResourceGraphDefinitions (RGDs), encompassing both schemas and resources, and defining mechanisms for controlled rollouts of RGD changes. The goal is to optimize the user experience for both platform teams (RGD authors) and application teams (instance authors).

Key enhancements delineated in this proposal include:

* **RGD Structure Modification**: Adapting the RGD schema to incorporate a versioned list, where each version maintains its discrete schema and resources.  
* **Instance Reconciliation Logic Update**: Modifying the instance reconciliation process to align with the specific version defined within the instance's spec.  
* **RGD Controller Management**: Empowering the RGD controller to effectively manage multi-version Custom Resource Definitions (CRDs) in accordance with the RGD specification.  
* **Version Pinning Mechanism**: Establishing spec.version within the instance as the primary mechanism for version pinning and enabling user-driven rollouts.

# Out of Scope

Conversely, this proposal explicitly excludes the following aspects:

* **Automated Instance Migration**: This proposal does not cover a controller for automatic instance migration between versions. Such migrations will remain user-initiated, enabled by updates to the spec.version field.  
* **Advanced Rollout Strategies**: The development of a dedicated controller for sophisticated rollout methodologies (e.g., canary rollouts based on namespace or labels) is outside the current scope. However, this proposal establishes the essential foundational versioning mechanism upon which such advanced tooling could be built.  
* **Cross-RGD Resource Adoption**: Logic related to migrating or adopting resources between instances affiliated with distinct RGDs is not included.

# Testing Strategy Requirements

* A functional Kubernetes cluster, configured for deploying the modified KRO controller.  
* A suite of e2e test fixtures using chainsaw  
  1. Multi version RGD manifest(s).  
  2. Multiple instance manifests, each precisely targeting a distinct RGD version.  
* Kind cluster with several namespaces should suffice for testing

# Test Plan

The testing will proceed according to the following plan:

* **Unit Tests:**  
  1. Implement unit tests for the RGD controller logic to validate the accurate generation of CRDs for multi-version RGDs.  
  2. Develop unit tests for the instance reconciliation logic, ensuring it correctly selects the appropriate schema and resources based on the instance's spec.version.  
* **Integration Tests:**  
  1. Create an RGD containing two distinct versions, v1 and v2.  
  2. **Test 1:** Instantiate an instance specifying v1. Verify the correct creation of v1 resources.  
  3. **Test 2:** Instantiate an instance specifying v2. Verify the correct creation of v2 resources.  
  4. **Test 3:** Update the v1 instance to v2. Confirm that the underlying resources are accurately modified or migrated to align with the v2 definition.  
  5. **Test 4:** Create an instance without a specified version. Verify that it defaults to the RGD's designated default or storage version.  
  6. **Test 5:** Augment the RGD by adding a v3. Confirm that existing v1 and v2 instances remain unaffected until their spec.version is explicitly altered.
