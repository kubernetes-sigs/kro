---
sidebar_position: 6
---

# Lifecycle

The `lifecycle` field controls what happens to resources when an instance is deleted. By default, all resources are deleted with the instance. You can override this behavior per-resource using the `policy()` CEL builder.

## Default Behavior

When you delete a KRO instance, all resources it created are automatically deleted:

```yaml
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      # ...
  # No lifecycle field = resource deleted with instance
```

This is the standard Kubernetes ownership pattern - when you delete the parent, the children are cleaned up.

## Retention

Use `lifecycle: "${policy().withRetain()}"` to keep a resource even after the instance is deleted:

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

## Policy Methods

The `policy()` builder provides two methods:

- **`withRetain()`** - Resource is orphaned when instance is deleted
- **`withDelete()`** - Resource is deleted (default behavior, explicit form)

```yaml
# Retain the resource
lifecycle: "${policy().withRetain()}"

# Delete the resource (explicit, same as omitting lifecycle field)
lifecycle: "${policy().withDelete()}"
```

## Conditional Retention

Use CEL conditionals to make retention decisions based on instance data:

```yaml
resources:
  - id: database
    template:
      apiVersion: v1
      kind: PersistentVolumeClaim
      # ...
    lifecycle: "${schema.spec.environment == 'production' ? policy().withRetain() : policy().withDelete()}"
```

Now production databases are retained, while dev/test databases are cleaned up.

## How It Works

When a resource has `lifecycle: "${policy().withRetain()}"`:

1. **During creation/updates**: KRO manages the resource normally with ownership labels
2. **During instance deletion**: KRO removes its ownership labels but leaves the resource in the cluster
3. **After deletion**: The resource continues to exist without KRO labels

### Resource Adoption

Orphaned resources can be re-adopted by a new instance with the same name:

```bash
# Delete instance (retains database PVC)
kubectl delete myapp my-instance

# Recreate instance with same name
kubectl apply -f my-instance.yaml
```

The new instance will adopt the existing PVC and continue using it, preserving data across instance recreations.

## Important Notes

- Lifecycle policies only affect behavior during **instance deletion**
- Resources without KRO labels are not tracked by KRO
- Orphaned resources can be manually deleted with `kubectl delete`
- Only schema variables can be referenced in lifecycle expressions
- The policy cannot be changed multiple times (e.g., `.withRetain().withDelete()` is an error)
