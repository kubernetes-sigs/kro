---
sidebar_position: 6
---

# Lifecycle

The `lifecycle` field controls how KRO handles certain actions on resources. You can configure per-resource behavior using the `policy()` CEL builder.

## Delete Policy

The delete policy determines what happens to resources when an instance is deleted.

### Default Behavior

By default, all resources are deleted with the instance:

```yaml
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      # ...
  # No lifecycle field = resource deleted with instance
```

### Retention

Use `lifecycle: "${policy().withRetain()}"` to avoid deleting a resource if the instance or RGD is deleted.

This also will avoid deleting the resource if you update your RGD to no longer include the resource.

```yaml
resources:
  - id: database
    template:
      apiVersion: v1
      kind: PersistentVolumeClaim
      metadata:
        name: ${schema.metadata.name}-data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 10Gi
    lifecycle: "${policy().withRetain()}"
```

When the instance is deleted, the PVC is **orphaned** (KRO labels removed) rather than deleted, preserving the data.

### Policy Methods

The `policy()` builder currently provides two methods:

- **`withRetain()`** - Resource is orphaned when instance is deleted
- **`withDelete()`** - Resource is deleted (default behavior, explicit form)

```yaml
# Retain the resource
lifecycle: "${policy().withRetain()}"

# Delete the resource (explicit, same as omitting lifecycle field)
lifecycle: "${policy().withDelete()}"

# Conditionally set policy
lifecycle: "${schema.spec.prod ? policy().withRetain() : policy().withDelete()}"
```