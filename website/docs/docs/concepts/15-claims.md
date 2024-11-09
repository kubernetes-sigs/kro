---
sidebar_position: 15
---

# 3. Claims

Claims are a fundamental concept in **KRO** that represent instances of
ResourceGroups. They define the desired state of a set of resources, which KRO
continuously works to maintain.

## What is a Claim?

A Claim is a Kubernetes custom resource that:

- References a specific ResourceGroup
- Provides values for the parameters defined in the ResourceGroup
- Represents the desired state of a set of Kubernetes resources

## Anatomy of a Claim

Here's an example of a Claim for a `WebApplication` ResourceGroup:

```yaml
apiVersion: kro.run/v1alpha1
kind: WebApplication
metadata:
  name: my-web-app
spec:
  name: awesome-app
  replicas: 3
  image: nginx:latest
  ports:
    - port: 80
      targetPort: 8080
  env:
    DB_HOST: mydb.example.com
    LOG_LEVEL: debug
```

:::tip The spec fields of a Claim correspond to the parameters defined in the
ResourceGroup. :::

## The reconciliation loop

KRO manages Claims through a continuous reconciliation process:

- **Desired state detection**: KRO observes the Claim, which represents the
  desired state of resources.
- **Current state assessment**: KRO talks to the api-server and checks the
  current state of resources in the cluster related to the Claim.
- **Difference identification**: Any differences between the desired state
  (Claim) and the current state are identified.
- **State Reconciliation**: KRO takes necessary actions to align the current
  state with the desired state. This may involve creating, updating, or deleting
  resources as needed.
- **Status Updates**: The Claim's status is updated to reflect the current state
  of reconciliation and any issues encountered.
- **Continuous Loop**: This process repeats regularly, ensuring the cluster
  state always converges towards the desired state defined in the Claim.

## Advantages of declarative management [need better title]

- **Declarative Management**: Users define what they want, not how to get there.
- **Self-healing**: The system continuously works to maintain the desired state.
- **Idempotency**: The same Claim always results in the same end state,
  regardless of the current state.
- **Abstraction**: Complex resource relationships are managed behind the scenes.
- **Consistency**: All resources for an application are managed as a unit.
- **Auditability**: The Claim serves as a single source of truth for the
  application's desired state.

:::tip Best Practices

- Treat claims as declarative definitions of your application's desired state.
  Use version control for your Claims to track changes over time.
- Leverage labels and annotations in Claims for organization and filtering.
- Regularly review Claims to ensure they reflect current requirements.
- Use KRO's dry-run feature to preview reconciliation actions before applying
  changes to Claims.
- Monitor Claim statuses to understand the current state of your applications.
  :::

## Common Status Fields

KRO automatically injects two common fields into the status of all claims:
**Conditions** and **State**. These fields provide crucial information about the
current status of the claim and its associated resources.

### 1. Conditions

Conditions are a standard Kubernetes concept that KRO leverages to provide
detailed status information. Each condition represents a specific aspect of the
claim's state. Common conditions include:

- **Ready**: Indicates whether the claim is fully reconciled and operational.
- **Progressing**: Shows if the claim is in the process of reaching the desired
  state.
- **Degraded**: Signals that the claim is operational but not functioning
  optimally.
- **Error**: Indicates that an error has occurred during reconciliation.

Each condition typically includes the following properties:

- **Type**: The name of the condition (e.g., "Ready").
- **Status**: Either "True", "False", or "Unknown".
- **LastTransitionTime**: When the condition last changed.
- **Reason**: A brief, machine-readable explanation for the condition's state.
- **Message**: A human-readable description of the condition.

Example:

```yaml
status:
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2024-07-23T01:01:59Z"
      reason: ResourcesAvailable
      message: "All resources are available and configured correctly"
```

### 2. State

The State field provides a high-level summary of the claim's current status. It
is typically one of the following values:

- **Pending**: The claim is being processed, but resources are not yet fully
  created or configured.
- **Running**: All resources are created and the claim is operational.
- **Failed**: An error occurred and the claim could not be fully reconciled.
- **Terminating**: The claim is in the process of being deleted.
- **Degraded**: The claim is operational but not functioning optimally.
- **Unknown**: The claim's status cannot be determined.

Example:

```yaml
status:
  state: Running
```

These common status fields provide users with a consistent and informative way
to check the health and state of their claims across different ResourceGroups.
They are essential for monitoring, troubleshooting, and automating operations
based on the status of KRO-managed resources.
