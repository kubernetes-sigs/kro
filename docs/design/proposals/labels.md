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

We will introduce the following new labels, which will be defined in `@pkg/metadata/labels.go`:

1.  **Label for an instance being reconciled:**
    - **Existing Labels:**
      - kro.run/resource-graph-definition-id: 761e8fb7-14d5-4da1-b4f9-0a16fc334d6c
      - kro.run/resource-graph-definition-name: check-instance-creation
      - Conflicts with label used in creation path for nested RGD (https://github.com/kro-run/kro/pull/631)
    - **Proposed Label:**
      - `kro.run/reconciled-by-rgd`
    - **Value:** The name of the ResourceGraphDefinition (e.g., `my-rgd-name`).
    - **Purpose:** To indicate that an instance is being actively reconciled against a specific RGD. This label will be applied to the instance itself.
    - **Alternative Names:**
      - `kro.run/reconciling-rgd`
      - `kro.run/template`

2.  **Label for resources created during instance reconciliation:**
    - **Existing Labels:**
      - kro.run/resource-graph-definition-id: 761e8fb7-14d5-4da1-b4f9-0a16fc334d6c
      - kro.run/resource-graph-definition-name: check-instance-creation
    - **Proposed Label:**
      - `kro.run/defined-by-rgd`
    - **Value:** The name of the ResourceGraphDefinition (e.g., `my-rgd-name`).
    - **Purpose:** To identify which RGD was used as a template to create a resource during the reconciliation of an instance. This label will be applied to all resources created by the instance controller.
    - **Alternative Names:**
      - kro.run/created-by-rgd
      - kro.run/managed-by-rgd
      - kro.run/owner-rgd
      - kro.run/template-rgd

3.  **Labels for resources to link back to the instance:**
    - **Existing Label:**
      - kro.run/instance-id: 6e6a1d95-4855-4062-b51b-8f288cb96365
      - kro.run/instance-name: test-instance
      - kro.run/instance-namespace: default
    - **Proposed Labels:**
      - `kro.run/managed-by-instance-group`: The API group of the instance (e.g., `mygroup.example.com`).
      - `kro.run/managed-by-instance-kind`: The kind of the instance (e.g., `MyKind`).
      - `kro.run/managed-by-instance-namespace`: The namespace of the instance (e.g., `default`).
      - `kro.run/managed-by-instance-name`: The name of the instance (e.g., `my-instance`).
      - The `managed-by` label is a common pattern in the Kubernetes ecosystem.
    - **Purpose:** To provide a direct and queryable link from a created resource back to the instance that caused its creation. This set of labels uniquely identifies the instance.
    - **Alternative Name prefix:**
      - `kro.run/owner-`
      - `kro.run/created-by-`

## Other solutions considered

1. We could continue using the existing labels, but their purpose is not as explicit for relationship tracking, which can lead to confusion for users and client tools.
2. We could also use annotations, but labels are better suited for this purpose as they are queryable via the Kubernetes API, which is a key requirement for observability and tooling.

## Scoping

#### What is in scope for this proposal?

-  Defining and adding the new labels to the KRO API.
-  Updating the instance controller to apply these labels to instances and created resources during reconciliation.
-  Updating the official KRO documentation to reflect the new labels and their usage.
-  Adding docs reflecting the new labels. Old labels would not be documented.
-  Deprecate old labels in next minor release or the one after that.

#### What is not in scope?

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