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
- For [collections](./04-collections.md), `includeWhen` applies to the entire collection

## What You Can Reference

Currently, `includeWhen` expressions can only reference `schema.spec` fields:

```kro
# ✓ Valid - references schema.spec and returns boolean
includeWhen:
  - ${schema.spec.ingress.enabled}
  - ${schema.spec.environment == "production"}
  - ${schema.spec.replicas > 3}
```

```kro
# ✗ Invalid - must return boolean
includeWhen:
  - ${schema.spec.appName}  # returns string, not boolean
```

kro validates `includeWhen` expressions when you create the ResourceGraphDefinition, ensuring they reference valid fields and return boolean values.

:::note
Currently, `includeWhen` can only reference `schema.spec` because conditions are evaluated before any resources are created. Support for conditional inclusion based on other resources' states is planned for future releases.
:::

## Dependencies and Skipped Resources

When a resource is skipped (due to `includeWhen` conditions), **all resources that depend on it are also skipped**.

<div style={{marginTop: '3rem', marginBottom: '3rem'}}>

![Dependency graph showing how skipped resources affect their children](/img/dep.svg)

</div>

In this example, the Deployment on the left has an `includeWhen` condition that evaluates to true, so it's included along with all its children (Service and Ingress). The Deployment on the right (shown in red) has a condition that evaluates to false, so it and all its dependencies are skipped.

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
