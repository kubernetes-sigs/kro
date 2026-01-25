---
sidebar_position: 2
---

# CEL Expressions

CEL (Common Expression Language) is the language you use in kro to reference data between resources, compute values, and define conditions. Understanding CEL is essential for creating ResourceGraphDefinitions.

## What is CEL?

CEL (Common Expression Language) is an open-source expression language originally created by Google. It's the same language Kubernetes uses for validation rules, admission control, and field selectors.

### Why CEL is Safe

CEL was designed specifically to be safe for executing user code. Unlike scripting languages where you'd never blindly execute user-provided code, **you can safely execute user-written CEL expressions**. This safety comes from:

- **No side effects**: CEL expressions can't modify state, write files, or make network calls
- **Guaranteed termination**: No loops or recursion means expressions always complete
- **Resource bounded**: Expressions are prevented from consuming excessive memory or CPU
- **Sandboxed execution**: CEL can't access the filesystem or system resources

### Why CEL is Fast

CEL is optimized for **compile-once, evaluate-many** workflows:

1. **Parse and check** expressions once at configuration time (when you create an RGD)
2. **Store** the checked AST (Abstract Syntax Tree)
3. **Evaluate** the stored AST repeatedly at runtime against different inputs

Because CEL prevents behaviors that would make it slower, expressions evaluate in **nanoseconds to microseconds** - making it ideal for performance-critical reconciliation loops.

**Learn more:** [CEL Overview](https://cel.dev) | [CEL Language Specification](https://github.com/google/cel-spec) | [CEL Go Documentation](https://pkg.go.dev/github.com/google/cel-go)

## CEL Syntax in kro

### Expression Delimiters

In kro, CEL expressions are wrapped in `${` and `}`:

```kro
metadata:
  name: ${schema.spec.appName}
```

Everything between `${` and `}` is a CEL expression that gets evaluated at runtime.

### Two Types of Expressions

#### 1. Standalone Expressions

A **standalone expression** is a field whose value is exactly one expression - nothing else:

```kro
spec:
  replicas: ${schema.spec.replicaCount}
```

The expression result **replaces the entire field value**. The result type must match the field's expected type:
- If the field expects an integer, the expression must return an integer
- If the field expects an object, the expression must return an object
- etc.

**Examples:**
```kro
# Integer field
replicas: ${schema.spec.count}

# String field
image: ${schema.spec.containerImage}

# Boolean field
enabled: ${schema.spec.featureEnabled}

# Object field
env: ${configmap.data}

# Array field
volumes: ${schema.spec.volumeMounts}
```

#### 2. String Templates

A **string template** contains one or more expressions embedded in a string:

```kro
metadata:
  name: "${schema.spec.prefix}-${schema.spec.name}"
```

All expressions in a string template **must return strings**, and the result is always a string (concatenation of all parts).

**Examples:**
```kro
# Simple concatenation
name: "app-${schema.spec.name}"

# Multiple expressions
connectionString: "host=${database.status.endpoint}:${database.status.port}"

# With literal text
message: "Application ${schema.spec.name} is running version ${schema.spec.version}"
```

:::warning
Expressions in string templates **must return strings**. This won't work:
```kro
name: "app-${schema.spec.replicas}"  # Error: replicas is an integer
```

Use `string()` to convert:
```kro
name: "app-${string(schema.spec.replicas)}"
```
:::

## Referencing Data

### The `schema` Variable

The `schema` variable represents the **instance spec** - the values users provide when creating an instance of your API.

**Instance:**
```kro
apiVersion: kro.run/v1alpha1
kind: WebApplication
metadata:
  name: my-app
spec:
  appName: awesome-app
  replicas: 3
```

**In your RGD, access via `schema.spec`:**
```kro
resources:
  - id: deployment
    template:
      metadata:
        name: ${schema.spec.appName}      # "awesome-app"
      spec:
        replicas: ${schema.spec.replicas}  # 3
```

### Resource Variables

Each resource in your RGD can be referenced by its `id`:

```kro
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      # ... deployment spec

  - id: service
    template:
      apiVersion: v1
      kind: Service
      spec:
        selector:
          # Reference the deployment's labels
          app: ${deployment.spec.template.metadata.labels.app}
```

This **automatically creates a dependency**: the service depends on the deployment. kro will create the deployment first. See [Dependencies & Ordering](./04-dependencies-ordering.md) for details.

### Field Paths

Use dot notation to navigate nested fields:

```kro
# Access nested objects
${deployment.spec.template.spec.containers[0].image}

# Access map values
${configmap.data.DATABASE_URL}

# Access status fields
${deployment.status.availableReplicas}
```

### Array Indexing

Access array elements using `[index]`:

```kro
# First container's image
${deployment.spec.template.spec.containers[0].image}

# Second port
${service.spec.ports[1].port}
```

## The Optional Operator (`?`)

The `?` operator makes a field access optional. If the field doesn't exist, the expression returns `null` instead of failing.

### When to Use `?`

Use the optional operator when:
1. **Referencing schema-less objects** (ConfigMaps, Secrets without known structure)
2. **Accessing fields that might not exist** (optional status fields)
3. **Working with dynamic data** where structure isn't guaranteed

### Syntax

Place `?` before the field that might not exist:

```kro
${configmap.data.?DATABASE_URL}
```

If `data.DATABASE_URL` doesn't exist, this returns `null` instead of erroring.

### Examples

**Referencing a ConfigMap:**
```kro
resources:
  - id: config
    externalRef:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: app-config

  - id: deployment
    template:
      spec:
        containers:
          - env:
              - name: DATABASE_URL
                # ConfigMap might not have this key
                value: ${config.data.?DATABASE_URL}
```

**Optional status fields:**
```kro
# Some resources might not have this status field immediately
ready: ${deployment.status.?readyReplicas > 0}
```

**Chaining optional accessors:**
```kro
# Multiple fields might not exist
${service.status.?loadBalancer.?ingress[0].?hostname}
```

:::warning
The `?` operator prevents kro from validating the field's existence at build time. Use it sparingly - prefer explicit schemas when possible.
:::

## Available CEL Libraries

| Library | Documentation |
|---------|---------------|
| Lists | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Lists) |
| Strings | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Strings) |
| Encoders | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Encoders) |
| Random | [kro custom](https://github.com/kubernetes-sigs/kro/blob/main/pkg/cel/library/random.go) |
| URLs | [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#URLs) |
| Regex | [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Regex) |

For the complete CEL language reference, see the [CEL language definitions](https://github.com/google/cel-spec/blob/master/doc/langdef.md#list-of-standard-definitions).

## Type Checking and Validation

One of kro's key features is **compile-time type checking** of CEL expressions.

### How Type Checking Works

When you create an RGD, kro:
1. Fetches the OpenAPI schema for each resource type from the API server
2. Validates that every field path in your expressions exists
3. Checks that expression output types match target field types
4. Reports errors **before** any instances are created

**Example:**
```kro
spec:
  replicas: ${schema.spec.appName}  # Error: appName is string, replicas expects integer
```

kro will reject this RGD with a clear error message.

### Type Compatibility

kro checks two forms of compatibility:

#### 1. Exact Type Match
```kro
# ✓ Correct: integer to integer
replicas: ${schema.spec.replicaCount}

# ✗ Wrong: string to integer
replicas: ${schema.spec.appName}
```

#### 2. Structural Compatibility (Duck Typing)

kro supports structural compatibility for complex types through deep type inspection:

**Map ↔ Struct compatibility**:
```kro
# Map can be assigned to struct
# Map keys must be strings matching struct field names
# Map values must be compatible with corresponding field types
labels: ${schema.spec.labelMap}

# Struct can be assigned to map
# Struct field names become string keys
# Struct field types must be compatible with map value type
annotations: ${deployment.metadata.labels}
```

**Struct subset semantics**:

The expression result can have fewer fields than the target expects, but cannot have extra fields the target doesn't define:

```kro
# Target field: containers expects objects with name, image, ports, env, etc.
spec:
  template:
    spec:
      # ✓ Valid: subset of expected fields
      containers:
        - ${{"name": "app", "image": schema.spec.image}}

      # ✗ Invalid: "foo" is not a valid container field
      containers:
        - ${{"name": "app", "image": schema.spec.image, "foo": "bar"}}
```

**List and Map recursive checking**:
- Lists: Element types must be structurally compatible
- Maps: Both key and value types must be structurally compatible
- Recursively validated for nested structures

## Common Patterns

### Conditional Values

Use ternary operator for conditional values:

```kro
# If-then-else
image: ${schema.spec.env == "prod" ? "nginx:stable" : "nginx:latest"}

# With optional
replicas: ${schema.spec.?replicas.orValue(3)}
```

### Building Complex Objects

Create objects inline:

```kro
env:
  - name: DATABASE_URL
    value: ${database.status.endpoint}
  - name: DATABASE_PORT
    value: ${string(database.status.port)}
```

Or use CEL to construct them:

```kro
labels: ${{"app": schema.spec.name, "env": schema.spec.environment}}
```

### String Formatting

Build connection strings and URLs:

```kro
# Connection string
connectionString: "postgresql://${db.status.endpoint}:${db.status.port}/${schema.spec.dbName}"

# ARN format
roleArn: ${"arn:aws:iam::%s:role/%s".format([schema.spec.accountId, schema.spec.roleName])}
```

### Working with Lists

Filter, map, and transform lists:

```kro
# Extract specific fields
containerNames: ${deployment.spec.template.spec.containers.map(c, c.name)}

# Filter by condition
readyConditions: ${deployment.status.conditions.filter(c, c.status == "True")}

# Check all items
allReady: ${schema.spec.services.all(s, s.enabled)}
```

### Aggregating Status

Collect status from multiple resources:

```kro
status:
  # From single resource
  endpoint: ${service.status.loadBalancer.ingress[0].hostname}

  # Computed from multiple
  allReady: ${deployment.status.availableReplicas == schema.spec.replicas && service.status.loadBalancer.ingress.size() > 0}

  # Complex aggregation
  totalPods: ${deployment.status.replicas + statefulset.status.replicas}
```

## Next Steps

- **[Dependencies & Ordering](./04-dependencies-ordering.md)** - Learn how CEL expressions create dependencies
- **[Conditional Creation](./02-resource-definitions/02-conditional-creation.md)** - Use CEL for `includeWhen` conditions
- **[Readiness](./02-resource-definitions/03-readiness.md)** - Use CEL for `readyWhen` conditions
- **[External References](./02-resource-definitions/05-external-references.md)** - Reference external resources with CEL
