---
sidebar_position: 4
---

# Graph Inference

kro automatically infers dependencies from CEL expressions. You don't specify the order - you describe relationships, and kro figures out the rest.

## How It Works

When you reference one resource from another using a CEL expression, you create a dependency:

```yaml
resources:
  - id: configmap
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: app-config
      data:
        DATABASE_URL: ${schema.spec.dbUrl}

  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        containers:
          - env:
              - name: DATABASE_URL
                value: ${configmap.data.DATABASE_URL}
```

The expression `${configmap.data.DATABASE_URL}` creates a dependency: `deployment` depends on `configmap`. kro will create the configmap first, wait for the expression to be resolvable, then create the deployment.

## Dependency Graph (DAG)

kro builds a Directed Acyclic Graph (DAG) where:
- **Nodes** are resources
- **Edges** are dependencies (created by CEL references)
- **Directed** means dependencies have direction (A depends on B)
- **Acyclic** means no circular dependencies

### Common Patterns

Dependency graphs follow common patterns that you'll encounter in most RGDs. Understanding these patterns helps you design effective resource relationships. Real-world RGDs often combine multiple patterns - a complex application might have parallel branches that converge into a diamond, followed by a linear chain.

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

<Tabs>
<TabItem value="linear" label="Linear Chain" default>

In a linear chain, each resource depends on the previous one. This pattern is common when you have a sequence of resources where each step requires data from the previous step - like a ConfigMap that feeds into a Deployment, which then informs a Service.

```
configmap ──▶ deployment ──▶ service
```

```yaml
resources:
  - id: configmap
    template:
      apiVersion: v1
      kind: ConfigMap
      data:
        key: ${schema.spec.value}

  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        template:
          spec:
            containers:
              - envFrom:
                  - configMapRef:
                      name: ${configmap.metadata.name}

  - id: service
    template:
      apiVersion: v1
      kind: Service
      spec:
        selector: ${deployment.spec.selector.matchLabels}
```

The deployment references `${configmap.metadata.name}`, creating a dependency. The service references `${deployment.spec.selector.matchLabels}`, adding another link. kro creates them in sequence: configmap first, then deployment, then service.

</TabItem>
<TabItem value="diamond" label="Diamond">

In a diamond pattern, multiple independent resources converge into a single dependent resource. This is typical for applications that need multiple backing services - like an app that requires both a database and a cache before it can start.

```
       ┌──────────┐
       │  schema  │
       └────┬─────┘
            │
     ┌──────┴──────┐
     ▼             ▼
┌─────────┐   ┌─────────┐
│database │   │  cache  │
└────┬────┘   └────┬────┘
     │             │
     └──────┬──────┘
            ▼
       ┌─────────┐
       │   app   │
       └─────────┘
```

```yaml
resources:
  - id: database
    template:
      apiVersion: databases.example.com/v1
      kind: PostgreSQL
      spec:
        name: ${schema.spec.name}-db

  - id: cache
    template:
      apiVersion: caches.example.com/v1
      kind: Redis
      spec:
        name: ${schema.spec.name}-cache

  - id: app
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        template:
          spec:
            containers:
              - env:
                  - name: DATABASE_URL
                    value: ${database.status.endpoint}
                  - name: CACHE_URL
                    value: ${cache.status.endpoint}
```

Both `database` and `cache` only reference `schema`, so kro creates them in parallel. The `app` references both `${database.status.endpoint}` and `${cache.status.endpoint}`, so it waits for both to be ready before being created.

</TabItem>
<TabItem value="parallel" label="Parallel Branches">

When resources only reference `schema` (not each other), they have no interdependencies and kro creates them concurrently. This maximizes deployment speed for independent components like multiple microservices or worker pools.

```
       ┌──────────┐
       │  schema  │
       └────┬─────┘
            │
  ┌─────────┼─────────┐
  ▼         ▼         ▼
┌─────┐ ┌───────┐ ┌──────┐
│ api │ │worker │ │ cron │
└─────┘ └───────┘ └──────┘
```

```yaml
resources:
  - id: api
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.spec.name}-api

  - id: worker
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.spec.name}-worker

  - id: cron
    template:
      apiVersion: batch/v1
      kind: CronJob
      metadata:
        name: ${schema.spec.name}-cron
```

All three resources only reference `${schema.spec.name}`, meaning they have no dependencies on each other. kro creates all of them simultaneously, reducing overall deployment time.

</TabItem>
</Tabs>

:::tip Combining Patterns
Real-world RGDs typically combine multiple patterns. A complex application might have parallel branches (independent microservices), a diamond (services converging into a gateway), and linear chains (each service with its own config → deployment → service sequence). kro handles any valid DAG structure - these patterns are just common building blocks.
:::

## Topological Order

kro computes a topological order - the sequence resources can be processed such that all dependencies are satisfied.

**Creation:** Resources created in topological order
**Deletion:** Resources deleted in reverse order

View the computed order:
```bash
kubectl get rgd my-app -o jsonpath='{.status.topologicalOrder}'
```

Example output:
```yaml
status:
  topologicalOrder:
    - configmap
    - deployment
    - service
```

## Circular Dependencies

Circular dependencies are not allowed and will cause validation to fail:

```yaml
# ✗ This will fail
resources:
  - id: serviceA
    template:
      spec:
        targetPort: ${serviceB.spec.port}  # A → B

  - id: serviceB
    template:
      spec:
        targetPort: ${serviceA.spec.port}  # B → A (circular!)
```

**Fix:** Break the cycle by using `schema.spec` instead:
```yaml
resources:
  - id: serviceA
    template:
      spec:
        targetPort: ${schema.spec.portA}  # Use schema

  - id: serviceB
    template:
      spec:
        targetPort: ${serviceA.spec.port}  # This is fine
```

## What Happens at Runtime

When kro reconciles an instance:

1. **Evaluate static expressions** - Expressions referencing only `schema.spec` are evaluated once
2. **Process in topological order** - For each resource:
   - Wait for all dependency expressions to be resolvable
   - Create or update the resource
   - Move to next resource in order
3. **Delete in reverse order** - During deletion, process resources backwards

kro waits for CEL expressions to be **resolvable** before proceeding. This means the referenced resource exists and has the field being accessed.

## Next Steps

- **[Readiness](./02-resource-definitions/03-readiness.md)** - Control when resources are considered ready with `readyWhen`
- **[CEL Expressions](./03-cel-expressions.md)** - Learn more about writing expressions
- **[Resource Basics](./02-resource-definitions/01-resource-basics.md)** - Learn about resource templates
