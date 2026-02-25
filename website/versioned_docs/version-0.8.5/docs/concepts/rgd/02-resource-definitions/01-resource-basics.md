---
sidebar_position: 1
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Basics

Resources define the Kubernetes objects that kro creates and manages when users instantiate your custom API. Each resource is **valid Kubernetes YAML** with **CEL expressions** for dynamic values, enabling full type checking and validation before any resources are deployed.

## Resource Structure

Every resource in an RGD requires an `id` and a `template`:

```kro
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: my-app
      spec:
        replicas: 3
        # ... rest of deployment spec ...

  - id: service
    template:
      apiVersion: v1
      kind: Service
      metadata:
        name: my-app
      spec:
        # ... rest of service spec ...
```

Each template must be a **valid Kubernetes resource** with:
- **`apiVersion`** - The API version (e.g., `apps/v1`, `v1`)
- **`kind`** - The resource type (e.g., `Deployment`, `Service`)
- **`metadata`** - At minimum, metadata fields like `name`

The `id` uniquely identifies the resource within your RGD and is used to reference it in other resources.

:::note ID Naming Requirements
Resource IDs must be in **lowerCamelCase** format because they're used as identifiers in CEL expressions. Dashes are not allowed, as they would be interpreted as subtraction operators in CEL.

**Valid IDs:**
- `deployment`
- `webServer`
- `postgresDatabase`

**Invalid IDs:**
- `web-server` (dash would be treated as subtraction)
- `WebServer` (should start with lowercase)
- `postgres_database` (underscores discouraged, use camelCase)
:::

## Resource Templates with CEL

Resource templates are structured Kubernetes manifests where specific fields contain CEL expressions that reference your schema or other resources:

```kro
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
```

Unlike text-based templating, kro maintains valid YAML structure throughout. The CEL expressions `${...}` are embedded directly in field values and are type-checked against actual Kubernetes schemas when you create the ResourceGraphDefinition.

:::important
When you create a ResourceGraphDefinition, kro validates every template and CEL expression before your API even exists. It parses each template as a real Kubernetes manifest, extracts all CEL expressions, validates the CEL syntax, and type-checks expressions against actual Kubernetes OpenAPI schemas. This means errors are caught when you create the RGD, not when users try to deploy resources.
:::

## What You Can Reference

CEL expressions in resource templates can reference three things:

1. **`schema.spec`** - User-provided configuration fields from the instance
2. **`schema.metadata`** - Instance metadata (name, namespace, labels, annotations)
3. **Other resources** - Any field from other resources in your RGD, including their `spec`, `metadata`, and `status`

:::important
When you reference another resource in a CEL expression, it automatically creates a dependency. This is an implicit way of declaring that one resource depends on another. kro uses these references to determine the correct creation order - ensuring the referenced resource exists before the one referencing it. Learn more about [Dependencies and Ordering](../dependencies-ordering).
:::
:::important
If a resource references a field in `schema.spec`, it creates a hard dependency on the field being present in the instance. You are declaring that the field must exist. Any instance that does not specify that field will fail reconciliation.
:::

## Reference Examples

Here's a simple example showing all types of references:

```kro
spec:
  resources:
    - id: database
      template:
        apiVersion: database.example.com/v1
        kind: PostgreSQL
        metadata:
          # Reference instance spec
          name: ${schema.spec.name}-db
          # Reference instance metadata
          namespace: ${schema.metadata.namespace}
          labels: ${schema.metadata.labels}
        spec:
          version: "15"
          storage: ${schema.spec.storageSize}
          replicas: 2

    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
          namespace: ${schema.metadata.namespace}
          # Reference another resource's metadata
          annotations: ${database.metadata.annotations}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.spec.name}
          template:
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
                  env:
                    # Reference another resource's status
                    - name: DATABASE_HOST
                      value: ${database.status.endpoint}
                    # Reference another resource's spec
                    - name: DATABASE_VERSION
                      value: ${database.spec.version}
```

In this example:
- `database` references `schema.spec` (name, storageSize) and `schema.metadata` (namespace, labels)
- `deployment` references `schema.spec` (name, replicas, image), `schema.metadata.namespace`, `database.metadata.annotations`, `database.spec.version`, and `database.status.endpoint`

:::important
kro automatically determines that `database` must be created before `deployment`. The deployment waits for the database's endpoint to be available before it can be created. Learn more about [Dependencies and Ordering](../dependencies-ordering).
:::

## CEL Property Verification

kro performs extensive validation and type checking on CEL expressions **before any instances are created**.

### What kro Validates

When you create a ResourceGraphDefinition, kro validates every CEL expression:

<div style={{height: '650px', overflowY: 'auto'}}>
<Tabs>
<TabItem value="template" label="Template Syntax">

kro looks for `${` and expects to find a matching `}`.

```yaml
resources:
  - id: configMap
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        # ✓ Valid - kro finds ${ and matching }
        name: ${schema.spec.name}
      data:
        # ✓ Valid - no ${, treated as literal string
        staticKey: "my-app"

        # ✗ Error - ${ found but missing closing }
        broken: ${schema.spec.name

        # ✗ Error - nested expressions not allowed
        nested: ${outer(${inner})}

        # ✓ Valid - nested ${} inside string literals is allowed
        valid: ${outer("${inner}")}
```

</TabItem>
<TabItem value="cel" label="CEL Expression Syntax">

Once kro extracts the expression between `${` and `}`, it must be valid CEL:

```kro
resources:
  - id: configMap
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        # ✓ Valid CEL expression
        name: ${schema.spec.name}
      data:
        # ✗ Incomplete expression
        broken1: ${schema.spec.name +}

        # ✗ Unclosed string literal
        broken2: ${schema.spec.name + "suffix}

        # ✗ Invalid operator
        broken3: ${schema.spec.name ++ "suffix"}

        # ✗ Missing closing parenthesis
        broken4: ${(schema.spec.replicas + 1}

        # ✗ Invalid syntax
        broken5: ${schema..spec.name}
```

</TabItem>
<TabItem value="type" label="Type Checking">

kro verifies that referenced fields exist in the actual Kubernetes resource schemas, that expression output types match the target field types, and that operations are valid for the data types involved.

```kro
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        # ✓ Valid: replicas expects integer, schema.spec.replicas is integer
        replicas: ${schema.spec.replicas}

        # ✗ Type error: replicas expects integer, not string
        replicas: ${schema.spec.name}
```

</TabItem>
<TabItem value="field" label="Field Existence">

kro validates that referenced fields actually exist:

```kro
resources:
  - id: service
    template:
      spec:
        # ✓ Valid: Deployment has spec.replicas field
        targetPort: ${deployment.spec.replicas}

        # ✗ Field doesn't exist: Deployment has no spec.podReplicas
        targetPort: ${deployment.spec.podReplicas}
```

</TabItem>
<TabItem value="schema" label="Schema Validation">

For your custom schema, kro validates that fields referenced in templates exist in your schema definition, that types are compatible, and that required fields are present.

```kro
spec:
  schema:
    spec:
      appName: string
      replicas: integer

  resources:
    - id: deployment
      template:
        metadata:
          # ✓ Valid: schema.spec.appName exists and is string
          name: ${schema.spec.appName}

          # ✗ Error: schema.spec.nonExistent doesn't exist
          name: ${schema.spec.nonExistent}
```

</TabItem>
<TabItem value="static" label="Static Fields">

kro also validates static fields (those without CEL expressions) against Kubernetes schemas, just like the API server:

```kro
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        # ✓ Valid: replicas is an integer
        replicas: 3

        # ✗ Type error: replicas must be integer, not string
        replicas: "three"

        # ✗ Unknown field: Deployment spec has no such field
        unknownField: value
```

</TabItem>
</Tabs>
</div>

For more details, see [Static Type Checking](../static-type-checking).

## How kro Processes Templates

kro processes templates in several distinct phases:

### 1. Validation Phase (RGD Creation)

When you create a ResourceGraphDefinition, kro parses all resource templates as Kubernetes manifests and extracts all CEL expressions from template fields. It validates CEL syntax, fetches actual Kubernetes OpenAPI schemas for referenced resource types, and type-checks expressions against real schemas. kro then verifies field existence in target resources, builds the dependency graph from expression references, and computes the topological order.

**All of this happens before any instances exist.** If validation fails, the RGD is rejected.

### 2. Evaluation Phase (Instance Creation)

When a user creates an instance, kro processes resources in topological order:

1. **Evaluate CEL expressions** - Substitute `schema.spec` values and any available resource references. If an expression references a field that doesn't exist yet (like `${database.status.endpoint}`), kro requeues and waits for the value to become available.
2. **Create the resource** - Apply the evaluated template to Kubernetes
3. **Wait for readiness** - If the resource has [`readyWhen`](./03-readiness.md) conditions, wait for them to be satisfied
4. **Move to the next resource** - Repeat for each resource in dependency order

### 3. Update Phase (Instance Updates)

When an instance is updated, kro re-evaluates expressions with new instance values, determines which resources need updates, and updates them in topological order while preserving dependency relationships.

## Type Safety Examples

<div style={{height: '450px', overflowY: 'auto'}}>
<Tabs>
<TabItem value="integer" label="Integer Fields">

```kro
# Deployment requires integer for replicas
resources:
  - id: deployment
    template:
      spec:
        # ✓ Valid: integer expression
        replicas: ${schema.spec.replicas}

        # ✓ Valid: integer arithmetic
        replicas: ${schema.spec.replicas * 2}

        # ✗ Type error: string value for integer field
        replicas: ${schema.spec.name}
```

</TabItem>
<TabItem value="string" label="String Fields">

```kro
resources:
  - id: deployment
    template:
      metadata:
        # ✓ Valid: string expression
        name: ${schema.spec.name}

        # ✓ Valid: string concatenation
        name: ${schema.spec.name + "-deployment"}

        # ✗ Type error: integer value for string field
        name: ${schema.spec.replicas}
```

</TabItem>
<TabItem value="object" label="Object/Map Fields">

```kro
resources:
  - id: deployment
    template:
      metadata:
        # ✓ Valid: object/map expression
        labels: ${schema.spec.labels}

        # ✓ Valid: accessing map values
        annotations:
          app: ${schema.spec.labels.app}
```

</TabItem>
<TabItem value="array" label="Array/List Fields">

```kro
# Assuming deployment1 exists with containers defined
resources:
  - id: deployment2
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        template:
          spec:
            # ✓ Valid: array of containers
            containers: ${deployment1.spec.template.spec.containers}

            # ✗ Type error: single container object where array expected
            containers: ${deployment1.spec.template.spec.containers[0]}
```

</TabItem>
</Tabs>
</div>

For more details, see [Static Type Checking](../static-type-checking).

## Validation Errors

When kro detects validation errors, the RGD is rejected and detailed error messages appear in the RGD's `status.conditions`. You can inspect these with `kubectl describe rgd <name>` or `kubectl get rgd <name> -o yaml`. Here are common error types:

### Example: Unknown API or Kind

If a resource references an API version or kind that doesn't exist in the cluster:

```kro
resources:
  - id: database
    template:
      apiVersion: database.example.com/v1
      kind: PostgreSQL
      metadata:
        name: my-db
```

**Condition:**
```yaml
status:
  conditions:
    - type: ResourceGraphAccepted
      status: "False"
      reason: InvalidResourceGraph
      message: "failed to get schema for resource database: schema not found"
```

### Example: Unknown Field in Definition

If a template contains a field that doesn't exist in the Kubernetes schema:

```kro
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      spec:
        unknownField: value
```

**Condition:**
```yaml
status:
  conditions:
    - type: ResourceGraphAccepted
      status: "False"
      reason: InvalidResourceGraph
      message: "error getting field schema for path spec.unknownField: schema not found for field unknownField"
```

### Example: Unknown Field in Reference

If a CEL expression references a field that doesn't exist on another resource:

```kro
resources:
  - id: service
    template:
      spec:
        port: ${deployment.spec.nonExistentField}
```

**Condition:**
```yaml
status:
  conditions:
    - type: ResourceGraphAccepted
      status: "False"
      reason: InvalidResourceGraph
      message: "failed to type-check template expression \"deployment.spec.nonExistentField\" at path \"spec.port\": no such member: nonExistentField"
```

### Example: Type Mismatch

If an expression's output type doesn't match what the target field expects:

```kro
resources:
  - id: deployment
    template:
      spec:
        replicas: ${schema.spec.name}  # name is string, replicas needs integer
```

**Condition:**
```yaml
status:
  conditions:
    - type: ResourceGraphAccepted
      status: "False"
      reason: InvalidResourceGraph
      message: "type mismatch in resource \"deployment\" at path \"spec.replicas\": expression \"schema.spec.name\" returns type \"string\" but expected \"integer\""
```

For more information about validation errors and type checking, see [Static Type Checking](../static-type-checking).

## Next Steps

- **[CEL Expressions](../03-cel-expressions.md)** - Master CEL syntax and functions
- **[Dependencies & Ordering](../04-dependencies-ordering.md)** - How templates create dependencies
- **[SimpleSchema](../../../../api/specifications/simple-schema.md)** - Define your API schema
