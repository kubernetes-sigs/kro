# KREP-2: Declarative Resource Collections for KRO

## Summary

KRO (Kubernetes Resource Orchestrator) enables users to define custom Kubernetes
APIs that create and manage multiple resources as a single unit. Users write
ResourceGraphDefinitions (RGDs) that specify a directed acyclic graph (DAG) of
resources and their relationships. KRO's engine uses CEL (Common Expression
Language) for dynamic field computation while maintaining strict security
properties: bounded execution time, runtime sandboxing, and predictable resource
generation.

Currently, KRO has a fundamental limitation: each resource block in an RGD
creates exactly one resource. The number of resources must be known at design
time and explicitly defined. This creates a gap between KRO's declarative model
and real-world infrastructure needs, where the number of resources often depends
on dynamic factors discovered at runtime - how many availability zones are
healthy, how many tenants exist, how many namespaces match certain criteria.

Users need the ability to provision resources dynamically based on runtime state
without sacrificing KRO's core properties: declarative configuration,
predictable execution, and security guarantees. They need the expressive power
of loops without the unbounded execution and security risks of traditional
templating languages.

This proposal introduces declarative resource collections. With collections, one
resource block can create multiple resources based on data discovered at
runtime. Collections give users the power of for loops while keeping KRO's
safety guarantees - execution time is still bounded and behavior is still
predictable.

## Motivation

### Problem Statement

Kubernetes environments are inherently dynamic, and the number of resources
needed often depends on runtime configuration rather than design-time decisions.
KRO's current model requires users to statically define each resource block in
their ResourceGraphDefinition, creating a fundamental mismatch with real-world
needs.

The core issue is simple: each resource block in an RGD creates exactly one
Kubernetes resource. If you need 5 Pods, you write 5 resource blocks. If you
later need 10, you must modify the RGD to add 5 more blocks.

Consider a batch processing application where users specify how many worker Pods
to run through their custom resource: `workers: 5`. Each worker needs its own
Pod with specific environment variables and configurations based on its index.
With KRO today, you cannot express "create N Pods where N equals spec.workers".
The same limitation appears in data processing pipelines where each stage might
need a different number of resources:
`stages: [{name: "extract", replicas: 3}, {name: "transform", replicas: 5}, {name: "load", replicas: 2}]`.

This forces users to either hardcode a fixed number of resources and live with
the inflexibility, or wrap KRO with templating tools to generate different
RGDs - defeating KRO's purpose of moving beyond templating to declarative APIs.

What users really need is the ability to say "for each item in this array,
create a resource" or "create N resources where N comes from the spec" - dynamic
resource creation based on runtime values while maintaining KRO's declarative
approach.

## Proposal

### Overview

This KREP introduces the `forEach` field for resource definitions, enabling a
single resource block to create multiple resources based on collections. The
forEach field can iterate over:

- Arrays from the custom resource spec
- Ranges of numbers, characters, or other types
- Collections of discovered resources (via externalRef)
- Outputs from other resources in the RGD

Each iteration creates a separate node in KRO's DAG, maintaining clear ownership
and reconciliation semantics. The number of DAG nodes is determined at runtime
but execution remains bounded and predictable.

### Core Concepts

**Collections as Declarative Relationships**

The `forEach` field establishes a declarative relationship between a collection
and a set of resources. When you write `forEach: ${spec.workers}`, you're
declaring "there should exist one resource for each item in spec.workers". KRO
maintains this relationship through continuous reconciliation, creating and
deleting resources as the collection changes.

**Bounded Execution**

While `forEach` enables dynamic resource creation, it maintains KRO's security
guarantees. The size of collections is bounded by Kubernetes API limits, CEL
expressions within `forEach` are subject to normal CEL cost limits, and the
total number of resources created is visible and predictable before execution
begins.

**DAG Expansion**

A resource block with the `forEach` field expands into multiple nodes in KRO's
execution DAG at runtime. Each expanded node is treated as a distinct resource
with its own reconciliation cycle, status tracking, and error handling.
Dependencies between resources are preserved - if resource B depends on resource
A which has `forEach`, then B depends on all expanded instances of A.

before Collections and after collections.

```
       Current KRO                                  WITH COLLECTIONS

     Application CR                                  Application CR
           │                                               │
           ▼                                               ▼
 ┌─────────────────┐                             ┌─────────────────┐
 │      RGD        │                             │      RGD        │
 └─────────────────┘                             └─────────────────┘
           │                                               │
 ┌─────────┴─────────────────┐                   ┌─────────┴─────────┐
 │         │         │       │                   │                   │
 ▼         ▼         ▼       ▼                   ▼                   ▼
┌────┐  ┌─────┐  ┌─────┐  ┌─────┐          ┌──────────┐     ┌─────────────┐
│ CM │  │Pod-1│  │Pod-2│  │Pod-3│          │ConfigMap │     │pods(forEach)│
└────┘  └─────┘  └─────┘  └─────┘          └──────────┘     └──────┬──────┘
                                                                   │
                                                                   │ Expands at
                                                                   │ runtime
                                                            ┌──────┼──────┐
                                                            │      │      │
                                                            ▼      ▼      ▼
                                                        ┌─────┐ ┌─────┐ ┌─────┐
                                                        │Pod-0│ │Pod-1│ │Pod-2│
                                                        └─────┘ └─────┘ └─────┘

Must define each pod in RGD                    Single forEach node → N resources
```

The key insight: in the RGD, `pods` is just one node. At runtime, it expands
based on the collection size. This preserves KRO's DAG model while enabling
dynamic resource creation.

## API Changes

### New Fields

This proposal introduces the following new field to the resource definition in
the `ResourceGraphDefinitions`:

#### `forEach` Field

The `forEach` field enables a single resource definition to create multiple
resource instances based on a collection. Semantically, `forEach: ${expression}`
is equivalent to "for each item in expression, manage a resource". It's a
declarative way to express iteration - you're asking KRO to iterate over a
collection and create a resource for each element.

```yaml
forEach: ${expression} # CEL expression that evaluates to something iterable
```

The expression must evaluate to an iterable collection. When forEach is
specified on a resource, KRO expands that single resource definition into
multiple resources at runtime - one for each item in the collection. Each
resource instance is tracked independently with its own reconciliation state.

Example in an RGD:

```yaml
kind: ResourceGraphDefinition
# ...
spec:
  resources:
    # workers node in the DAG
    - id: workers
      forEach: ${range(1, 4)} # Creates 3 Pods
      template:
        apiVersion: v1
        kind: Pod
        metadata: { ... }
        spec: { ... }
```

The `forEach` expression must evaluate to one of the following types:

- **Arrays**: Any list value - `${[0, 1, 2]}`, `${spec.regions}`,
  `${["us-east", "us-west"]}`, or `${pod.spec.containers}`
- **Maps**: key-value pairs where iteration occurs over entries -
  `${spec.labels}` or `${{"env": "prod", "tier": "web"}}`

The expression can include CEL transformations or macros:
`${spec.workers.filter(w, w.enabled)}` or `${spec.zones.map(z, z + "-a")}`.

As the collection changes (items added/removed), KRO automatically reconciles
the resources - creating new ones for added items and deleting resources for
removed items. Each resource maintains a stable identity based on its position
or key in the collection.

### Iteration Context Variables

When a resource uses forEach, special variables become available within CEL
expressions in the template:

- `each.item` - The current element being iterated over
- `each.index` - The index (for arrays)
- `each.key` - The key (for maps)
- `each.value` - The value (for maps)
- `each.length` - Total size of the collection

Example usage:

```yaml
- id: workers
  forEach: ${spec.workers} # ["alice", "bob", "charlie"]
  template:
    kind: Pod
    metadata:
      name: ${'worker-' + each.item} # worker-alice, worker-bob, worker-charlie
      labels:
        worker-name: ${each.item} # alice, bob, charlie
        worker-index: ${string(each.index)} # "0", "1", "2"
        total-workers: ${string(each.length)} # "3"
```

### `readyWhen` with Collections

While `readyWhen` is an existing field in KRO, it gains new capabilities with
collections. For resources using forEach, readyWhen can now define conditions
across all instances in the collection:

```yaml
- id: workers
  forEach: ${spec.workers}
  # 'workers' node is ready when ALL worker pods are running
  readyWhen: ${workers.all(w, w.status.phase == 'Running')}
  template:
    kind: Pod
    # ...
```

The resource collection is considered ready only when ALL worker pods are
running. You can also use:

- `workers.all(w, condition)` - Ready when all instances match
- `workers.exists(w, condition)` - Ready when at least one instance exists
- `workers.exists_one(w, condition)` - Ready when exactly one instance matches

An aggregate condition can be defined using the following CEL functions:
- `workers.filter(w, condition)` - Filter instances based on a condition
- `workers.map(w, expression)` - Transform instances into another form

### includeWhen with Collections

The resource-level `includeWhen` determines whether any instances should be
created at all. If `includeWhen` evaluates to false, the entire forEach is
skipped and no resources are created.

```yaml
- id: backupJobs
  includeWhen: ${size(spec.databases) > 0 && spec.backupEnabled}
  forEach: ${spec.databases}
  template:
    kind: CronJob
    # ...
```

This is different from filtering within forEach - it's an all-or-nothing
decision for the entire collection.

### excluding specific instances within a collection

To exclude specific instances from being created within a collection, users can
leverage CEL functions like `filter` or `map` directly in the `forEach`
expression. For example i want to create N configMaps and exclude odd numbers:

```yaml
- id: configMaps
  forEach: ${range(0, 10).filter(x, x % 2 == 0)} # Only even numbers
  template:
    kind: ConfigMap
    metadata:
      name: ${'config-' + string(each.item)} # config-0, config-2, config-4, ...
```

### simple schema enhancements

The status section of the schema can now include CEL expressions that reference
resource collections. This enables dynamic status computation based on the state
of all resources created by forEach:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
spec:
  schema:
    apiVersion: v1alpha1
    kind: Application
    spec:
      workers: []string
    status:
      # Reference collections directly in status
      totalWorkers: ${size(workerPods.items)}
      runningWorkers: ${size(workerPods.items.filter(w, w.status.phase == 'Running'))}
      allWorkersReady: ${workerPods.all(w, w.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True'))}
      workerNames: ${workerPods.items.map(w, w.metadata.name).join(',')}
  resources:
    - id: workerPods
      forEach: ${spec.workers}
      template:
        kind: Pod
        # ...
```

Status fields are continuously updated as the collection changes, providing
real-time observability into the state of all resources managed by `forEach`.

### resource references in CEL

Collection resources can be referenced by other resources using their "ID". For
example, you have a collection of services and you want to create an ingress
that routes to all of them:

```yaml
kind: ResourceGraphDefinition
# ...
spec:
  resources:
    - id: microservices
      forEach: ${spec.services}
      template:
        kind: Service
        # ...

    - id: gateway
      template:
        kind: Ingress
        # ...
        spec:
          rules:
            - host: ${spec.domain}
              http:
                # collection resource can be referenced by its ID
                paths:
                  | # kro will do it's magic making sure to replace this with the proper type.
                  ${microservices.map(svc, {
                    'path': '/' + svc.metadata.labels.app,
                    'pathType': 'Prefix',
                    'backend': {
                      'service': {
                        'name': svc.metadata.name,
                        'port': {'number': 80}
                      }
                    }
                  })}
```

or you might want to reference a collection to create another collection
(collection piping):

```yaml
kind: ResourceGraphDefinition
# ...
spec:
  resources:
    # Create a collection of pods for each database
    - id: databases
      forEach: ${spec.databases}
      template:
        kind: Pod
        # ...

    - id: databaseBackups
      # Create a backup job for each database
      forEach: ${databases}
      template:
        kind: CronJob
        metadata:
          name: ${each.item.metadata.name + '-backup'}
        spec:
          schedule: "0 2 * * *"
          jobTemplate:
            # ...
```

### External references with collections

Today, KRO supports `externalRef` to discover resources outside the RGD.
external references does point to one and unique resource, discovered by name
and namespace.

The `externalRef` field can be used in conjunction with collections to
dynamically discover resources and create new resources based on the discovered
set. For example, you might want to create a Pod for each Namespace that matches
a certain label:

#### Discover resources based on labels

```yaml
- id: targetNamespaces
  externalRef:
    apiVersion: v1
    kind: Namespace
    selector:
      matchLabels:
        team: frontend
```

#### Discover resources using forEach semantics

```yaml
- id: namespacePods
  forEach: ${targetNamespaces} # Iterate over discovered namespaces
  template:
    kind: Pod
    metadata:
      name: ${'pod-in-' + each.item.metadata.name}
      namespace: ${each.item.metadata.name}
```

## Scope

This proposal focuses on enabling dynamic resource creation through the
`forEach` field and essential supporting features.

### In Scope

- The `forEach` field for iterating over arrays, maps, and resource collections
- Iteration context variables (`each.item`, `each.index`, `each.key`,
  `each.value`, `each.length`)
- Resource references in CEL to enable collection chaining
- Status field computations using collections
- Integration with existing KRO fields (`readyWhen`, `includeWhen`)
- Updates to resources when collection items change
- Enhanced externalRef to watch collections of resources (via slectors)
- Combining `forEach` with externalRef for dynamic external resource discovery

### Out of Scope

- Nested forEach loops or recursive expansion
- Custom CEL functions beyond standard library
- Ensuring all resources in a forEach succeed or fail together (each resource is
  reconciled independently)
- Custom reconciliation ordering within a forEach
- Controlled rollout strategies (e.g. rolling updates, canary deployments) -
  these may be added in future iterations
- Dealing with cross-namespace resource creation

### Non-Goals

This proposal is not attempting to:

- Turn KRO into a general purpose programming language
- Replace specialized controllers that need complex state machines
- Provide workflow orchestration with conditional branching
- Enable unbounded computation or recursion

The goal is narrowly focused: enable declarative management of N resources where
N is determined at runtime, while maintaining KRO's security and predictability
guarantees.

## Design details

### RGD processing and static analysis

When an `ResourceGraphDefinition` is submitted to the cluster, KRO already performs
extensive validation including type checking, in memory dry runs, and CEL
expression validation among other things. This existing validation framework is
extended to handle `forEach field while maintaining the same safety guarantees.

The type checker is enhanced to validate forEach expressions. When KRO
encounters a forEach field, it verifies that the CEL expression resolves to an
iterable type - either an array or a map. The type information flows from the
schema definition, so if `spec.workers` is defined as `[]string`, KRO knows that
`forEach: ${spec.workers}` will iterate over strings. For each iteration type,
KRO validates the availability and types of iteration variables. Arrays provide
`each.item` (typed according to the array element type), `each.index` (always
int), and `each.length` (always int). Maps provide `each.key` and `each.value`
with types derived from the map definition.

CEL validation extends to ensure iteration variables are scoped correctly. The
validator checks that `each.*` variables are only accessible within the template
block of the same collection resource. It also validates that these variables are
used with type-appropriate operations - for instance, `each.index` can be used
in arithmetic operations, while `each.item` must be used according to its actual
type. When forEach references other resources like `forEach: ${databases}`, KRO
verifies that `databases` is a valid resource ID and will indeed produce a
collection at runtime.

The existing dry-run validation continues to work by using synthetic values for
iteration variables. For each forEach block, KRO generates representative test
values based on the types and runs them through the template to ensure valid
Kubernetes resources are produced. This catches issues like invalid resource
specifications or missing required fields before any instances are created.

> NOTE(a-hilaly): The dry run validation is currently limiting and often too
> strict, i do have plans to completely get rid of it in favor of better type
> checking and CEL validation.

Importantly, the dependency analysis system requires no changes. Collections are
treated as single nodes in the DAG regardless of how many resources they might
create at runtime. If resource B depends on a collection resource A, the
dependency is at the collection level - B waits for all of A's resources to be
ready based on A's readyWhen condition. This maintains KRO's existing DAG
validation and cycle detection without modification.

### Instance reconciliation and runtime execution

When KRO reconciles an instance, it builds a runtime representation of the
resource graph from the RGD. This graph is then traversed in topological order
using depth-first search (DFS), ensuring dependencies are respected. Resources
are reconciled one by one - if a resource isn't ready or its dependencies aren't
met, KRO requeues the instance for later reconciliation. This existing
reconciliation model extends naturally to handle forEach collections.

During graph construction, forEach nodes are initially represented as single
vertices, just like regular resources. When the reconciler reaches a forEach
node during DFS traversal, it evaluates the forEach expression within KRO's CEL
sandbox. This evaluation happens with the same security constraints as all CEL
expressions - bounded execution time and budget, and no external access. The
expression receives the current instance's spec, status information from
previously reconciled resources in the graph, and any data from external
references.

Once the collection is evaluated, the forEach node expands into multiple
resource operations - one for each item in the collection. However, from the
graph traversal perspective, the forEach still acts as a single node. The
reconciler processes all resources in the collection before moving to the next
node in the topological order. This preserves dependency semantics: if resource
B depends on a collection resource A, B waits until all of A's collection
resources are processed.

KRO tracks resources created by collections using its existing ownership system,
extended with collection specific labels. Each resource receives the standard
KRO ownership labels plus `kro.run/collection-key` which identifies which item
in the collection this resource represents. This labeling scheme, orthogonal to
the ApplySet work, enables precise tracking of collection resources across
reconciliation cycles.

> NOTE(a-hilaly): Depending on how the ApplySet work shapes up, we might be able
> to leverage it for collections as well - maybe even simplify the
> implementation of collections. I'm thinking that all the garbage collection
> logic can be deferred to ApplySet.

When reconciling a forEach node, KRO first queries for all existing resources
with the matching `kro.run/resource-id` label. This comprehensive query catches
any orphaned resources from previous reconciliations. KRO then compares the
current collection against these existing resources. New items in the collection
trigger resource creation, removed items trigger deletion, and existing items
trigger updates. This "three way" reconciliation ensures the cluster state
matches the desired state without resource leaks.

If any resource in the forEach fails to reconcile, the error is recorded
(propagated back to instance conditions) but doesn't block other resources in
the collection. Once all collection resources are attempted, the forEach node's
readyWhen condition is evaluated. If the condition isn't met (e.g., "all pods
running"), the entire instance is requeued for later reconciliation, following
KRO's standard retry behavior. This maintains the invariant that graph traversal
only proceeds when dependencies are satisfied, whether those dependencies are
single resources or entire collections.

### Status computation with collections

KRO generates OpenAPI schemas for CRDs by analyzing CEL expressions at RGD
submission time. For status fields, KRO infers types from these expressions to
create a properly structured CRD rather than using unstructured fields. This
type inference system now extends to handle forEach collections.

When KRO encounters a status expression like
`totalWorkers: ${size(workerPods)}`, it recognizes that `workerPods` references
a collection resource and infers the expression will return an integer. Similarly,
`readyWorkers: ${workerPods.filter(w, w.status.phase == 'Running').size()}` is
analyzed to determine it produces an integer type. This static analysis happens
during RGD processing, allowing KRO to generate the correct OpenAPI schema for
the CRD with properly typed status fields.

The type inference combines knowledge of CEL built-in functions with awareness
of forEach collections. KRO knows that `size()` returns int, `filter()` returns
a collection, and `all()` returns bool. This enables generation of a structured
status schema even when the actual collection size is only known at runtime.
Users get properly typed status fields in their CR instances, with the values
computed dynamically during reconciliation.

## Backward Compatibility

This proposal maintains full backward compatibility with existing KRO
installations and RGDs. The forEach field is purely additive - existing RGDs
without forEach continue to function exactly as before. No API version bump is
required as this enhancement only adds new optional fields without modifying any
existing behavior.

Current KRO users can upgrade to a version with forEach support without any
migration steps. Their existing RGDs will continue to work unchanged, and they
can selectively adopt forEach in new RGDs or add it to existing ones when
needed. The generated CRDs maintain the same structure for resources without
forEach, ensuring that existing CR instances remain valid.

The CEL evaluation environment is extended rather than modified. Existing CEL
expressions in RGDs continue to work as before, while new expressions gain
access to forEach-related functionality. Status computations, readyWhen
conditions, and includeWhen expressions maintain their current semantics when
not referencing collections.

From an operational perspective, the reconciliation loop, dependency resolution,
and error handling maintain their current behavior for non-collection resources.
The forEach expansion happens within the existing reconciliation framework,
appearing as standard resource operations to the rest of the system. This
ensures that monitoring, logging, and debugging tools continue to work without
modification.

---
design: add declarative resource collections support

Introduces declarative resource collections to KRO, enabling dynamic 
creation of multiple resources based on runtime data. This addresses the 
fundamental limitation where each RGD resource block could only create a 
single Kubernetes resource, requiring static definition of all resources 
at design time.

This proposal adds the `forEach` field to RGD `spec.resources`, allowing 
users to create N resources where N is determined at runtime from arrays, 
maps, or discovered resources. The design maintains KRO's declarative 
model and security guarantees while solving real world use cases like 
creating resources per tenant, per namespace, or per availability zone.

While this proposal also covers related enhancements such as collection 
support in `externalRef`, status field computations with collections, and 
various integration points, the implementation will be split into phases. 
The initial implementation will focus on core `forEach` functionality with 
basic collections, followed by incremental additions of advanced features 
like collection chaining and external resource discovery.

The enhancement is fully backward compatible, requiring no API version 
changes or migration. Existing RGDs continue to work unchanged, and users 
can adopt collections incrementally as needed. All changes are additive 
and maintain KRO's existing reconciliation semantics.