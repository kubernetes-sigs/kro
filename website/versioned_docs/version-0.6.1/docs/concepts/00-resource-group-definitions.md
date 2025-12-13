---
sidebar_position: 1
---

# ResourceGraphDefinitions

ResourceGraphDefinitions are the fundamental building blocks in **kro**. They provide a
way to define, organize, and manage sets of related Kubernetes resources as a
single, reusable unit.

## What is a ResourceGraphDefinition?

A **ResourceGraphDefinition** is a custom resource that lets you create new Kubernetes
APIs for deploying multiple resources together. It acts as a blueprint,
defining:

- What users can configure (schema)
- What resources to create (resources)
- How resources reference each other (dependencies)
- When resources should be included (conditions)
- What status to expose (status)

When you create a **ResourceGraphDefinition**, kro generates a new API (a.k.a Custom
Resource Definition) in your cluster that others can use to deploy resources in a
consistent, controlled way.

## Anatomy of a ResourceGraphDefinition

A ResourceGraphDefinition, like any Kubernetes resource, consists of three main parts:

1. **Metadata**: name, labels, etc.
2. **Spec**: Defines the structure and properties of the ResourceGraphDefinition
3. **Status**: Reflects the current state of the ResourceGraphDefinition

The `spec` section of a ResourceGraphDefinition contains two main components:

- **Schema**: Defines what an instance of your API looks like:
  - What users can configure during creation and update
  - What status information they can view
  - Default values and validation rules
- **Resources**: Specifies the Kubernetes resources to create:
  - Resource templates
  - Dependencies between resources
  - Conditions for inclusion
  - Readiness criteria
  - [External References](#using-externalref-to-reference-objects-outside-the-resourcegraphdefinition)

This structure translates to YAML as follows:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-resourcegraphdefinition # Metadata section
spec:
  schema: # Define your API
    apiVersion: v1alpha1 # API version
    kind: MyAPI # API kind
    spec: {} # fields users can configure
    status: {} # fields kro will populate

  # Define the resources kro will manage
  resources:
    - id: resource1
      # declare your resources along with default values and variables
      template: {}
```

Let's look at each component in detail...

## Understanding the Schema

The schema section defines your new API's structure. It determines:

- What fields users can configure when creating instances
- What status information they can view
- Type validation and default values

Here's an example schema:

```yaml
schema:
  apiVersion: v1alpha1
  kind: WebApplication # This becomes your new API type
  spec:
    # Fields users can configure using a simple, straightforward syntax
    name: string
    image: string | default="nginx"
    replicas: integer | default=3
    ingress:
      enabled: boolean | default=false

  status:
    # Fields kro will populate automatically from your resources
    # Types are inferred from these CEL expressions
    availableReplicas: ${deployment.status.availableReplicas}
    conditions: ${deployment.status.conditions}

  validation:
    # Validating admission policies added to the new API type's CRD
    - expression: "${ self.image == 'nginx' || !self.ingress.enabled }"
      message: "Only nginx based applications can have ingress enabled"

  additionalPrinterColumns:
    # Printer columns shown for the created custom resource
    - jsonPath: .status.availableReplicas
      name: Available replicas
      type: integer
    - jsonPath: .spec.image
      name: Image
      type: string
```

**kro** follows a different approach for defining your API schema and shapes. It
leverages a human-friendly and readable syntax that is OpenAPI spec compatible.
No need to write complex OpenAPI schemas - just define your fields and types in
a straightforward way. For the complete specification of this format, check out
the [Simple Schema specification](./10-simple-schema.md). Status fields use CEL
expressions to reference fields from resources defined in your ResourceGraphDefinition.
kro automatically:

- Infers the correct types from your expressions
- Validates that referenced resources exist
- Updates these fields as your resources change

## Processing

When you create a **ResourceGraphDefinition**, kro processes it in several steps to ensure
correctness and set up the necessary components:

1. **Validation**: kro validates your **ResourceGraphDefinition** to ensure it's well
   formed and follows the correct syntax, maximizing the chances of successful
   deployment, and catching as many errors as possible early on. It:

   - Validates your schema definition follows the simple schema format
   - Ensures all resource templates are valid Kubernetes manifests
   - Checks that referenced values exist and are of the correct type
   - Confirms resource dependencies form a valid Directed Acyclic Graph(DAG)
     without cycles
   - Validates all CEL expressions in status fields and conditions using CEL's native type system
     - Validates field references exist in the actual resource schemas
     - Ensures expressions return types compatible with their target fields
     - Validates that CEL functions called in expressions exist and are used correctly
     - Checks expression correctness and type compatibility statically without executing expressions

2. **API Generation**: kro generates and registers a new CRD in your cluster
   based on your schema. For example, if your **ResourceGraphDefinition** defines a
   `WebApplication` API, kro creates a CRD that:

   - Provides API validation based on your schema definition
   - Automatically applies default values you've defined
   - Makes status information available to users and other systems
   - Integrates seamlessly with kubectl and other Kubernetes tools

3. **Controller Configuration**: kro configures itself to watch for instances of
   your new API and their managed resources:

   - Creates all required resources following the dependency order
   - Manages references and value passing between resources
   - Handles the complete lifecycle for create, update, and delete operations
   - Keeps status information up to date based on actual resource states
   - Automatically detects and reconciles drift in managed resources
   - Triggers reconciliation when any managed resource changes

For instance, when you create a `WebApplication` ResourceGraphDefinition, kro generates
the `webapplications.kro.run` CRD. When users create instances of this API, kro
manages all the underlying resources (Deployments, Services, Custom Resources,
etc.) automatically.

kro continuously monitors your ResourceGraphDefinition for changes, updating the API and
its behavior accordingly.

## Instance Example

After the **ResourceGraphDefinition** is validated and registered in the cluster, users
can create instances of it. Here's an example of how an instance for the
`WebApplication` might look:

```yaml title="my-web-app-instance.yaml"
apiVersion: kro.run/v1alpha1
kind: WebApplication
metadata:
  name: my-web-app
spec:
  appName: awesome-app
  image: nginx:latest
  replicas: 3
```

## More about Resources

Users can specify more controls in resources in `.spec.resources[]` 

```yaml
spec:
  resources:
    - id: my-resource
      template || externalRef: {} # users can either template resources or reference objects outside the graph
      readyWhen:
      # users can specify CEL expressions to determine when a resource is ready
      - ${deployment.status.conditions.exists(x, x.type == 'Available' && x.status == "True")}
      includeWhen:
      # users can specify CEL expressions to determine when a resource should be included in the graph
      - ${schema.spec.value.enabled}
```

### Using `externalRef` to reference Objects outside the ResourceGraphDefinition.

Users can specify if the object is something that is created out-of-band and needs to be referenced in the RGD.
An external reference could be specified like this:
```
resources:
   id: projectConfig
   externalRef:
     apiVersion: corp.platform.com/v1
     kind: Project
     metadata:
       name: default-project
       namespace: # optional, if empty uses instance namespace
```

As part of processing the Resource Graph, the instance reconciler waits for the `externalRef` object to be present and reads the object from the cluster as a node in the graph. Subsequent resources can use data from this node.


### Using Conditional CEL Expressions (`?`)

KRO can make use of CEL Expressions (see [this proposal for details](https://github.com/google/cel-spec/wiki/proposal-246) or look at the [CEL Implementation Reference](https://pkg.go.dev/github.com/google/cel-go/cel#hdr-Syntax_Changes-OptionalTypes)) to define optional runtime conditions for resources based on the conditional operator `?`.

This allows you to optionally define values that have no predefined schema or are not hard dependencies in the Graph.

#### Using `?` for referencing schema-less objects like `ConfigMap` or `Secret`

You can use the `optional` operator to reference objects that do not have a predefined schema in the ResourceGraphDefinition. This is useful for referencing objects that may or may not exist at runtime.

> :warning: `?` removes the ability of KRO to introspect the schema of the referenced object. Thus, it cannot wait for fields after the `?` to be present. It is recommended to use conditional expressions only for objects that are not critical to the ResourceGraphDefinition's operation or when the schema cannot be known at design time.

A config map can be referenced like this:

```yaml title="config-map.yaml"
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  VALUE: "foobar"
```

```yaml title="external reference in ResourceGraphDefinition"
- id: external
  externalRef:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: demo
      namespace: default
```

With this reference, you can access the data in your schema:

```text title="CEL Expression"
${external.data.?VALUE}
```

> :warning: KRO will only wait for the external reference to be present in the cluster, but it will not validate the schema of the referenced config. If the config map does not have the `VALUE` field, the expression will evaluate to `null` and might result in unexpected behavior in your application if not handled properly.


_For a more detailed example, see the [Optional Values & External References](../../examples/basic/optionals.md) documentation._

## Status Reporting

The `status` section of a `ResourceGraphDefinition` provides information about the state of the graph and it's generated `CustomResourceDefinition` and controller.

`status` includes a stable `Ready` condition (as well as a set of technical `status.conditions` that provide more detailed information about the state of the graph and its resources).

:::info

When the `Ready` condition `status` is `True`, it indicates that the ResourceGraphDefinition is valid and you can use it to create [instances](./15-instances.md).

:::

:::warning

Try to only rely on the `Ready` condition, as other condition types may change frequently and are more technical in nature, can change their API over time and are generally more indicative of KRO's internal state.

:::

Additionally, the ResourceGraphDefinition contains a `topologicalOrder` field that provides a list of resources in the order they should be processed. This is useful for understanding the dependencies between resources and their apply order.

Generally a status in `ResourceGraphDefinition` may look like

```yaml
status:
  conditions:
    - lastTransitionTime: "2025-08-06T17:26:41Z"
      message: resource graph and schema are valid
      observedGeneration: 1
      reason: Valid
      status: "True"
      type: ResourceGraphAccepted
    - lastTransitionTime: "2025-08-06T17:26:41Z"
      message: kind DeploymentService has been accepted and ready
      observedGeneration: 1
      reason: Ready
      status: "True"
      type: KindReady
    - lastTransitionTime: "2025-08-06T17:26:41Z"
      message: controller is running
      observedGeneration: 1
      reason: Running
      status: "True"
      type: ControllerReady
    - lastTransitionTime: "2025-08-06T17:26:41Z"
      message: ""
      observedGeneration: 1
      reason: Ready
      status: "True"
      type: Ready
  state: Active
  topologicalOrder:
    - configmap
    - deployment
```
