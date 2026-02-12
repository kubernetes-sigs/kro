# Proposal: Standardize KRO Labels

## Problem statement

The current labels in KRO are used to track ownership and metadata of resources managed by the orchestrator. While these labels provide essential information, they lack a clear and standardized way to represent the relationships between different KRO components, such as ResourceGraphDefinitions (RGDs), instances, and the resources they generate. This ambiguity can make it difficult to query for related objects and understand the provenance of resources within a cluster.

Specifically, we need a clearer way to identify:
1.  Which RGD is responsible for reconciling a given instance.
2.  Which RGD a created resource originates from.
3.  Which instance a created resource belongs to.

This proposal aims to introduce new, more descriptive labels to clarify these relationships, aligning with Kubernetes best practices for resource management and observability.

## Proposal

We propose introducing a set of standardized labels to better represent the relationships between RGDs, instances, and the resources they manage. This will improve the ability to discover, manage, and debug resources created and managed by KRO.

#### Overview

The proposal is to add the following labels:
-   A label on an instance to indicate which RGD is reconciling it.
-   A label on resources created by an instance reconciliation to indicate which RGD they come from.
-   Labels on resources created by an instance reconciliation to indicate which instance created them.

This aligns with the common Kubernetes practice of using labels for discovery and organization of related resources, as seen in tools like Helm and Kustomize.

#### Design details

We will rename or introduce the following labels, which will be defined in `@pkg/metadata/labels.go`:

1.  **Labels for mapping created resources back to the instance:**
    - To provide a direct and queryable link from a created resource back to the instance that caused its creation. This set of labels uniquely identifies the instance.
    - **Proposed Labels:**
      - `kro.run/instance-gvk: mygroup.example.com/v1/MyKind`: The Group Version Kind (GVK) of the instance
      - `kro.run/instance: default/my-instance`: The namespace and name of the instance.
      - `app.kubernetes.io/instance: MyKind/default/my-instance`: Kind namespace and name of the instance
    - **Existing Labels**
      - `kro.run/instance-id: 6e6a1d95-4855-4062-b51b-8f288cb96365`
      - `kro.run/instance-name: test-instance`
      - `kro.run/instance-namespace: default`
2.  **Label for mapping created resources back to the RGD:**
    - To identify which RGD was used as a template to create a resource during the reconciliation of an instance. This label will be applied to all resources created by the instance controller.
    - **Proposed Labels:**
      - `kro.run/part-of: my-rgd-name` - The name of the ResourceGraphDefinition
      - `app.kubernetes.io/part-of: my-rgd-name`
    - **Existing Labels:**
      - `kro.run/resource-graph-definition-id: 761e8fb7-14d5-4da1-b4f9-0a16fc334d6c`
      - `kro.run/resource-graph-definition-name: my-rgd-name`
3. **Label for nested RGD instance:**
    - To differenciate the RGD that defines (and hence reconciles) the instance and the RGD that created it, we need to introduce a separate label.
    - Currently we dont differentiate, it results in a conflict (https://github.com/kro-run/kro/pull/631)
    - To indicate that an instance is being actively reconciled against a specific RGD. This label will be applied to the instance itself.
    - **Proposed Labels:**
      - `kro.run/managed-by-rgd: my-rgd-name` - name of the ResourceGraphDefinition that reconciles the instance.
    - **Existing Labels:**
      - `kro.run/resource-graph-definition-id: 761e8fb7-14d5-4da1-b4f9-0a16fc334d6c`
      - `kro.run/resource-graph-definition-name: my-rgd-name`
      - Conflicts with label used in creation path for nested RGD (https://github.com/kro-run/kro/pull/631)
4. **Kubernetes Common Labels:**
    - **Purpose:** Conforming to standard/common k8s labels allows easier integration across tools in k8s ecosystem.
    - **Proposed Labels:**
      - `app.kubernetes.io/managed-by: KRO` : Fixed value

## Other solutions considered

1. We could continue using the existing labels, but their purpose is not as explicit for relationship tracking, which can lead to confusion for users and client tools.
2. We could also use annotations, but labels are better suited for this purpose as they are queryable via the Kubernetes API, which is a key requirement for observability and tooling.
3. Use common k8s labels: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
  * `app.kubernetes.io/name:	mysql`
  * `app.kubernetes.io/instance: mysql-abcxyz`
  * `app.kubernetes.io/version: 5.7.21`
  * `app.kubernetes.io/component: database`
  * `app.kubernetes.io/part-of: wordpress`
  * `app.kubernetes.io/managed-by: Helm`


## Scoping

#### What is in scope for this proposal?

-  Defining and adding the new labels to the KRO API.
-  Updating the instance controller to apply these labels to instances and created resources during reconciliation.
-  Updating the official KRO documentation to reflect the new labels and their usage.
-  Adding docs reflecting the new labels. Old labels would not be documented.
-  Deprecate old labels in next minor release or the one after that.

#### What is not in scope?

- Tracking UID od RGD, Instance in the labels. We need to handle recreation of parent resources where UID changes. Can be a future enhancement.
- Supporting deeply nested RGDs. When RGD1 created RGD2 and that creates RGD3, do we add labels in objects of RGD3 linking it back to RGD2 and RGD1 ?
- Changing how KRO uses annotations.
- Changing these existing labels:
  - kro.run/kro-version: v0.4.0
  - kro.run/owned: "true"

## Testing strategy

#### Requirements

-   A running Kubernetes cluster to deploy KRO and test the label application.

#### Test plan

-   **Unit tests:** Add unit tests for any new labeler functions in `pkg/metadata/labels.go` to ensure they construct the correct labels.
-   **Integration tests:**
    -   Create an RGD and an instance of that RGD.
    -   Verify that the instance object has the `kro.run/reconciled-by` label pointing to the RGD.
    -   Verify that all resources created by the instance reconciliation have the `kro.run/created-by` label, and the `kro.run/instance-group`, `kro.run/instance-kind`, `kro.run/instance-namespace`, and `kro.run/instance-name` labels with the correct values.
    -   Update an instance to be reconciled by a different RGD and verify that the `kro.run/reconciled-by` label is updated accordingly.