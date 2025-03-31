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
      name: string | required=true description="My Name"
      replicas: integer | default=1 minimum=1 maximum=100
      image: string | required=true

      # Structured type
      ingress:
        enabled: boolean | default=false
        host: string | default="example.com"
        path: string | default="/"

      # Array type
      ports: "[]integer"

      # Map type
      env: "map[string]string"

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

Multiple markers can be combined using the `|` separator.

For example:

```yaml
name: string | required=true default="app" description="Application name"
replicas: integer | default=3 minimum=1 maximum=10
price: float | minimum=0.01 maximum=999.99
mode: string | enum="debug,info,warn,error" default="info"
```

## Status Fields

Status fields use CEL expressions to reference values from resources. kro
automatically:

- Infers the correct types from the expressions
- Validates that referenced resources exist
- Updates values when the underlying resources change

```yaml
status:
  # Types are inferred from the referenced fields
  availableReplicas: ${deployment.status.availableReplicas}
  endpoint: ${service.status.loadBalancer.ingress[0].hostname}
```

## Default Status Fields

kro automatically injects two fields to every instance's status:

### 1. Conditions

An array of condition objects tracking the instance's state:

```yaml
status:
  conditions:
    - type: string # e.g., "Ready", "Progressing"
      status: string # "True", "False", "Unknown"
      lastTransitionTime: string
      reason: string
      message: string
```

Common condition types:

- `Ready`: Instance is fully reconciled
- `Progressing`: Working towards desired state
- `Degraded`: Operational but not optimal
- `Error`: Reconciliation error occurred

### 2. State

A high-level summary of the instance's status:

```yaml
status:
  state: string # Ready, Progressing, Degraded, Unknown, Deleting
```

:::tip

`conditions` and `state` are reserved words. If defined in your schema, kro will
override them with its own values.

:::
