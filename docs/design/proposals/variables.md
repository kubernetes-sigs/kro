# KREP-011: RGD Variables

## Problem statement

kro does not support intermediate computed values in ResourceGraphDefinitions.
This creates two problems:

1. **No reusable expressions.** When a complex CEL expression like
   `${schema.metadata.name + '-' + schema.spec.environment}` is needed across
   multiple resources, users must copy-paste it into every resource that uses it.
   Updating the expression requires changes in every location, creating
   maintenance burden and risk of inconsistency.

2. **No computed lists inside a resource.** If a user wants to define a list of
   containers as a template and reference it inside a Pod spec, there is no way
   to do so today. `forEach` creates multiple *resources*, but there is no
   mechanism to produce a computed *value* (like an array) that can be embedded
   within a single resource's template.

Refs: 
- https://github.com/kubernetes-sigs/kro/issues/1008
- https://github.com/kubernetes-sigs/kro/issues/781

## Proposal

This proposal introduces **data nodes** — a new resource node type in the RGD
that computes values without creating Kubernetes resources. Data nodes
participate in the DAG alongside template and externalRef nodes, enabling
reusable expressions and computed lists that other resources can reference.

#### Overview

Extend the RGD `resources` field to support a new node type: **data nodes**.
Today, each entry in `resources` is either:

- A **template** node — kro creates and manages a Kubernetes resource.
- An **externalRef** node — kro reads an existing Kubernetes resource (read-only).

This proposal adds:

- A **data** node — kro computes values without creating any Kubernetes resource.

Data nodes participate in the DAG like any other node. They can reference
`schema.spec` fields and other resources, and downstream resources can reference
them. They support `forEach` and `includeWhen`. `readyWhen` is not applicable
to data nodes since they don't create Kubernetes resources — a data node is
considered ready as soon as its expressions evaluate successfully. Specifying
`readyWhen` on a data node is a validation error.

#### Design details

##### Data node structure

A data node uses a `data` field (mutually exclusive with `template` and
`externalRef`). Like `template`, the `data` field is of type
`runtime.RawExtension`, meaning it can hold any valid YAML/JSON structure:
scalars, maps, arrays, or nested objects. This gives data nodes the same
structural flexibility that template nodes have for defining Kubernetes
manifests.

Values within a data node can be:

- **Constants:** literal values the user wants to reuse throughout the RGD.
- **Static variables:** CEL expressions referencing `schema.spec` — the type is
  known at compile time (RGD reconciler) and the value is provided at runtime
  (instance reconciler).
- **Dynamic variables:** CEL expressions referencing other resources in the RGD,
  creating dependencies in the DAG.

```yaml
- id: vars
  data:
    prefix: constant-value
    name: ${schema.spec.name}
    podName: ${myPod.metadata.name}
```

Since `data` is `runtime.RawExtension`, it can also hold non-map structures:

```yaml
# A single scalar value
- id: prefix
  data: ${schema.metadata.name + '-' + schema.spec.environment}

# An array
- id: regions
  data:
    - us-east-1
    - us-west-2
    - eu-west-1
```

##### Referencing data nodes

Other resources reference data node values in two ways:

- **By path:** `${vars.prefix}`, `${vars.name}` — accesses a specific field
  within a map-structured data node.
- **By id:** `${vars}` — receives the entire data structure. If the data node
  is a scalar, this gives the scalar value. If it's a map, this gives the whole
  map.

Standard kro type validation applies at compile time: the type of the field
where the variable is used must match the type of the referenced value.

##### forEach with data nodes

Data nodes support `forEach`, producing an array of computed values rather than
multiple Kubernetes resources. This addresses problem #2 — computed lists that
can be embedded within a single resource template.

**Example: shared container list across Deployment and StatefulSet**

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: shared-pod-template
spec:
  schema:
    apiVersion: v1alpha1
    kind: MyApp
    spec:
      appImage: string
      workers: "[]string"
  resources:
    # Data node: compute a list of containers using forEach
    - id: containers
      forEach:
        - worker: ${schema.spec.workers}
      data:
        name: ${worker}
        image: ${schema.spec.appImage}
        command: ["process", "${worker}"]

    # Both Deployment and StatefulSet reference the same container list
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name + '-deploy'}
        spec:
          template:
            spec:
              containers: ${containers}

    - id: statefulset
      template:
        apiVersion: apps/v1
        kind: StatefulSet
        metadata:
          name: ${schema.metadata.name + '-sts'}
        spec:
          template:
            spec:
              containers: ${containers}
```

Without variables, the user would have to duplicate the container definitions
in both the Deployment and the StatefulSet, and any change to the container
spec would need to be made in both places.

**Example: DRY naming prefix**

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: MyApp
    spec:
      environment: string
      module: string
  resources:
    - id: naming
      data:
        prefix: ${schema.metadata.name + '-' + schema.spec.environment}
        sanitizedModule: ${schema.spec.module.replace('.', '-').replace('_', '-').replace('/', '-')}

    - id: configmap
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${naming.prefix + '-config'}

    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${naming.prefix + '-' + naming.sanitizedModule}
        spec: ...

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${naming.prefix + '-svc'}
        spec: ...
```

**Example: dynamic variable referencing resource output**

```yaml
- id: vpc
  template:
    apiVersion: ec2.services.k8s.aws/v1alpha1
    kind: VPC
    metadata:
      name: ${schema.metadata.name + '-vpc'}
    spec: ...

- id: vpcInfo
  data:
    region: ${vpc.status.ackResourceMetadata.region}
    vpcId: ${vpc.status.vpcID}
    fullName: ${schema.metadata.name + '-' + schema.spec.environment + '-' + vpc.status.ackResourceMetadata.region}

- id: subnet
  template:
    apiVersion: ec2.services.k8s.aws/v1alpha1
    kind: Subnet
    metadata:
      name: ${vpcInfo.fullName + '-subnet'}
    spec:
      vpcID: ${vpcInfo.vpcId}
```

##### DAG representation

Each data node is a single node in the DAG. Dependencies are extracted from its
CEL expressions the same way they are for template and externalRef nodes. If
`vars.podName` references `${myPod.metadata.name}`, then `vars` depends on
`myPod`.

A downstream resource that references `${vars.prefix}` depends on the `vars`
node, which means it waits for all of `vars`'s own dependencies to resolve
before evaluating.

##### Type inference

Template nodes infer types from the Kubernetes resource schema (OpenAPI). Data
nodes need a different approach since there is no Kubernetes schema backing them.

kro already handles this pattern for RGD instance status fields, which are also
schemaless `runtime.RawExtension` content containing CEL expressions. The
existing `parser.ParseSchemalessResource` function (in `pkg/graph/parser/`)
recursively walks a `map[string]interface{}` structure, extracting CEL
expressions as `FieldDescriptor`s and identifying plain (constant) field paths.
Data nodes can reuse this same parser to:

- **Extract CEL expressions** from the data content and feed them into the CEL
  compiler, which infers the return type from the expression and the known types
  of referenced nodes.
- **Identify constant fields** and infer their types from the parsed YAML values
  (`5` is an integer, `"hello"` is a string, `true` is a boolean, `[1, 2, 3]`
  is an integer array, nested maps preserve their structure).

This reuse means data node type inference requires no new parsing infrastructure
— the same code path that handles instance status fields applies directly.

## Other solutions considered

### 1. ConfigMap as variable storage

Users can store values in a ConfigMap resource within the RGD and reference its
fields from other resources:

```yaml
resources:
  - id: vars
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: ${schema.metadata.name + '-vars'}
      data:
        prefix: ${schema.metadata.name + '-' + schema.spec.environment}
        sanitizedModule: ${schema.spec.module.replace('.', '-').replace('_', '-')}

  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${vars.data.prefix + '-deploy'}
      spec: ...
```

Issues with this approach:

- ConfigMap `data` only supports `map[string]string`, so it cannot hold complex
  typed data (integers, booleans, arrays, nested objects).
- It creates an actual Kubernetes resource in the cluster just to hold
  intermediate values, which is wasteful.

### 2. Separate "variable" RGD

Users can create a dedicated RGD with no resources and a schema that serves as
a variable container, then include an instance of it inside the RGD where they
need the variables.

```yaml
# A dedicated "variable" RGD
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: variable-rgd
spec:
  schema:
    apiVersion: v1alpha1
    kind: Variable
    spec:
      complexStringVar: string
      constantVar: integer | default=5
---
# The actual RGD that uses it
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
spec:
  schema:
    spec:
      name: string
  resources:
    - id: variableResource
      template:
        apiVersion: kro.run/v1alpha1
        kind: Variable
        metadata:
          name: variableInstance
        spec:
          complexStringVar: ${schema.spec.name.split('.')[0]}
    - id: myPod
      template:
        apiVersion: v1
        kind: Pod
        metadata:
          name: ${variableResource.spec.complexStringVar}-pod
```

Issues with this approach:

- **RGD proliferation:** Users need a new RGD for every distinct variable
  "shape" they want. Each variable RGD generates a CRD, a dynamic controller,
  and an instance — significant overhead for storing a few computed values.
- **Management burden:** kro must reconcile both the variable RGD and its
  instances. Variable instances go through the full lifecycle (CRD creation,
  instance reconciliation, status tracking) even though they manage no actual
  Kubernetes resources.
- **Hidden dependencies:** Creates implicit coupling between RGDs. Deleting or
  modifying the variable RGD breaks all RGDs that reference its kind. These
  cross-RGD dependencies are not visible in any single RGD's DAG.

## Scoping

#### What is in scope for this proposal?

- The `data` field on resources as a new node type (mutually exclusive with
  `template` and `externalRef`), typed as `runtime.RawExtension`
- Constants, static variables, and dynamic variables within data nodes
- DAG participation: data nodes as dependencies and dependents
- `forEach` support on data nodes to produce computed arrays
- `includeWhen` support on data nodes for conditional computation
- Validation error when `readyWhen` is specified on a data node
- Type inference for data node fields via `parser.ParseSchemalessResource`
- Referencing data nodes by id (whole structure) or by path (specific field)

#### What is not in scope?

- **Schema-level locals:** A separate `locals` field on the schema (option 1
  from the GitHub issue). The resource-level approach is more flexible since it
  can reference resource outputs.
- **Mutable variables:** Data nodes are computed once per reconciliation cycle,
  not reassigned.

## Testing strategy

#### Requirements

- envtest-based integration test environment (existing infrastructure)
- Unit tests for the graph builder changes (CEL parsing, DAG construction, type
  inference for data nodes)

#### Test plan

- **Unit tests:**
  - Data node parsing and validation (constants, static vars, dynamic vars)
  - DAG dependency extraction from data node CEL expressions
  - Type inference for data node fields
  - Mutual exclusivity validation (data vs template vs externalRef)
  - Validation error when `readyWhen` is set on a data node
  - forEach expansion on data nodes producing arrays
- **Integration tests:**
  - RGD with data nodes reconciles successfully and generates correct CRD
  - Instance reconciliation evaluates data node expressions in correct order
  - Data nodes with `forEach` produce correct arrays
  - Data nodes with `includeWhen` conditionally compute values
  - Downstream resources correctly receive data node values
  - Circular dependency detection involving data nodes
- **E2E tests:**
  - Full lifecycle: RGD with data nodes -> instance creation -> resource
    creation with correct computed values -> update -> deletion

## Discussion and notes

#### Questions for reviewers

1. **Data-to-data references:** Should one data node be able to reference
   another data node? This enables composition (e.g., `naming.prefix` used in
   a `fullLabels` data node) but adds complexity. If allowed, cycle detection
   must cover data-to-data edges. If disallowed, users must inline expressions
   or combine values into a single data node.
