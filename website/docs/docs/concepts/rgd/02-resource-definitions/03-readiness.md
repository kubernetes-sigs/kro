---
sidebar_position: 3
---

# Readiness

Not all resources are ready immediately after creation. A Deployment might exist but have zero available replicas, or a LoadBalancer Service might not have an external IP yet. If dependent resources try to use values that don't exist yet, they'll fail or get invalid data.

kro provides the `readyWhen` field to define when a resource is considered ready. When you add `readyWhen` to a resource, kro waits for all conditions to be true before proceeding with dependent resources.

## Basic Example

Here's a simple example where a database must be fully ready before the application deployment is created:

```kro
resources:
  - id: database
    template:
      apiVersion: database.example.com/v1
      kind: PostgreSQL
      metadata:
        name: ${schema.spec.name}-db
      spec:
        version: "15"
    readyWhen:
      - ${database.status.conditions.exists(c, c.type == "Ready" && c.status == "True")}
      - ${database.status.?endpoint != ""}

  - id: app
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.spec.name}
      spec:
        template:
          spec:
            containers:
              - name: app
                image: ${schema.spec.image}
                env:
                  - name: DATABASE_HOST
                    value: ${database.status.endpoint}
```

The `app` deployment won't be created until:
1. The database resource exists **AND**
2. The database has a Ready condition with status True **AND**
3. The database has an endpoint in its status

This ensures `${database.status.endpoint}` has a valid value when the app is created.

## How readyWhen Works

`readyWhen` is a list of CEL expressions that control when a resource is considered ready:

- **Without `readyWhen`**: Resources are ready once created and all their CEL expression references resolve
- **With `readyWhen`**: Resources are created but remain in a waiting state until all conditions are true
- If **all** expressions evaluate to `true`, the resource is marked ready
- If **any** expression evaluates to `false`, the resource continues waiting
- Each expression must evaluate to a **boolean** value (`true` or `false`)
- **Dependent resources wait** until all their dependencies are ready
- For [collections](./04-collections.md), `readyWhen` applies to the entire collection

## What You Can Reference

`readyWhen` expressions can only reference the resource itself (by its `id`):

```kro
# ✓ Valid - references the resource itself and returns boolean
- id: deployment
  readyWhen:
    - ${deployment.status.availableReplicas > 0}
    - ${deployment.status.conditions.exists(c, c.type == "Available" && c.status == "True")}
```

```kro
# ✗ Invalid - cannot reference other resources or schema
- id: deployment
  readyWhen:
    - ${service.status.loadBalancer.ingress.size() > 0}  # Can't reference other resources
    - ${schema.spec.replicas > 3}  # Can't reference schema
    - ${deployment.status.availableReplicas}  # Must return boolean
```

kro validates `readyWhen` expressions when you create the ResourceGraphDefinition, ensuring they reference valid fields and return boolean values.

:::important
`readyWhen` determines when **this specific resource** is ready. It can't depend on other resources' states - that's handled automatically by the dependency graph when you reference other resources in your templates. This keeps readiness conditions local, deterministic, and easy to debug.
:::

## Dependencies and Readiness

When a resource has a `readyWhen` condition, **all resources that depend on it must wait** until it's ready.

kro processes resources in the correct order based on references. If a resource references another resource's status field, kro:
1. Creates the referenced resource first
2. Waits for its `readyWhen` conditions (if any) to be satisfied
3. Only then creates the dependent resource with the correct status values

This ensures your resources always have valid data and prevents race conditions.

## The Optional Operator (?)

Use the optional operator `?` when accessing fields that are truly optional or have unknown structure:

```kro
readyWhen:
  # Use ? for optional fields that might never exist
  - ${service.status.?loadBalancer.?ingress.size() > 0}

  # Use ? for fields with unknown structure (like ConfigMap data)
  - ${config.data.?endpoint != ""}

  # No ? needed for fields that will eventually exist
  - ${database.status.endpoint != ""}
```

The `?` operator returns `null` if the field doesn't exist. This is useful when a field is optional or its structure is unknown at validation time. For fields that will eventually exist, kro simply waits for them to become available.

## Next Steps

- **[Dependencies & Ordering](../04-dependencies-ordering.md)** - Understand how kro determines resource creation order
- **[Conditional Resources](./02-conditional-creation.md)** - Control whether resources are created
- **[Collections](./04-collections.md)** - Use aggregate readiness conditions with `forEach`
- **[CEL Expressions](../03-cel-expressions.md)** - Master expression syntax for readiness conditions
