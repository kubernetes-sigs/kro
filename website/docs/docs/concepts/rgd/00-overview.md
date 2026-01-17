---
sidebar_position: 1
sidebar_label: Overview
---

# Overview

A **ResourceGraphDefinition** (RGD) lets you create a custom Kubernetes API that deploys multiple resources together as a single unit. It's the only API you need to configure kro - you define the schema for your new API, the resources it should create, and how data flows between them using CEL expressions.

When you apply an RGD, kro configures itself to serve your new API. It generates a CRD, registers it with the Kubernetes API server, and starts watching for instances. When users create instances of your API, kro creates the underlying resources in the correct order, wires values between them, and manages their full lifecycle.

## How it Works

1. **You create an RGD** defining a new API (like `Application`)
2. **kro generates a CRD** for your API
3. **Users create instances** of your API
4. **kro creates and manages** all the underlying resources

<div style={{textAlign: 'center', marginTop: '2rem', marginBottom: '2rem'}}>

<img src="/img/overview-diag.svg" alt="kro RGD Flow" style={{maxWidth: '600px', width: '100%'}} />

</div>

## Example

Here's an RGD that creates a new `Application` API. When users create an `Application`, kro automatically creates a Deployment:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: application
spec:
  schema:
    apiVersion: v1alpha1
    kind: Application
    spec:
      name: string
      image: string | default="nginx:latest"
      replicas: integer | default=3

  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.spec.name}
          template:
            metadata:
              labels:
                app: ${schema.spec.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
```

Users can now create applications:

```yaml
apiVersion: v1alpha1
kind: Application
metadata:
  name: my-app
spec:
  name: my-app
  image: nginx:1.27
  replicas: 5
```

kro will create and manage the Deployment automatically.

## Anatomy of a ResourceGraphDefinition

An RGD has the standard Kubernetes resource structure:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata: {}      # Standard Kubernetes metadata
spec:
  schema: {}      # Define your custom API
  resources: []   # Define Kubernetes resources to create
status:           # Managed by kro
  conditions: []
  state: ""
  topologicalOrder: []
```

- **`metadata`** - Standard Kubernetes metadata (name, labels, annotations)
- **`spec.schema`** - Your custom API: what fields users can configure and what computed values to expose
- **`spec.resources`** - The Kubernetes resources to create and manage (Deployments, Services, etc.)
- **`status`** - Current state managed by kro: conditions, overall state, and computed creation order

For complete field documentation, see the [RGD API Reference](../../../api/crds/resourcegraphdefinition.md).

## How kro Processes RGDs

kro performs extensive static analysis to catch errors **before deployment**. Rather than discovering issues when users create instances, kro validates everything upfront when you create the RGD. Type errors, non-existent fields, missing CRDs, syntax issues, and circular dependencies are all caught during RGD creation, giving you immediate feedback.

<div style={{textAlign: 'center', margin: '3rem 0'}}>

![RGD Validation Flow](/img/rgd-validation.svg)

</div>

### Static Analysis and Validation

kro validates your RGD **before any instances are created**. When you create a ResourceGraphDefinition:

1. **Schema validation** - Ensures your schema follows the [SimpleSchema](../../../api/specifications/simple-schema.md) format
2. **CRD verification** - Validates that all referenced resource types (Deployments, Services, etc.) exist in your cluster
3. **CEL type checking** - Parses and validates all [CEL expressions](./03-cel-expressions.md), checking that:
   - Referenced fields exist in the actual resource schemas
   - Expression output types match their target field types
   - All expressions are syntactically correct
4. **Dependency inference** - Automatically analyzes CEL expressions to [infer resource dependencies](./04-dependencies-ordering.md) and compute the creation order

This validation happens at RGD creation time, catching errors early before any user creates an instance. For a deep dive into how this works, see [Static Analysis](./05-static-type-checking.md).

### Automatic Dependency Management

kro analyzes your CEL expressions to automatically infer dependencies between resources. For example, if a Service references `${deployment.metadata.name}`, kro knows the Deployment must be created first. This dependency graph is used to:

- **Compute creation order** - Resources are created in topological order
- **Compute deletion order** - Resources are deleted in reverse topological order
- **Detect circular dependencies** - kro rejects RGDs with circular dependencies
- **Show the order** - The computed order appears in `status.topologicalOrder`

See [Dependencies & Ordering](./04-dependencies-ordering.md) for details on how this works.

### Generated CRDs and Controllers

When your RGD is validated and accepted:

1. **CRD generation** - kro generates a CustomResourceDefinition for your API and registers it with the Kubernetes API server
2. **Dynamic controller** - kro configures itself to watch for instances of your new API
3. **Lifecycle management** - The controller creates resources in dependency order, evaluates CEL expressions, manages status updates, and handles deletion

### RGD Status Conditions

kro reports the RGD's state through three conditions in `status.conditions`:

| Condition | Description |
|-----------|-------------|
| **ResourceGraphAccepted** | Whether the RGD spec passed validation. If `False`, the `message` field contains the validation error. |
| **KindReady** | Whether the CRD for your custom API has been generated and registered with Kubernetes. |
| **ControllerReady** | Whether kro is actively watching for instances of your custom API. |

When all three conditions are `True`, the RGD is fully operational and ready to accept instances. You can check the status with:

```bash
kubectl get rgd <name> -o yaml
```

For complete status field documentation, see the [RGD API Reference](../../../api/crds/resourcegraphdefinition.md).

## What RGDs Provide

- **Type safety**: All CEL expressions are validated when you create the RGD
- **Dependency management**: kro automatically determines the order to create resources
- **Validation**: Users get immediate feedback if they provide invalid values
- **Reusability**: Define once, use many times across teams

## Next Steps

Explore the details of ResourceGraphDefinitions:

- **[Schema](./01-schema.md)** - Define your custom API structure
- **[Resource Basics](./02-resource-definitions/01-resource-basics.md)** - Define resources with CEL expressions
- **[Conditional Creation](./02-resource-definitions/02-conditional-creation.md)** - Create resources conditionally with `includeWhen`
- **[Readiness](./02-resource-definitions/03-readiness.md)** - Control when resources are considered ready
- **[Collections](./02-resource-definitions/04-collections.md)** - Create multiple resources with `forEach`
- **[External References](./02-resource-definitions/05-external-references.md)** - Reference resources outside your RGD
- **[CEL Expressions](./03-cel-expressions.md)** - Reference data between resources
- **[Dependencies & Ordering](./04-dependencies-ordering.md)** - How kro infers dependencies and determines creation order
- **[Static Type Checking](./05-static-type-checking.md)** - How kro validates RGDs before instances are created
