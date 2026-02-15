---
sidebar_position: 5
---

# External References

Sometimes you need to reference resources that already exist in your cluster - like shared configuration, pre-provisioned infrastructure, or cluster-wide resources. External references let you read existing resources and use their data in your ResourceGraphDefinition without kro managing their lifecycle.

kro provides the `externalRef` field to reference existing resources. When you add `externalRef`, kro reads the resource from the cluster but never creates, updates, or deletes it.

## Basic Example

Here's a simple example where an application references a shared ConfigMap that exists in the cluster:

```kro
resources:
  - id: sharedConfig
    externalRef:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: platform-config
        namespace: platform-system

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
                  - name: PLATFORM_URL
                    value: ${sharedConfig.data.?platformUrl}
                  - name: REGION
                    value: ${sharedConfig.data.?region}
```

The `app` deployment won't be created until:
1. The `platform-config` ConfigMap exists in the `platform-system` namespace
2. kro successfully reads the ConfigMap and makes its data available

This allows multiple instances to share the same configuration without duplicating it.

## How externalRef Works

`externalRef` defines a resource that kro reads but doesn't manage:

- **kro reads the resource** from the cluster and makes its data available to other resources
- **kro never creates, updates, or deletes** the external resource
- **The resource must exist** for reconciliation to succeed - kro waits for it to be present
- **External resources participate in the dependency graph** just like managed resources
- **If namespace is omitted**, kro looks for the resource in the instance's namespace

## What You Can Reference

External references require these fields:

```kro
# Required fields
- id: myExternal
  externalRef:
    apiVersion: v1           # Required: API version
    kind: ConfigMap          # Required: Resource type
    metadata:
      name: my-config        # Required: Resource name
      namespace: default     # Optional: Defaults to instance namespace
```

You can reference any Kubernetes resource:
- **Namespaced resources**: ConfigMaps, Secrets, Services (specify namespace or use instance namespace)
- **Cluster-scoped resources**: StorageClasses, ClusterIssuers (omit namespace)
- **Custom resources**: Any CRD in your cluster

## The Optional Operator (?)

Use the optional operator `?` when accessing fields with unknown or unstructured schemas. kro can't validate the structure at build time, so `?` safely returns `null` if the field doesn't exist.

Common examples include:
- **ConfigMaps and Secrets**: The `data` field has no predefined keys
- **Custom resources**: CRDs with free-form `spec` or `status` fields
- **Any resource with dynamic fields**: Fields whose structure isn't known at RGD creation time

```kro
# ✓ Safe: returns null if platformUrl doesn't exist
value: ${config.data.?platformUrl}

# ✗ Unsafe: fails validation because kro can't verify the field exists
value: ${config.data.platformUrl}
```

### Using orValue() for Defaults

Combine `?` with `.orValue()` to provide defaults when fields don't exist:

```kro
env:
  - name: LOG_LEVEL
    value: ${config.data.?LOG_LEVEL.orValue("info")}

  - name: MAX_CONNECTIONS
    value: ${config.data.?MAX_CONNECTIONS.orValue("100")}
```

:::warning
When you use `?`, kro cannot validate the field exists at build time. If the resource doesn't have the expected field, the expression evaluates to `null`. Document the expected structure and use `.orValue()` to provide sensible defaults.
:::

## Dependencies

External references participate in the dependency graph just like managed resources. If you reference an external resource's data, kro automatically creates a dependency:

```kro
resources:
  - id: platformConfig
    externalRef:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: platform-config

  - id: database
    template:
      spec:
        region: ${platformConfig.data.?region}

  - id: app
    template:
      spec:
        env:
          - name: DB_ENDPOINT
            value: ${database.status.endpoint}
```

**Dependency chain:**
```
platformConfig (external) → database → app
```

kro will:
1. Wait for `platformConfig` to exist
2. Create `database` using the config data
3. Wait for `database` to be ready
4. Create `app`

## Next Steps

- **[CEL Expressions](../03-cel-expressions.md)** - Learn more about the `?` operator
- **[Dependencies & Ordering](../04-dependencies-ordering.md)** - Understand how external refs affect dependency graphs
