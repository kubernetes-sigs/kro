---
sidebar_position: 2
---

# Simple Schema

**kro** follows a different approach for defining your API schema and shapes. It
leverages a human-friendly and readable syntax that is OpenAPI specification
compatible. Here's a comprehensive example:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: web-application
spec:
  schema:
    apiVersion: v1alpha1
    kind: WebApplication
    spec:
      # Basic types
      name: string | required=true immutable=true description="My Name"
      replicas: integer | default=1 minimum=1 maximum=100
      image: string | required=true

      # Unstructured objects
      values: object | required=true

      # Structured type
      ingress:
        enabled: boolean | default=false
        host: string | default="example.com"
        path: string | default="/"

      # Array type
      ports: "[]integer"

      # Map type
      env: "map[string]myType"

    # Custom Types
    types:
      myType:
        value1: string | required=true
        value2: integer | default=42

    status:
      # Status fields with auto-inferred types
      availableReplicas: ${deployment.status.availableReplicas}
      serviceEndpoint: ${service.status.loadBalancer.ingress[0].hostname}
```

## Type Definitions

### Basic Types

kro supports these foundational types:

- `string`: Text values
- `integer`: Whole numbers
- `boolean`: True/False values
- `float`: Decimal numbers

For example:

```yaml
name: string
age: integer
enabled: boolean
price: float
```

### Structure Types

You can create complex objects by nesting fields. Each field can use any type,
including other structures:

```yaml
# Simple structure
address:
  street: string
  city: string
  zipcode: string

# Nested structures
user:
  name: string
  address: # Nested object
    street: string
    city: string
  contacts: "[]string" # Array of strings
```

### Unstructured Objects

Unstructured objects are declared using `object` as a type.

:::warning

This disables the field-validation normally offered by kro, and forwards the values to your RGD as-is. This is generally discouraged and should therefore be used with caution. In most cases, using a structured object is a better approach.

:::

```yaml
kind: ResourceGraphDefintion
metadata: {}
spec:
  schema:
    spec:
      additionalHelmChartValues: object
```

This allows you to pass data to your CRDs directly in cases where the schema is not known in advance. This type supports any valid object, and can mix and match different primitives as well as structured types.

```yaml
apiVersion: kro.run/v1alpha1
kind: CRDWithUnstructuredObjects
metadata:
  name: test-instance
spec:
  additionalHelmChartValues:
    boolean-value: true
    numeric-value: 42
    structural-type:
      with-additional:
        nested: fields
    string-value: my-string
    mapping-value:
      - item1
      - item2
      - item3
```

### Array Types

Arrays are denoted using `[]` syntax:

- Basic arrays: `[]string`, `[]integer`, `[]boolean`

Examples:

```yaml
tags: []string
ports: []integer
```

### Map Types

Maps are key-value pairs denoted as `map[keyType]valueType`:

- `map[string]string`: String to string mapping
- `map[string]integer`: String to integer mapping

Examples:

```yaml
labels: "map[string]string"
metrics: "map[string]float"
```

### Custom Types

Custom types are specified in the separate `types` section.
They provide a map of names to type specifications that follow the simple schema.

Example:

```yaml
schema:
  types:
    Person:
      name: string
      age: integer
  spec:
    people: '[]Person | required=true`
```

## Validation and Documentation

Fields can have multiple markers for validation and documentation:

```yaml
name: string | required=true default="app" description="Application name"
replicas: integer | default=3 minimum=1 maximum=10
mode: string | enum="debug,info,warn,error" default="info"
```

### Supported Markers

- `required=true`: Field must be provided
- `default=value`: Default value if not specified
- `description="..."`: Field documentation
- `enum="value1,value2"`: Allowed values
- `minimum=value`: Minimum value for numbers
- `maximum=value`: Maximum value for numbers
- `immutable=true`: Field cannot be changed after creation
- `pattern="regex"`: Regular expression pattern for string validation
- `minLength=number`: Minimum length for strings
- `maxLength=number`: Maximum length for strings
- `uniqueItems=true`: Ensures array elements are unique
- `minItems=number`: Minimum number of items in arrays
- `maxItems=number`: Maximum number of items in arrays

Multiple markers can be combined using the `|` separator.

### String Validation Markers

String fields support additional validation markers:

- **`pattern="regex"`**: Validates the string against a regular expression pattern
- **`minLength=number`**: Sets the minimum number of characters
- **`maxLength=number`**: Sets the maximum number of characters

Examples:

```yaml
# Email validation
email: string | pattern="^[\\w\\.-]+@[\\w\\.-]+\\.\\w+$" required=true

# Username with length constraints and pattern
username: string | minLength=3 maxLength=15 pattern="^[a-zA-Z0-9_]+$"

# Country code format
countryCode: string | pattern="^[A-Z]{2}$" minLength=2 maxLength=2

# Password with minimum length
password: string | minLength=8 description="Password must be at least 8 characters"
```

### Array Validation Markers

Array fields support validation markers to ensure data quality:

- **`uniqueItems=true`**: Ensures all elements in the array are unique
- **`uniqueItems=false`**: Allows duplicate elements (default behavior)
- **`minItems=number`**: Sets the minimum number of elements required in the array
- **`maxItems=number`**: Sets the maximum number of elements allowed in the array

Examples:

```yaml
# Unique tags with size constraints
tags: "[]string" | uniqueItems=true minItems=1 maxItems=10 description="1-10 unique tags"

# Unique port numbers with minimum requirement
ports: "[]integer" | uniqueItems=true minItems=1 description="At least one unique port"

# Allow duplicate comments with size limits
comments: "[]string" | uniqueItems=false maxItems=50 description="Up to 50 comments"

# Complex validation with multiple markers
roles: "[]string" | uniqueItems=true minItems=1 maxItems=5 required=true description="1-5 unique user roles"

# Optional array with size constraints
priorities: "[]integer" | minItems=0 maxItems=3 description="Up to 3 priority levels"
```

For example:

```yaml
name: string | required=true default="app" description="Application name"
id: string | required=true immutable=true description="Unique identifier"
replicas: integer | default=3 minimum=1 maximum=10
price: float | minimum=0.01 maximum=999.99
mode: string | enum="debug,info,warn,error" default="info"
email: string | pattern="^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$" description="Valid email address"
username: string | minLength=3 maxLength=20 pattern="^[a-zA-Z0-9_]+$"
tags: "[]string" | uniqueItems=true minItems=1 maxItems=10 description="1-10 unique tags"
```

:::warning Floating Point Precision
When using `float` or `double` types in CEL expressions (particularly in `readyWhen` or `includeWhen` conditions), be aware of floating point precision issues that could cause unexpected behavior. Avoid comparing floating point values for equality in conditional logic. Prefer using `string`, `integer`, or `boolean` types whenever possible to avoid precision-related oscillations in resource state.
:::

## Status Fields

Status fields use CEL expressions to reference values from resources. kro
automatically:

- Infers the correct types from the expressions
- Validates that referenced resources exist at ResourceGraphDefinition creation time
- Updates values when the underlying resources change
- Validates type compatibility using CEL's native type system

```yaml
status:
  # Types are inferred from the referenced fields
  availableReplicas: ${deployment.status.availableReplicas}  # integer
  endpoint: ${service.status.loadBalancer.ingress[0].hostname}  # string
  metadata: ${deployment.metadata}  # object
```

### Single vs Multi-Expression Fields

Status fields can contain either a single CEL expression or multiple expressions concatenated together:

**Single Expression Fields** can be any type:
```yaml
status:
  replicas: ${deployment.status.replicas}  # integer
  metadata: ${deployment.metadata}  # object
  name: ${deployment.metadata.name}  # string
  ready: ${deployment.status.conditions.exists(c, c.type == 'Available')}  # boolean
```

**Multi-Expression Fields** (string templating) must contain only string expressions:
```yaml
status:
  # ✓ Valid - all expressions return strings
  endpoint: "https://${service.metadata.name}.${service.metadata.namespace}.svc.cluster.local"

  # ✓ Valid - explicit string conversion
  summary: "Replicas: ${string(deployment.status.replicas)}, Ready: ${string(deployment.status.ready)}"

  # ✗ Invalid - concatenating non-string types
  invalid: "${deployment.status.replicas}-${deployment.metadata}"  # Will fail validation
```

Multi-expression fields are useful for string templating scenarios like constructing URLs, connection strings, or IAM policies:

```yaml
status:
  iamPolicy: |
    {
      "Effect": "Allow",
      "Resource": "arn:aws:s3:::${bucket.metadata.name}/*",
      "Principal": "${serviceAccount.metadata.name}"
    }
```

:::tip
Use explicit `string()` conversions when concatenating non-string values to ensure type compatibility.

Alternatively, you can use CEL's built-in `format()` function for string formatting:
```yaml
status:
  endpoint: ${"https://%s.%s.svc.cluster.local".format([service.metadata.name, service.metadata.namespace])}
```

The `${...}${...}` templating syntax is a kro convenience feature that makes common string concatenation patterns more readable.
:::

## Default Status Fields

kro automatically injects two fields to every instance's status:

### 1. Conditions

An array of condition objects tracking the instance's state:

```yaml
status:
  conditions:
    - type: string # e.g., "Ready", "InstanceManaged", "GraphResolved", "ResourcesReady"
      status: string # "True", "False", "Unknown"
      lastTransitionTime: string
      observedGeneration: integer
      reason: string
      message: string
```

kro provides a hierarchical condition structure:

- `Ready`: Top-level condition indicating the instance is fully operational
  - `InstanceManaged`: Instance finalizers and labels are properly set
  - `GraphResolved`: Runtime graph has been created and resources resolved
  - `ResourcesReady`: All resources in the graph are created and ready

The `Ready` condition aggregates the state of all sub-conditions and only becomes `True` when all sub-conditions are `True`. Each condition includes an `observedGeneration` field that tracks which generation of the instance the condition reflects.

### 2. State

A high-level summary of the instance's status:

```yaml
status:
  state: string # ACTIVE, IN_PROGRESS, FAILED, DELETING, ERROR
```

:::tip

`conditions` and `state` are reserved words. If defined in your schema, kro will
override them with its own values.

:::

## Additional Printer Columns

You can define `additionalPrinterColumns` for the created CRD through the ResourceGraphDefinition by setting them on `spec.schema.additionalPrinterColumns`.

```yaml
schema:
  spec:
    image: string | default="nginx"
  status:
    availableReplicas: ${deployment.status.availableReplicas}
  additionalPrinterColumns:
    - jsonPath: .status.availableReplicas
      name: Available replicas
      type: integer
    - jsonPath: .spec.image
      name: Image
      type: string
```
