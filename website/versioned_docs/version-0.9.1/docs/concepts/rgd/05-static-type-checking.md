---
sidebar_position: 7
---

# Static Analysis

One of kro's most powerful features is **static analysis**. When you create or update a ResourceGraphDefinition, kro performs extensive validation before accepting it. This analysis happens during RGD reconciliation - before any instances of your custom resource are created.

Static analysis catches errors early: invalid CEL syntax, references to non-existent resources or fields, type mismatches, circular dependencies, and schema violations. Without this upfront validation, these errors would only surface during reconciliation when kro attempts to create resources in your cluster. This means immediate feedback during development instead of runtime failures.

kro achieves this by integrating directly with Kubernetes OpenAPI schemas. For every resource in your graph - whether built-in types like Deployments or custom CRDs - kro fetches the schema and validates your templates against it. This ensures CEL expressions reference actual fields, output types match target field expectations, and your entire resource graph is structurally sound.

## The Validation Process

When you create or update a ResourceGraphDefinition, kro performs validation in multiple stages:

<div style={{marginTop: '3rem', marginBottom: '3rem'}}>

![kro validation pipeline stages](/img/stages.svg)

</div>

### Stage 1: Schema Validation

kro starts by validating your custom API schema:

1. **Parses your SimpleSchema definition** - Reads the schema from `spec.schema`
2. **Converts to OpenAPI schema** - Transforms SimpleSchema to standard OpenAPI format
3. **Validates the CRD spec** - Ensures the generated CRD specification is valid

```kro
spec:
  schema:
    spec:
      # kro validates this schema definition
      replicas: integer | default=3
      ports: array
      labels: object
```

### Stage 2: Status Schema Inference

For your custom API's `status` field, kro automatically infers the schema from CEL expressions:

```kro
status:
  endpoint: ${service.status.loadBalancer.ingress[0].hostname}
  replicas: ${deployment.status.availableReplicas}
```

kro inspects these expressions, determines their output types, and generates the OpenAPI schema for your status field automatically. This means you don't need to manually define status field types - kro figures them out from your CEL expressions.

### Stage 3: Resource Naming Validation

kro validates that all resource IDs are valid CEL identifiers. Resource IDs must be valid variable names in CEL - no hyphens, special characters, or starting with numbers.

**Why?** CEL uses resource IDs as variables (like `${deployment.spec.replicas}`). Invalid identifiers would cause CEL syntax errors.

```kro
# ✓ Valid IDs
resources:
  - id: deployment
  - id: configMap
  - id: servicePrimary

# ✗ Invalid IDs
resources:
  - id: my-deployment  # Hyphens not allowed (CEL subtraction operator)
  - id: 1st-service    # Can't start with number
```

### Stage 4: Resource Template Validation

For each resource template, kro:
1. **Validates basic Kubernetes object structure** - Ensures the template has required fields like `apiVersion`, `kind`, and `metadata`
2. **Resolves the OpenAPI schema** - Gets the schema from the API server for the resource type. This works for both built-in Kubernetes resources (like Deployments and Services) and Custom Resource Definitions installed in your cluster.
3. **Extracts CEL expressions and determines expected types** - For each field in the template:
   - **If the field contains a CEL expression**: kro extracts the expression and determines what type the target field expects based on the OpenAPI schema
   - **If the field is a literal value**: kro performs standard OpenAPI validation, just like the kube-api-server does
4. **Validates forEach expressions** - For resources with `forEach`, kro validates that iterator expressions evaluate to arrays and types the iterator variables for use in the template

```kro
resources:
  - id: deployment
    template:
      apiVersion: apps/v1  # kro fetches Deployment schema
      kind: Deployment
      # ... validates template against Deployment schema
```

### Stage 5: AST Analysis and Dependency Graph Building

kro analyzes the Abstract Syntax Tree (AST) of all CEL expressions to understand how they reference each other:

1. **Parses CEL expressions into ASTs** - Converts each expression into its abstract syntax tree representation
2. **Analyzes references** - Identifies what each expression references (schema fields, other resources, functions)
3. **Validates references** - Checks that referenced resources exist in the DAG and functions are declared
4. **Builds the dependency graph** - Creates a directed acyclic graph showing which resources depend on which
5. **Detects circular dependencies** - Identifies any cycles in the dependency graph

At this stage, kro already knows if you're referencing something that doesn't exist or using undeclared functions, and has detected any circular dependencies. See [CEL AST Parsing and Dependency Detection](#cel-ast-parsing-and-dependency-detection) for technical details.

### Stage 6: Expression Type Checking

With expressions extracted and the dependency graph built, kro now performs comprehensive type checking on each CEL expression:

1. **Type-checks the expression** - kro uses CEL's type checker to validate the expression against the typed environment containing all resource schemas. This verifies that all field accesses, function calls, and operations are valid and type-safe.

2. **Infers the expression's output type** - Based on the type checking results, kro determines what type the expression will produce. For `${schema.spec.replicas}`, kro infers an integer. For `${schema.spec.name + "-deployment"}`, kro infers a string.

3. **Validates type compatibility** - kro compares the inferred output type against the expected type (determined earlier from the target field's OpenAPI schema). First, it tries CEL's built-in type assignability check. If that fails, it performs deep structural compatibility checking, which handles complex cases like map/struct conversions and subset validation. See [Type Compatibility Deep Dive](#type-compatibility-deep-dive) for technical details.

**Example:**
```kro
# Expression: ${schema.spec.replicas}
# Inferred output type: integer (from schema.spec definition)
# Expected type: integer (from Deployment.spec.replicas schema)
# Result: ✓ Compatible
```

### Stage 7: Condition Expression Validation

kro validates special condition expressions used in resource lifecycle control:

1. **Validates `readyWhen` expressions** - Ensures readiness conditions are valid CEL expressions that return boolean values
2. **Validates `includeWhen` expressions** - Ensures conditional inclusion expressions are valid CEL expressions that return boolean values

These conditions control when resources are considered ready and whether they should be created at all, so they must always return `true` or `false`.

### Stage 8: RGD Activation

If all validation stages pass, kro activates the ResourceGraphDefinition:

1. **Infers topological order** - Computes the order in which resources will be created based on the dependency graph
2. **Registers the CRD** - Creates the Custom Resource Definition in the cluster for your new API
3. **Starts the microcontroller** - Registers the controller that will reconcile instances of your custom resource
4. **Begins serving instances** - Your ResourceGraphDefinition is now ready to accept instance creation requests

At this point, the RGD is fully validated and operational. When users create instances of your custom API, kro will orchestrate the resources according to the validated graph.

## CEL AST Parsing and Dependency Detection

During Stage 5, kro parses every CEL expression into an Abstract Syntax Tree (AST) and analyzes how expressions reference each other. This enables kro to build a complete dependency graph and detect issues before any resources are created.

### How AST Parsing Works

When kro encounters a CEL expression like `${deployment.spec.replicas}`, it:

1. **Parses the expression into an AST** - The CEL parser breaks down the expression into its component parts: an identifier (`deployment`), a field access (`.spec`), and another field access (`.replicas`).

2. **Walks the AST to find references** - kro traverses the tree to identify what the expression references:
   - Root identifiers (`schema`, `deployment`, `configmap`, etc.)
   - Field accesses (`.spec`, `.data`, `.status`)
   - Function calls (`size()`, `string()`, etc.)
   - Operators (`+`, `*`, `?`, etc.)

3. **Validates references exist** - For each identifier found:
   - If it's `schema`, kro validates the field path exists in your custom schema
   - If it's a resource ID, kro checks that resource is defined in the DAG
   - If it's a function, kro validates it's a declared CEL function

4. **Builds dependency edges** - When resource B references resource A, kro adds an edge A → B in the dependency graph

### Dependency Graph Construction

kro builds a directed acyclic graph (DAG) showing which resources depend on which:

```kro
resources:
  - id: configmap
    template:
      data:
        key: ${schema.spec.value}  # depends on: schema

  - id: deployment
    template:
      spec:
        replicas: ${schema.spec.replicas}  # depends on: schema
        env:
          - value: ${configmap.data.key}    # depends on: configmap
```

kro builds this dependency graph:
```
schema
  ├─→ configmap
  └─→ deployment
        └─→ configmap
```

This graph determines:
- **Creation order**: configmap before deployment
- **Evaluation dependencies**: deployment expressions can only be evaluated after configmap exists
- **Circular dependency detection**: kro validates there are no cycles

### Reference Validation

kro validates all references during AST analysis:

**Resource references**:
```kro
# ✓ Valid: deployment exists
value: ${deployment.spec.replicas}

# ✗ Invalid: typo in resource ID
value: ${deployent.spec.replicas}  # Error: resource 'deployent' not found
```

**Function references**:
```kro
# ✓ Valid: size() is a CEL builtin
condition: ${schema.spec.items.size() > 0}

# ✗ Invalid: undefined function
condition: ${schema.spec.items.length()}  # Error: function 'length' not declared
```

### Circular Dependency Detection

kro detects circular dependencies by checking for cycles in the DAG:

```kro
# ✗ This fails validation
resources:
  - id: serviceA
    template:
      spec:
        port: ${serviceB.spec.targetPort}  # A → B

  - id: serviceB
    template:
      spec:
        targetPort: ${serviceA.spec.port}  # B → A (cycle!)
```

Error: `circular dependency detected: serviceA → serviceB → serviceA`

## Type Compatibility Deep Dive

During Stage 6, kro performs deep structural type compatibility checking. This goes beyond simple type matching to handle complex Kubernetes schemas through recursive comparison of type structures.

### Structural Compatibility

kro doesn't just check if types have the same name - it performs deep structural comparison:

**For primitives**: Checks kind equality (int, string, bool, etc.)

**For lists**: Recursively checks element type compatibility
```kro
# Expression returns: list<int>
# Field expects: list<int>
# Result: ✓ Compatible

# Expression returns: list<string>
# Field expects: list<int>
# Result: ✗ Incompatible - element types don't match
```

**For maps**: Recursively checks key and value type compatibility
```kro
# Expression returns: map<string, int>
# Field expects: map<string, int>
# Result: ✓ Compatible

# Expression returns: map<string, string>
# Field expects: map<string, int>
# Result: ✗ Incompatible - value types don't match
```

**For structs**: Validates that the output struct is a subset of the expected struct (subset semantics)
```kro
# Expression returns: {name: string, replicas: int}
# Field expects: {name: string, replicas: int}
# Result: ✓ Compatible (exact match)

# Expression returns: {name: string}
# Field expects: {name: string, replicas: int}
# Result: ✓ Compatible (output is subset - missing fields are OK)

# Expression returns: {name: string, replicas: int, extra: string}
# Field expects: {name: string, replicas: int}
# Result: ✗ Incompatible (output has extra field 'extra' not in expected)
```

### Map/Struct Compatibility

Kubernetes often treats maps and structs interchangeably (like labels, annotations, data fields). kro handles this intelligently:

**Map → Struct assignment**:
```kro
# Expression: ${schema.spec.labels} (type: map<string, string>)
# Target field: labels (type: struct with string fields)
# kro validates: map keys are strings, map values match struct field types
# Result: ✓ Compatible if all struct fields accept strings
```

**Struct → Map assignment**:
```kro
# Expression: ${configmap.data} (type: struct with dynamic fields)
# Target field: data (type: map<string, string>)
# kro validates: all struct fields are string-compatible
# Result: ✓ Compatible if struct → map conversion is valid
```

### Nested Field Type Checking

kro validates types at any depth by recursively walking the type structure:

```kro
spec:
  template:
    spec:
      containers:
        - env:
            - name: PORT
              # Expression returns: int
              # Field path: spec.template.spec.containers[].env[].value
              # Field expects: string
              # kro resolves the full nested path and checks compatibility
              value: ${schema.spec.port}  # ✗ Type error: int → string
```

### PreserveUnknownFields Handling

For fields with `x-kubernetes-preserve-unknown-fields: true`, kro uses permissive validation:

```kro
# ConfigMap.data has PreserveUnknownFields
configmap:
  data:
    # kro cannot validate structure at build time
    # Any expression type is accepted
    DATABASE_URL: ${schema.spec.dbUrl}
    PORT: ${string(schema.spec.port)}
```

kro still validates:
- Expression syntax is correct
- Referenced resources exist
- Types are internally consistent

But it cannot validate:
- Field names are correct
- Field types match (since schema is unknown)

## Next Steps

- **[Resource Basics](./02-resource-definitions/01-resource-basics.md)** - See how templates are validated
- **[CEL Expressions](./03-cel-expressions.md)** - Learn CEL type system
- **[SimpleSchema](../../../api/specifications/simple-schema.md)** - Define typed schemas
