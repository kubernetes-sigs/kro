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
- **If namespace is omitted** on a scalar ref, kro looks for the resource in the instance's namespace
- **If namespace is omitted** on a collection ref, kro lists resources across all namespaces

## What You Can Reference

External references support two forms: **scalar** (single resource by name) and **collection** (multiple resources by label selector).

```kro
# Scalar: reference a single resource by name
- id: myConfig
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: my-config          # Fetch one resource by name
      namespace: default       # Optional: defaults to instance namespace

# Collection: reference multiple resources by label selector
- id: teamConfigs
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      namespace: default       # Optional: omit to list across all namespaces
      selector:                # Fetch all matching resources as an array
        matchLabels:
          team: platform
```

:::warning
`name` and `selector` are **mutually exclusive**. You must provide exactly one of them. A scalar ref exposes a single object; a collection ref exposes an array.
:::

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

## External Collections

When you use `selector` instead of `name`, the external ref becomes a **collection** — an array of all resources matching the label selector. This lets you aggregate data from a dynamic set of resources.

### Defining a Collection Selector

Use standard Kubernetes [LabelSelector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors) syntax:

```kro
# matchLabels — all labels must match
- id: teamConfigs
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      selector:
        matchLabels:
          team: platform
          env: production

# matchExpressions — set-based requirements
- id: prodSecrets
  externalRef:
    apiVersion: v1
    kind: Secret
    metadata:
      selector:
        matchExpressions:
          - key: tier
            operator: In
            values: ["gold", "silver"]
          - key: deprecated
            operator: DoesNotExist
```

### Collection as Array

A collection external ref is exposed as an array. Use CEL list functions to work with it:

```kro
# Number of matching resources
configCount: ${string(size(teamConfigs))}

# Extract names
names: ${teamConfigs.map(c, c.metadata.name).join(",")}

# Filter by a data field
critical: ${teamConfigs.filter(c, c.data.?priority == "critical")}

# Check if any match a condition
hasCritical: ${teamConfigs.exists(c, c.data.?priority == "critical")}
```

You can also use collections in status expressions:

```kro
status:
  configCount: ${string(size(teamConfigs))}
  allNames: ${teamConfigs.map(c, c.metadata.name).join(",")}
```

For more on collections and iteration patterns, see **[Collections](./04-collections.md)**.

### CEL Expressions in Selectors

`matchExpressions` values can contain CEL expressions using `${...}` syntax, enabling per-instance filtering:

```kro
- id: teamConfigs
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      selector:
        matchExpressions:
          - key: team
            operator: In
            values: ["${schema.spec.teamName}"]
```

Each instance of the custom resource resolves the CEL expression with its own spec values. For example, an instance with `spec.teamName: bravo` only sees ConfigMaps labeled `team=bravo`.

:::note
CEL selector values are evaluated on every reconciliation. If the schema field changes, the matched set updates automatically on the next reconcile.
:::

### Empty Selectors

An empty selector matches **all** resources of the given kind across all namespaces (or in a specific namespace if `namespace` is set):

```kro
- id: allConfigs
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      selector: {}
```

:::warning
An empty selector can return a large number of resources. Use specific labels to keep the result set bounded.
:::

### Sorting with sortBy

Collections have no guaranteed order. Use `sortBy` to sort by a field:

```kro
# Sort by a data field (lexicographic ordering)
sortedNames: ${configs.sortBy(c, c.data.priority).map(c, c.metadata.name).join(",")}

# Sort by resource name — useful for deterministic ordering
byName: ${configs.sortBy(c, c.metadata.name)}

# Sort by UID — guaranteed unique, stable tiebreaker
byUID: ${configs.sortBy(c, c.metadata.uid)}
```

For example, to build a comma-separated list of ConfigMap names in alphabetical order:

```kro
status:
  orderedNames: ${teamConfigs.sortBy(c, c.metadata.name).map(c, c.metadata.name).join(",")}
  # → "api-config,db-config,web-config"
```

`sortBy(variable, expression)` is a comprehension that returns a new list sorted in ascending lexicographic order of the expression result. To sort numerically, ensure values have consistent string-sortable formatting (e.g., zero-padded numbers).

## Reactive Watches

External references — both scalar and collection — are watched via Kubernetes informers. When the external resource changes, kro detects the change and triggers re-reconciliation automatically.

### How It Works

- kro sets up an **informer watch** for each external ref's GVK (GroupVersionKind)
- **No configuration required** — watches are set up automatically when the RGD becomes active

### Practical Scenario

Consider a Deployment that reads its replica count from a shared ConfigMap:

```kro
resources:
  - id: config
    externalRef:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: scaling-config

  - id: app
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.spec.name}
      spec:
        replicas: ${config.data.replicas}
```

When an operator updates `scaling-config`, the informer watch fires and kro immediately reconciles, updating the Deployment's replica count — without waiting for the next requeue cycle.

### Collections and Watches

For collection external refs, watches also detect:
- **New resources** that match the selector (e.g., a new ConfigMap with the right labels)
- **Removed resources** that no longer match (deleted or relabeled)
- **Updated resources** in the matched set

This means collection-based status expressions (like `size(configs)`) stay current as the cluster state evolves.

## Next Steps

- **[Collections](./04-collections.md)** - Learn about forEach iteration and collection patterns
- **[CEL Expressions](../03-cel-expressions.md)** - Learn more about the `?` operator and list functions
- **[Dependencies & Ordering](../04-dependencies-ordering.md)** - Understand how external refs affect dependency graphs
