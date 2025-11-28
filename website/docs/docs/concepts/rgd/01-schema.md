---
sidebar_position: 1
---

# Schema Definition

The schema section of a ResourceGraphDefinition defines the shape of your custom API. When you create an RGD, kro uses this schema to generate a new Custom Resource Definition (CRD) that users can instantiate.

## What the Schema Defines

The schema section specifies:

- **API identification**: The `apiVersion`, `kind`, and optionally `group` for your custom resource
- **Spec fields**: What inputs users provide when creating instances
- **Status fields**: What runtime information kro surfaces from managed resources
- **Custom types**: Reusable type definitions for complex schemas
- **Additional printer columns**: Custom columns for `kubectl get` output

## Basic Structure

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: application
spec:
  schema:
    apiVersion: v1alpha1        # Your API version
    kind: Application            # Your custom resource kind

    spec:                        # User-provided fields
      name: string
      replicas: integer
      image: string

    status:                      # Runtime fields from resources
      availableReplicas: ${deployment.status.availableReplicas}
      endpoint: ${service.status.loadBalancer.ingress[0].hostname}
```

## API Identification

### Group

The `group` field sets the API group for your generated CRD. If omitted, it defaults to `kro.run`.

```yaml
schema:
  apiVersion: v1alpha1
  kind: Application
  group: mycompany.io  # Creates applications.mycompany.io CRD
```

This allows you to organize your custom APIs under your own domain, making the full API `mycompany.io/v1alpha1`.

## The spec Section

The `spec` section defines what users can configure when they create an instance of your API. These are the input fields that control resource behavior.

### Defining Spec Fields

kro uses [SimpleSchema](../../../api/specifications/simple-schema.md) syntax for defining types:

```yaml
spec:
  # Basic types with validation
  name: string | required=true
  replicas: integer | default=3 minimum=1 maximum=100
  enabled: boolean | default=false

  # Structured types
  ingress:
    enabled: boolean | default=false
    host: string | default="example.com"
    path: string | default="/"

  # Collections
  env: "map[string]string"
  ports: "[]integer"
```

Common validation markers:
- `required=true` - Field must be provided
- `default=value` - Default value if omitted
- `minimum=n` / `maximum=n` - Numeric constraints
- `enum="val1,val2"` - Allowed values
- `pattern="regex"` - String pattern validation
- `description="..."` - Field documentation

See [SimpleSchema](../../../api/specifications/simple-schema.md) for complete syntax reference.

## The status Section

The `status` section defines what runtime information kro exposes from your managed resources. Status fields use CEL expressions to reference values from the resources in your graph.

### Status Fields with CEL Expressions

```yaml
resources:
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      # ... deployment spec ...

  - id: service
    template:
      apiVersion: v1
      kind: Service
      # ... service spec ...

schema:
  status:
    # Reference resource fields directly
    availableReplicas: ${deployment.status.availableReplicas}

    # Extract nested values
    serviceIP: ${service.spec.clusterIP}

    # Construct composite values
    endpoint: "http://${service.status.loadBalancer.ingress[0].hostname}"
```

kro automatically:
- **Infers proper types** from CEL expressions by inspecting what the expression returns (integers, strings, objects, arrays, etc.)
- **Validates expressions** when you create the RGD (not at runtime)
- **Type-checks** expressions against actual Kubernetes schemas
- **Updates values** whenever the underlying resources change

This means status fields have strongly-typed schemas in the generated CRD, not arbitrary objects. If a CEL expression returns an integer, the status field will be typed as an integer in the CRD.

### Structured Status Fields

Status fields can be scalar values, structured objects, or arrays:

```yaml
status:
  # Scalar values
  replicas: ${deployment.status.replicas}

  # Structured objects
  connection:
    host: ${service.spec.clusterIP}
    port: ${service.spec.ports[0].port}
    protocol: "TCP"

  # Arrays
  endpoints:
    - ${service.status.loadBalancer.ingress[0].hostname}
    - ${service.status.loadBalancer.ingress[1].hostname}

  # Nested structures
  deployment:
    metadata:
      name: ${deployment.metadata.name}
      namespace: ${deployment.metadata.namespace}
    status:
      ready: ${deployment.status.readyReplicas}
      total: ${deployment.status.replicas}
```

### Using CEL Functions in Status

Status fields support the full power of CEL expressions, including built-in functions:

```yaml
status:
  # Type conversions
  replicasAsString: ${string(deployment.status.replicas)}

  # Filtering arrays
  readyPods: ${deployment.status.conditions.filter(c, c.type == "Ready")}

  # Mapping arrays
  podNames: ${pods.items.map(p, p.metadata.name)}

  # Conditional logic
  isHealthy: ${deployment.status.availableReplicas >= deployment.spec.replicas}

  # Complex expressions
  healthStatus: ${deployment.status.availableReplicas >= deployment.spec.replicas ? "healthy" : "degraded"}

  # Combining multiple operations
  activePodCount: ${pods.items.filter(p, p.status.phase == "Running").size()}
```

### String Templating in Status

Status fields can use multiple CEL expressions for string construction:

```yaml
status:
  # Single expression - can be any type
  replicas: ${deployment.status.replicas}  # integer

  # Multiple expressions - must all be strings
  connectionString: "postgresql://${secret.data.username}:${secret.data.password}@${service.spec.clusterIP}:5432"
```

### Built-in Status Fields

kro automatically adds two fields to every instance status:

**conditions**: An array tracking the instance state
```yaml
status:
  conditions:
    - type: Ready              # Overall readiness
      status: "True"
      lastTransitionTime: "..."
      reason: "..."
      message: "..."
```

**state**: A high-level summary
```yaml
status:
  state: ACTIVE  # ACTIVE | IN_PROGRESS | FAILED | DELETING | ERROR
```

:::warning
`conditions` and `state` are reserved fields. kro will override them if you define them in your schema.
:::

## How kro Uses the Schema

### 1. CRD Generation

When you create an RGD, kro converts your SimpleSchema into an OpenAPI v3 schema and generates a CRD:

```yaml
# Your RGD schema
schema:
  apiVersion: v1alpha1
  kind: Application
  spec:
    name: string | required=true
```

kro generates:
```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: applications.v1alpha1
spec:
  versions:
    - name: v1alpha1
      schema:
        openAPIV3Schema:
          properties:
            spec:
              properties:
                name:
                  type: string
              required: [name]
```

### 2. Instance Validation

When users create instances, Kubernetes validates them against the generated CRD schema before kro processes them. This means:
- Invalid instances are rejected immediately
- Type mismatches are caught at admission time
- Required fields are enforced by Kubernetes itself

### 3. Status Updates

kro continuously evaluates status expressions and updates instance status as resources change. If a deployment's replica count changes, the corresponding status field updates automatically.

## Custom Types

For complex schemas, you can define reusable custom types:

```yaml
schema:
  types:
    ContainerConfig:
      image: string | required=true
      tag: string | default="latest"
      env: "map[string]string"

  spec:
    primary: ContainerConfig
    sidecars: "[]ContainerConfig"
```

Custom types are expanded inline when kro generates the CRD.

## Additional Printer Columns

Control what `kubectl get` displays:

```yaml
schema:
  spec:
    name: string
    replicas: integer

  status:
    availableReplicas: ${deployment.status.availableReplicas}

  additionalPrinterColumns:
    - name: Replicas
      type: integer
      jsonPath: .spec.replicas

    - name: Available
      type: integer
      jsonPath: .status.availableReplicas

    - name: Age
      type: date
      jsonPath: .metadata.creationTimestamp
```

This produces:
```bash
$ kubectl get applications
NAME     REPLICAS   AVAILABLE   AGE
my-app   5          5           10m
```

## Complete Example

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
      # Basic configuration
      name: string | required=true
      replicas: integer | default=3 minimum=1
      image: string | required=true

      # Structured configuration
      ingress:
        enabled: boolean | default=false
        host: string
        path: string | default="/"

      # Collections
      env: "map[string]string"
      ports: "[]integer" | default=[80]

    status:
      # Resource state
      availableReplicas: ${deployment.status.availableReplicas}
      serviceIP: ${service.spec.clusterIP}

      # Conditional fields (only present if ingress enabled)
      ingressHost: ${ingress.spec.rules[0].host}

    additionalPrinterColumns:
      - name: Replicas
        type: integer
        jsonPath: .spec.replicas
      - name: Available
        type: integer
        jsonPath: .status.availableReplicas
      - name: Image
        type: string
        jsonPath: .spec.image

  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        # ... deployment configuration using ${schema.spec.*} ...

    - id: service
      template:
        apiVersion: v1
        kind: Service
        # ... service configuration ...

    - id: ingress
      includeWhen:
        - ${schema.spec.ingress.enabled}
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        # ... ingress configuration ...
```

## Next Steps

- **[SimpleSchema Reference](../../../api/specifications/simple-schema.md)** - Complete syntax and validation rules
- **[Resource Definitions](./02-resource-definitions/01-resource-basics.md)** - Learn how to use schema values in resource templates
- **[CEL Expressions](./03-cel-expressions.md)** - Master expression syntax for status fields
