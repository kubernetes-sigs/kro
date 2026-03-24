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

### Multiline Expressions and YAML Block Scalars

YAML block scalars (`|` and `>`) often add a **trailing newline**. For fields that must be a *standalone* expression (like `includeWhen` and `readyWhen`), a trailing newline means the value is no longer exactly `${...}` and validation fails.

Use the **chomp indicator** `-` to strip the final newline:

```kro
includeWhen:
  - |-
    ${
      schema.spec.enabled &&
      schema.spec.count > 0
    }
```

If you prefer folded scalars (`>`), also use `>-` to avoid the trailing newline:

```kro
readyWhen:
  - >-
    ${
      deployment.status.phase == "Ready" &&
      deployment.status.observedGeneration == deployment.metadata.generation
    }
```

:::note
- `|` preserves newlines; `>` folds them into spaces.
- The `-` is what removes the final newline.
:::

### Escaping `${VAR}` Syntax

kro uses `${...}` as CEL expression delimiters, which conflicts with shell `${VAR}` variable expansion syntax. To produce a literal `${VAR}` in the output, wrap the variable reference in a CEL string literal:

**Pattern:** `${"${VAR}"}` produces the literal output `${VAR}`

kro sees the outer `${...}` and evaluates the contents as CEL. The contents `"${VAR}"` is just a CEL string literal (text between double quotes), so it evaluates to the string `${VAR}`.

**Example:**
```kro
containers:
  - name: worker
    command:
      - bash
      - -c
      - echo "Hello ${"${USER}"}"
```

This works for all shell parameter expansion forms:

| Shell syntax | Escaped for kro | CEL evaluates to |
|---|---|---|
| `${VAR}` | `${"${VAR}"}` | `${VAR}` |
| `${VAR:-default}` | `${"${VAR:-default}"}` | `${VAR:-default}` |
| `${VAR:=value}` | `${"${VAR:=value}"}` | `${VAR:=value}` |

:::note
Shell syntax that does **not** use `${` doesn't need escaping:

- `$VAR` — no braces, kro ignores it
- `$(command)` — command substitution uses `$(`, not `${`
- `$@`, `$1`, `$?` — special variables without braces
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

| Library                     | Documentation                                                                                  |
|-----------------------------|------------------------------------------------------------------------------------------------|
| Lists (cel-go)              | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Lists)                            |
| Lists (k8s)                 | [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Lists) |
| Strings                     | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Strings)                          |
| Encoders                    | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Encoders)                         |
| Two-Variable Comprehensions | [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#TwoVarComprehensions)             |
| Random                      | [kro custom](https://github.com/kubernetes-sigs/kro/blob/main/pkg/cel/library/random.go)       |
| JSON                        | [kro custom](https://github.com/kubernetes-sigs/kro/blob/main/pkg/cel/library/json.go)         |
| Index Mutation (lists)      | [kro custom](https://github.com/kubernetes-sigs/kro/blob/main/pkg/cel/library/lists.go)        |
| URLs                        | [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#URLs)   |
| Regex                       | [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Regex)  |
| Quantity                    | [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Quantity) |

The kro **Index Mutation** library adds three pure list functions (they return a new list and do not modify the input):

| Function | Signature | Description |
|---|---|---|
| `lists.setAtIndex` | `list(T), int, T → list(T)` | Replace the element at `index` with `value`. Index must be in `[0, size(list))`. |
| `lists.insertAtIndex` | `list(T), int, T → list(T)` | Insert `value` before `index`. Use `index == size(list)` to append. Index must be in `[0, size(list)]`. |
| `lists.removeAtIndex` | `list(T), int → list(T)` | Remove the element at `index`. Index must be in `[0, size(list))`. |

**Examples:**

```kro
# Replace the second tag
tags: ${lists.setAtIndex(schema.spec.tags, 1, "new-tag")}

# Prepend an environment variable
envVars: ${lists.insertAtIndex(schema.spec.envVars, 0, "DEBUG=true")}

# Remove the first port
ports: ${lists.removeAtIndex(schema.spec.ports, 0)}

# Chain operations: swap first two elements
swapped: ${lists.setAtIndex(lists.setAtIndex(schema.spec.items, 0, schema.spec.items[1]), 1, schema.spec.items[0])}
```

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

# Sort by a field (lexicographic ordering)
sorted: ${items.sortBy(item, item.data.priority)}
orderedNames: ${items.sortBy(i, i.data.priority).map(i, i.metadata.name).join(",")}
```

### Converting Between Lists and Maps

The two-variable comprehension macros let you transform lists into maps, reshape maps, and extract lists from maps. Each macro provides access to both the key/index and the value of each entry.

#### `transformMap` — Transform values, keeping keys

Applies an expression to each value while preserving the original keys (or indices for lists):

```kro
# Double each value in a map
scaled: ${{ "cpu": 2, "memory": 4 }.transformMap(k, v, v * 2)}
# → {"cpu": 4, "memory": 8}

# Convert a list to a map of {index: transformed_value}
indexed: ${schema.spec.ports.transformMap(i, v, v + 1000)}
# [80, 443] → {0: 1080, 1: 1443}
```

With an optional filter predicate (four-argument form), only entries where the predicate is true are included:

```kro
# Keep only entries with value > 1
filtered: ${{ "a": 1, "b": 5, "c": 3 }.transformMap(k, v, v > 1, v * 10)}
# → {"b": 50, "c": 30}
```

#### `transformMapEntry` — Build new key-value pairs

Each iteration must return a single-entry map `{key: value}`. The results are merged into one map:

```kro
# Swap keys and values
swapped: ${{ "us-east-1": "primary", "eu-west-1": "secondary" }.transformMapEntry(k, v, {v: k})}
# → {"primary": "us-east-1", "secondary": "eu-west-1"}

# Convert a list of names into a map of {name: index}
nameIndex: ${schema.spec.items.transformMapEntry(i, v, {v: string(i)})}
# ["app", "db"] → {"app": "0", "db": "1"}
```

#### `transformList` — Extract a list from a map

Converts a map to a list by evaluating an expression for each entry:

```kro
# Extract all keys
keys: ${{ "app": "nginx", "version": "1.19" }.transformList(k, v, k)}
# → ["app", "version"]

# Extract all values
values: ${{ "app": "nginx", "version": "1.19" }.transformList(k, v, v)}
# → ["nginx", "1.19"]

# Build formatted strings from key-value pairs
envVars: ${{ "APP": "nginx", "PORT": "8080" }.transformList(k, v, k + "=" + v)}
# → ["APP=nginx", "PORT=8080"]
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
