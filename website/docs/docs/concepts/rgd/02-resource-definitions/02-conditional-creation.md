---
sidebar_position: 2
---

# Conditionals

Not all resources in a ResourceGraphDefinition need to be created for every instance. Sometimes you want to create resources conditionally based on user configuration - like enabling monitoring, backups, or TLS only when requested.

kro provides the `includeWhen` field to make resources optional. When you add `includeWhen` to a resource, kro evaluates the conditions when an instance is created and only includes the resource if all conditions are true.

## Basic Example

Here's a simple example where an Ingress resource is only created if the user enables it in their instance spec:

```kro
resources:
  - id: ingress
    includeWhen:
      - ${schema.spec.ingress.enabled}
    template:
      apiVersion: networking.k8s.io/v1
      kind: Ingress
      metadata:
        name: ${schema.spec.name}
      # ... rest of ingress configuration
```

When a user creates an instance:

```kro
apiVersion: example.com/v1
kind: Application
metadata:
  name: my-app
spec:
  name: my-app
  ingress:
    enabled: true  # Ingress will be created
```

If `ingress.enabled` is `false` or not provided, the Ingress resource is skipped entirely.

## How includeWhen Works

`includeWhen` is a list of CEL expressions that control whether a resource is created:

- If **all** expressions evaluate to `true`, the resource is included
- If **any** expression evaluates to `false`, the resource is skipped
- Each expression must evaluate to a **boolean** value (`true` or `false`)

kro extracts dependencies from these expressions and adds them to the dependency graph during graph construction. At runtime, `includeWhen` expressions are evaluated with access to all dependent resources.

## What You Can Reference

`includeWhen` expressions can reference both `schema.spec` and other resources in the graph by their `id`:

```kro
# ✓ Valid - references schema.spec and returns boolean
includeWhen:
  - ${schema.spec.ingress.enabled}
  - ${schema.spec.environment == "production"}
  - ${schema.spec.replicas > 3}
```

```kro
# ✓ Valid - references another resource by ID
resources:
  - id: app
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        labels:
          environment: production

  - id: loadbalancer
    includeWhen:
      - ${app.metadata.labels.environment == 'production'}
    template:
      apiVersion: networking.k8s.io/v1
      kind: Ingress
```

```kro
# ✗ Invalid - must return boolean
includeWhen:
  - ${schema.spec.appName}  # returns string, not boolean
```

When an expression references a resource by `id`, kro automatically creates an implicit dependency on that resource. kro validates `includeWhen` expressions when you create the ResourceGraphDefinition, ensuring referenced resource IDs exist and expressions return boolean values.


## Dependencies and Skipped Resources

When a resource is skipped (due to `includeWhen` conditions), **all resources that depend on it are also skipped**.

Referencing a resource in `includeWhen` creates an implicit dependency on that resource. If the referenced resource is skipped, any resource that depends on it (implicitly or explicitly) is also skipped.

<div style={{marginTop: '3rem', marginBottom: '3rem'}}>

![Dependency graph showing how skipped resources affect their children](/img/dep.svg)

</div>

In this example, the Deployment on the left has `includeWhen: ${true}`, so it's included along with all its children (Service and Ingress). The Deployment on the right (shown in red) has a condition that evaluates to false, so it and all its dependencies are skipped.

This ensures that the resource graph remains consistent and prevents resources from referencing other resources that don't exist.

## Multiple Conditions

All conditions in `includeWhen` must be true (logical AND):

```kro
resources:
  - id: certificate
    includeWhen:
      - ${schema.spec.ingress.enabled}
      - ${schema.spec.ingress.tls}
    template:
      apiVersion: cert-manager.io/v1
      kind: Certificate
      # ... certificate config
```

The certificate is created **only if**:
- `ingress.enabled` is `true` **AND**
- `ingress.tls` is `true`

For OR logic, combine in a single expression:

```kro
includeWhen:
  - ${schema.spec.env == "staging" || schema.spec.env == "production"}
```

## Next Steps

- **[Readiness](./03-readiness.md)** - Control when resources are considered ready
- **[Resource Basics](./01-resource-basics.md)** - Learn about resource template structure
