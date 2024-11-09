---
sidebar_label: "ResourceGroup API"
sidebar_position: 100
---

# API Reference

## Packages

- [kro.run/v1alpha1](#krorunv1alpha1)

## kro.run/v1alpha1

Package v1alpha1 contains API Schema definitions for the x v1alpha1 API group

### Resource Types

- [ResourceGroup](#resourcegroup)
- [ResourceGroupList](#resourcegrouplist)

#### Condition

Condition is the common struct used by all CRDs managed by ACK service
controllers to indicate terminal states of the CR and its backend AWS service
API resource

_Appears in:_

- [ResourceGroupStatus](#resourcegroupstatus)

| Field                                                                                                                     | Description                                                                                                                                                                                                                                                                                           | Default | Validation        |
| ------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | ----------------- |
| `type` _[ConditionType](#conditiontype)_                                                                                  | Type is the type of the Condition                                                                                                                                                                                                                                                                     |         |                   |
| `status` _[ConditionStatus](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#conditionstatus-v1-core)_ | Status of the condition, one of True, False, Unknown.                                                                                                                                                                                                                                                 |         |                   |
| `lastTransitionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#time-v1-meta)_           | Last time the condition transitioned from one status to another.                                                                                                                                                                                                                                      |         |                   |
| `reason` _string_                                                                                                         | The reason for the condition's last transition.                                                                                                                                                                                                                                                       |         |                   |
| `message` _string_                                                                                                        | A human readable message indicating details about the transition.                                                                                                                                                                                                                                     |         |                   |
| `observedGeneration` _integer_                                                                                            | observedGeneration represents the .metadata.generation that the condition was set based upon.<br />For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date<br />with respect to the current state of the instance. |         | Minimum: 0 <br /> |

#### ConditionType

_Underlying type:_ _string_

_Appears in:_

- [Condition](#condition)

#### Definition

_Appears in:_

- [ResourceGroupSpec](#resourcegroupspec)

| Field                                    | Description | Default | Validation |
| ---------------------------------------- | ----------- | ------- | ---------- |
| `spec` _[RawExtension](#rawextension)_   |             |         |            |
| `status` _[RawExtension](#rawextension)_ |             |         |            |
| `types` _[RawExtension](#rawextension)_  |             |         |            |
| `validation` _string array_              |             |         |            |

#### Resource

_Appears in:_

- [ResourceGroupSpec](#resourcegroupspec)

| Field                                        | Description | Default | Validation          |
| -------------------------------------------- | ----------- | ------- | ------------------- |
| `name` _string_                              |             |         | Required: {} <br /> |
| `definition` _[RawExtension](#rawextension)_ |             |         | Required: {} <br /> |

#### ResourceGroup

ResourceGroup is the Schema for the resourcegroups API

_Appears in:_

- [ResourceGroupList](#resourcegrouplist)

| Field                                                                                                             | Description                                                                                                                                                                                                                                                                                                            | Default | Validation |
| ----------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                             | `kro.run/v1alpha1`                                                                                                                                                                                                                                                                                                     |         |            |
| `kind` _string_                                                                                                   | `ResourceGroup`                                                                                                                                                                                                                                                                                                        |         |            |
| `kind` _string_                                                                                                   | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |         |            |
| `apiVersion` _string_                                                                                             | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources       |         |            |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`.                                                                                                                                                                                                                                                        |         |            |
| `spec` _[ResourceGroupSpec](#resourcegroupspec)_                                                                  |                                                                                                                                                                                                                                                                                                                        |         |            |
| `status` _[ResourceGroupStatus](#resourcegroupstatus)_                                                            |                                                                                                                                                                                                                                                                                                                        |         |            |

#### ResourceGroupList

ResourceGroupList contains a list of ResourceGroup

| Field                                                                                                         | Description                                                                                                                                                                                                                                                                                                            | Default | Validation |
| ------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- | ---------- |
| `apiVersion` _string_                                                                                         | `kro.run/v1alpha1`                                                                                                                                                                                                                                                                                                     |         |            |
| `kind` _string_                                                                                               | `ResourceGroupList`                                                                                                                                                                                                                                                                                                    |         |            |
| `kind` _string_                                                                                               | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |         |            |
| `apiVersion` _string_                                                                                         | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources       |         |            |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.3/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`.                                                                                                                                                                                                                                                        |         |            |
| `items` _[ResourceGroup](#resourcegroup) array_                                                               |                                                                                                                                                                                                                                                                                                                        |         |            |

#### ResourceGroupSpec

ResourceGroupSpec defines the desired state of ResourceGroup

_Appears in:_

- [ResourceGroup](#resourcegroup)

| Field                                     | Description | Default | Validation          |
| ----------------------------------------- | ----------- | ------- | ------------------- |
| `kind` _string_                           |             |         | Required: {} <br /> |
| `apiVersion` _string_                     |             |         | Required: {} <br /> |
| `definition` _[Definition](#definition)_  |             |         | Required: {} <br /> |
| `resources` _[Resource](#resource) array_ |             |         | Optional: {} <br /> |

#### ResourceGroupState

_Underlying type:_ _string_

_Appears in:_

- [ResourceGroupStatus](#resourcegroupstatus)

#### ResourceGroupStatus

ResourceGroupStatus defines the observed state of ResourceGroup

_Appears in:_

- [ResourceGroup](#resourcegroup)

| Field                                               | Description                                                                 | Default | Validation |
| --------------------------------------------------- | --------------------------------------------------------------------------- | ------- | ---------- |
| `state` _[ResourceGroupState](#resourcegroupstate)_ | State is the state of the resourcegroup                                     |         |            |
| `topologicalOrder` _string array_                   | TopologicalOrder is the topological order of the resourcegroup graph        |         |            |
| `conditions` _[Condition](#condition) array_        | Conditions represent the latest available observations of an object's state |         |            |
