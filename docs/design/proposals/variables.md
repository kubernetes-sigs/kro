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

This proposal introduces **variable nodes** — a new resource node type in the RGD
that computes values without creating Kubernetes resources. Variable nodes
participate in the DAG alongside template and externalRef nodes, enabling
reusable expressions and computed lists that other resources can reference.

### Overview

Extend the RGD `resources` field to support a new node type: **variable nodes**.
Today, each entry in `resources` is either:

- A **template** node — kro creates and manages a Kubernetes resource.
- An **externalRef** node — kro reads an existing Kubernetes resource (read-only).

This proposal adds:

- A **variable** node — kro computes values without creating any Kubernetes resource.

Variable nodes participate in the DAG like any other node. They can reference
`schema.spec` fields and other resources, and downstream resources can reference
them. They support `forEach`, `includeWhen`, and `readyWhen`.

### Design details

#### Variable node structure

A variable node uses a `variable` field (mutually exclusive with `template` and
`externalRef`). The `variable` field must be a map (key-value pairs). Values
within the map can be constants, CEL expressions, arrays, or nested objects.

Values within a variable node can be:

- **Constants:** literal values the user wants to reuse throughout the RGD.
- **Static variables:** CEL expressions referencing `schema.spec` — the type is
  known at compile time (RGD reconciler) and the value is provided at runtime
  (instance reconciler).
- **Dynamic variables:** CEL expressions referencing other resources in the RGD,
  creating dependencies in the DAG.

```yaml
- id: vars
  variable:
    prefix: constant-value
    name: ${schema.spec.name}
    podName: ${myPod.metadata.name}
```

#### Referencing variable nodes

Variable nodes can be referenced by any resource or other variable nodes in the RGD,
as long as it's not cyclic. References work in two ways:

- **By path:** `${vars.prefix}`, `${vars.name}` — accesses a specific field
  within the variable node's map.
- **By id:** `${vars}` — receives the entire map structure.

Standard kro type validation applies at compile time: the type of the field
where the variable is used must match the type of the referenced value.

A variable node's top-level keys must not match its own id. Since variable node fields
are referenced as `${<id>.<key>}`, a key matching the id would create confusion
about whether `${naming.naming}` refers to the node itself or a nested field.
For example, the following is invalid:

```yaml
- id: naming
  variable:
    naming: ${schema.spec.name}  # error: top-level key "naming" matches node id
    prefix: ${schema.spec.name + '-' + schema.spec.environment}
```

#### forEach with variable nodes

Variable nodes support `forEach`, producing an array of computed values rather than
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
    # Variable node: compute a list of containers using forEach
    - id: containers
      forEach:
        - worker: ${schema.spec.workers}
      variable:
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
      variable:
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
  variable:
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

#### DAG representation

Each variable node is a single node in the DAG. Dependencies are extracted from its
CEL expressions the same way they are for template and externalRef nodes. If
`vars.podName` references `${myPod.metadata.name}`, then `vars` depends on
`myPod`.

A downstream resource that references `${vars.prefix}` depends on the `vars`
node, which means it waits for all of `vars`'s own dependencies to resolve
before evaluating.

#### Type inference

Template nodes infer types from the Kubernetes resource schema (OpenAPI). Variable
nodes need a different approach since there is no Kubernetes schema backing them.

kro already handles this pattern for RGD instance status fields, which are also
schemaless `runtime.RawExtension` content containing CEL expressions. The
existing `parser.ParseSchemalessResource` function (in `pkg/graph/parser/`)
recursively walks a `map[string]interface{}` structure, extracting CEL
expressions as `FieldDescriptor`s and identifying plain (constant) field paths.
Variable nodes can reuse this same parser to:

- **Extract CEL expressions** from the data content and feed them into the CEL
  compiler, which infers the return type from the expression and the known types
  of referenced nodes.
- **Identify constant fields** and infer their types from the parsed YAML values
  (`5` is an integer, `"hello"` is a string, `true` is a boolean, `[1, 2, 3]`
  is an integer array, nested maps preserve their structure).

This reuse means variable node type inference requires no new parsing infrastructure
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

### What is in scope for this proposal?

- The `variable` field on resources as a new node type (mutually exclusive with
  `template` and `externalRef`), must be a map of key-value pairs
- Constants, static variables, and dynamic variables within variable nodes
- DAG participation: variable nodes as dependencies and dependents
- `forEach` support on variable nodes to produce computed arrays
- `includeWhen` support on variable nodes for conditional computation
- `readyWhen` support on variable nodes
- Type inference for variable node fields via `parser.ParseSchemalessResource`
- Referencing variable nodes by id (whole map) or by path (specific field)

### What is not in scope?

- **Schema-level locals:** A separate `locals` field on the schema (option 1
  from the GitHub issue). The resource-level approach is more flexible since it
  can reference resource outputs.
- **Mutable variables:** Variable nodes are computed once per reconciliation cycle,
  not reassigned.

## Testing strategy

### Requirements

- envtest-based integration test environment (existing infrastructure)
- Unit tests for the graph builder changes (CEL parsing, DAG construction, type
  inference for variable nodes)

### Test plan

- **Unit tests:**
  - Variable node parsing and validation (constants, static vars, dynamic vars)
  - DAG dependency extraction from variable node CEL expressions
  - Type inference for variable node fields
  - Mutual exclusivity validation (variable vs template vs externalRef)
  - `readyWhen` evaluation on variable nodes
  - forEach expansion on variable nodes producing arrays
- **Integration tests:**
  - RGD with variable nodes reconciles successfully and generates correct CRD
  - Instance reconciliation evaluates variable node expressions in correct order
  - Variable nodes with `forEach` produce correct arrays
  - Variable nodes with `includeWhen` conditionally compute values
  - Downstream resources correctly receive variable node values
  - Circular dependency detection involving variable nodes
- **E2E tests:**
  - Full lifecycle: RGD with variable nodes -> instance creation -> resource
    creation with correct computed values -> update -> deletion

## Community discussions

### `spec.variables` as an alternative placement

An alternative to placing variable nodes in the `resources` array is a dedicated
`spec.variables` field. Variables would have the same capabilities as variable
nodes — DAG participation, resource output references, forEach, includeWhen —
but live in their own section:

```yaml
spec:
  schema: ...
  variables:
    - id: naming
      data:
        prefix: ${schema.spec.name + '-' + schema.spec.environment}
    - id: vpcInfo
      data:
        vpcId: ${vpc.status.vpcID}
  resources:
    - id: vpc
      template: ...
    - id: subnet
      template:
        spec:
          vpcID: ${vpcInfo.vpcId}
```

**Advantages:**

- Clear visual separation between "compute values" and "kubernetes resources".
- The `Resource` type stays focused on template vs externalRef.

**Tradeoffs:**

- Variable IDs and resource IDs must be unique across both arrays, but the
  uniqueness constraint isn't obvious from the YAML structure since the entries
  live in separate fields.

Both approaches are nearly equivalent in implementation cost and user
complexity. The `variable`-in-resources approach keeps all DAG participants in one
place; `spec.variables` gives cleaner type separation.

### Virtual resources as variable nodes

Instead of introducing a new `variable` field, variable nodes could be modeled
as **virtual resources** — resources that use the existing `template` field with
a reserved apiVersion (`virtual.kro.run/v1alpha1`) and kind (`Variable`). kro
would recognize this apiVersion/kind combination and treat the resource as a
variable node: it participates in the DAG but no Kubernetes resource is created.

```yaml
resources:
  - id: naming
    template:
      apiVersion: virtual.kro.run/v1alpha1
      kind: Variable
      data:
        prefix: ${schema.spec.name + '-' + schema.spec.environment}
        sanitizedModule: ${schema.spec.module.replace('.', '-')}

  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${naming.prefix + '-deploy'}
      spec: ...
```

**Advantages:**

- No new field — reuses the familiar `template` syntax users already know.
- **Explicit intent.** The `apiVersion` and `kind` make it unambiguous that this
  is a variable node. Unlike the "template without apiVersion" approach, a
  forgotten `apiVersion` on a real resource would still produce a validation
  error rather than silently becoming a variable.
- **Familiar structure.** Virtual resources follow the same shape as Kubernetes
  resources (apiVersion, kind, data/spec), reducing cognitive overhead for users
  already familiar with the Kubernetes resource model.
- **Clear validation.** Since kro unmarshals the template into a `Variable`
  struct, unknown fields produce natural error messages (e.g., "unknown field
  `metadata` in Variable node"). No special validation logic is needed beyond
  struct unmarshalling.
- **Extensible.** The `virtual.kro.run` API group can host other virtual resource
  kinds in the future without requiring new fields on the `Resource` type.

**Tradeoffs:**

- **Overloaded `template` semantics.** The `template` field would now mean two
  different things depending on the apiVersion: "create this Kubernetes resource"
  vs "compute these values internally." This dual meaning is subtler than a
  dedicated `variable` field.
- **Boilerplate.** Every variable node requires the `apiVersion` and `kind`
  lines, adding two lines of YAML per variable node compared to the dedicated
  `variable` field approach.
