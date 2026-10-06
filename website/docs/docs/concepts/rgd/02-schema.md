---
sidebar_position: 2
---

# Schema Definition

The schema section of a ResourceGraphDefinition defines the shape of your custom API. When you create an RGD, kro uses this schema to generate a new Custom Resource Definition (CRD) that users can instantiate.

## What the Schema Defines

The schema section specifies:

- **API identification**: The `apiVersion`, `kind`, and optionally `group` for your custom resource
- **Scope**: Whether instances are namespaced or cluster-scoped
- **Spec fields**: What inputs users provide when creating instances
- **Status fields**: What runtime information kro surfaces from managed resources
- **Custom types**: Reusable type definitions for complex schemas
- **Additional printer columns**: Custom columns for `kubectl get` output
- **Short names and categories**: Optional kubectl aliases and grouping for the generated API

## Basic Structure

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: application
spec:
  schema:
    apiVersion: v1alpha1         # Your API version
    kind: Application            # Your custom resource kind

    spec:                        # User-provided fields
      name: string
      replicas: integer
      image: string

    status:                      # Runtime fields from resources
      availableReplicas: ${deployment.status.availableReplicas}
      endpoint: ${service.status.loadBalancer.ingress[0].hostname}

  resources:                     # The resources the status fields read from
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
          template:
            metadata:
              labels:
                app: ${schema.spec.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
        spec:
          type: LoadBalancer
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - port: 80
```

This page covers `spec.schema`. The `resources` section is covered in
[Resource Basics](./03-resource-basics.md).

## API Identification

### Group

The `group` field sets the API group for your generated CRD. If omitted, it defaults to `kro.run`.

```kro
schema:
  apiVersion: v1alpha1
  kind: Application
  group: mycompany.io  # Creates applications.mycompany.io CRD
```

This allows you to organize your custom APIs under your own domain, making the full API `mycompany.io/v1alpha1`.

### Scope

The `scope` field determines whether the generated CRD is namespaced or cluster-scoped:

```yaml
schema:
  apiVersion: v1alpha1
  kind: ClusterPolicy
  scope: Cluster  # or "Namespaced" (default)
```

| Value | Description |
|-------|-------------|
| `Namespaced` | Instances exist within a namespace (default) |
| `Cluster` | Instances are cluster-wide, with no namespace |

:::warning Cluster-Scoped Instances
When using `scope: Cluster`, all namespaced resources must explicitly set `metadata.namespace` (hardcoded or via CEL). kro validates this at RGD creation time.
:::

```yaml
schema:
  apiVersion: v1alpha1
  kind: Tenant
  scope: Cluster
  spec:
    targetNamespace: string | required=true

resources:
  # Template with explicit namespace
  - id: configmap
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: ${schema.metadata.name}-config
        namespace: ${schema.spec.targetNamespace}  # Required

  # External ref also requires explicit namespace
  - id: existingSecret
    externalRef:
      apiVersion: v1
      kind: Secret
      metadata:
        name: db-credentials
        namespace: ${schema.spec.targetNamespace}  # Required
```

:::note
The `scope` field is immutable after creation.
:::

### Plural

The generated CRD is named `<plural>.<group>`. kro derives it by pluralizing the lowercased `kind` using English rules,
which is wrong for some words: `PodInfo` becomes `podinfoes`.

Set `plural` to override it as needed:

```yaml
schema:
  apiVersion: v1alpha1
  kind: PodInfo
  plural: podinfos
```

```yaml
- apiGroups:
    - kro.run
  resources:
    - podinfos
```

Must be a valid [RFC 1035 label name](https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#rfc-1035-label-names).

:::note
The `plural` field is immutable after creation, because it is part of the generated CRD's name.
:::

### Short Names and Categories

`shortNames` adds kubectl aliases for the generated CRD, and `categories` makes instances show up when users list a category.

```yaml
schema:
  apiVersion: v1alpha1
  kind: WebApplication
  shortNames:
    - wa
    - webapp
  categories:
    - kro
```

This lets users run:

```bash
kubectl get wa
kubectl get webapp
kubectl get kro
```

instead of the full plural resource name:

```bash
kubectl get webapplications
```

Short names and categories must be valid [RFC 1035 label names](https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#rfc-1035-label-names). Duplicate entries are rejected.

## The spec Section

The `spec` section defines what users can configure when they create an instance of your API. These are the input fields that control resource behavior.

### Defining Spec Fields

kro uses [SimpleSchema](../../../api/specifications/simple-schema.md) syntax for defining types:

```simpleschema
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

```kro
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

```kro
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

```kro
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
  healthStatus: '${deployment.status.availableReplicas >= deployment.spec.replicas ? "healthy" : "degraded"}'

  # Combining multiple operations
  activePodCount: ${pods.items.filter(p, p.status.phase == "Running").size()}
```

### String Templating in Status

Status fields can use multiple CEL expressions for string construction:

```kro
status:
  # Single expression - can be any type
  replicas: ${deployment.status.replicas}  # integer

  # Multiple expressions - must all be strings
  connectionString: "postgresql://${secret.data.username}:${secret.data.password}@${service.spec.clusterIP}:5432"
```

### Built-in Status Fields

kro automatically adds two fields to every instance status:

**conditions**: An array tracking the instance state
```kro
status:
  conditions:
    - type: Ready              # Overall readiness
      status: "True"
      lastTransitionTime: "..."
      reason: "..."
      message: "..."
```

**state**: A high-level summary
```kro
status:
  state: ACTIVE  # ACTIVE | IN_PROGRESS | FAILED | DELETING | ERROR
```

You can define your own `conditions` to publish domain-specific conditions
instead of kro's built-ins. See [Custom Status Conditions](./04-status-conditions.md).

:::warning
`state` is a reserved field. kro will override it if you define it in your schema.
:::

## How kro Uses the Schema

### 1. CRD Generation

When you create an RGD, kro converts your SimpleSchema into an OpenAPI v3 schema and generates a CRD:

```kro
# Your RGD schema
schema:
  apiVersion: v1alpha1
  kind: Application
  spec:
    name: string | required=true
```

kro generates a CRD named `applications.kro.run` whose `v1alpha1` schema
contains (excerpt):
```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: applications.kro.run
spec:
  group: kro.run
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
  # ...
```
#### Adding Labels and Annotations to CRDs

You can also apply custom labels and annotations to the generated CRD using the `metadata` field.
This is useful for organizing CRDs and integrating with external tools.

```kro
schema:
  apiVersion: v1alpha1
  kind: Application
  metadata:
    labels:
      team: platform
      managed-by: kro
      environment: production
    annotations:
      description: "Application resource for managing web applications"
  spec:
    name: string | required=true
```
The labels and annotations you define here will be applied to the CRD itself (not to instances of the CRD).

#### Validating Instance Names

Use `metadata.nameValidation` to add OpenAPI constraints to the names of
instances created from the generated CRD. The value uses marker-only
SimpleSchema syntax; the `string` type is implicit.

```kro
schema:
  apiVersion: v1alpha1
  kind: Application
  metadata:
    nameValidation: maxLength=30 pattern="^[a-z][a-z0-9-]*$"
```

Supported markers are `minLength`, `maxLength`, `pattern`, `enum`, and the
field-scoped `validation` CEL marker. Type declarations, `required`, and
defaults are not supported. These rules supplement Kubernetes' built-in name
validation; they cannot make an otherwise invalid Kubernetes name valid.

Kubernetes applies the rules to the final name, including a name generated
from `metadata.generateName`. A limit of `maxLength=30` permits exactly 30
characters, but it does not guarantee that names derived for child resources
by appending text will fit those resources' limits.

### 2. Instance Validation

When users create instances, Kubernetes validates them against the generated CRD schema before kro processes them. This means:
- Invalid instances are rejected immediately
- Type mismatches are caught at admission time
- Required fields are enforced by Kubernetes itself

### 3. Status Updates

kro continuously evaluates status expressions and updates instance status as resources change. If a deployment's replica count changes, the corresponding status field updates automatically.

### 4. Schema Updates

When you update an RGD's schema, kro checks whether the changes are compatible with existing instances. Changes like removing fields, changing types, or adding required fields without defaults are considered breaking and will be blocked by default. See [Breaking Changes](./01-overview.md#breaking-changes) for how to allow breaking changes when needed.

## Custom Types

For complex schemas, you can define reusable custom types:

```kro
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

### Recursive Custom Types

Custom types can reference other custom types. kro resolves dependencies automatically and detects cyclic references:

```yaml
schema:
  types:
    Address:
      street: string
      city: string
    Person:
      name: string
      address: Address
  spec:
    owner: Person
```

For more details about SimpleSchema syntax and custom types, see the [SimpleSchema Specification](../../../api/specifications/simple-schema.md).

## Additional Printer Columns

Control what `kubectl get` displays:

```kro
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

```kro
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
      ports: "[]integer | default=[80]"

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
        metadata:
          name: ${schema.spec.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.spec.name}
          template:
            metadata:
              labels:
                app: ${schema.spec.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
                  ports:
                    - containerPort: ${schema.spec.ports[0]}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
        spec:
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - port: 80
              targetPort: ${schema.spec.ports[0]}

    - id: ingress
      includeWhen:
        - ${schema.spec.ingress.enabled}
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: ${schema.spec.name}
        spec:
          rules:
            - host: ${schema.spec.ingress.host}
              http:
                paths:
                  - path: ${schema.spec.ingress.path}
                    pathType: Prefix
                    backend:
                      service:
                        name: ${service.metadata.name}
                        port:
                          number: 80
```

## Next Steps

- **[SimpleSchema Reference](../../../api/specifications/simple-schema.md)** - Complete syntax and validation rules
- **[Resource Definitions](./03-resource-basics.md)** - Learn how to use schema values in resource templates
- **[CEL Expressions](../expressions/01-cel-expressions.md)** - Master expression syntax for status fields
