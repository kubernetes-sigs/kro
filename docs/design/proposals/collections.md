# KREP-2: Declarative Resource Collections for KRO

## Summary

Each resource block in an RGD creates exactly one Kubernetes resource. This
means the number of resources must be known at RGD design time. But real
system and infrastructure needs are dynamic: the number of resources often depends
on user input, runtime conditions, or outputs from other resources. Pods,
Databases, Services - all may need to be created in variable quantities.

This proposal introduces `forEach`, enabling one resource block to create
multiple resources based on runtime data. Users get the power of loops while
KRO maintains bounded execution and predictable behavior.

## Motivation

KRO's current model requires each resource to be statically defined in the RGD.
Today, if you need 5 Pods you write 5 resource blocks. If you later need 10,
you modify the RGD. You cannot express "create N Pods where N equals
spec.workers".

Example: a batch processing app where users specify worker count via
`workers: 5`, or a pipeline with variable stages:
`stages: [{name: "extract", replicas: 3}, {name: "transform", replicas: 5}]`.

Users are forced to either hardcode resources or wrap KRO with templating
tools - defeating KRO's purpose.

## Proposal

### Overview

This KREP introduces the `forEach` field for resource definitions, enabling a
single resource block to create multiple resources based on collections. The
forEach field can iterate over:

- Arrays from the custom resource spec
- Ranges of numbers, characters, or other types
- Outputs from other resources in the RGD

A forEach block is a single DAG node that creates one Kubernetes resource per
collection item. Each resource is tracked independently with its own
reconciliation cycle, status, and error handling. As the collection changes
(items added or removed), KRO automatically creates or deletes resources to
match. The count is determined at runtime but execution remains bounded and
predictable.

### Core Concepts

**Collections as Declarative Relationships**

The `forEach` field establishes a declarative relationship between a collection
and a set of resources. When you write `forEach: [{name: ${schema.spec.workers}}]`, you're
declaring "there should exist one resource for each item in schema.spec.workers". KRO
maintains this relationship through continuous reconciliation, creating and
deleting resources as the collection changes.

**Bounded Execution**

While `forEach` enables dynamic resource creation, it maintains KRO's safety
guarantees. Collection size is determined when the `forEach` expression is
evaluated at runtime and is bounded by a hard maximum (currently 1000 items per
collection) to prevent resource exhaustion. Because evaluation is deterministic,
repeated reconciles produce the same set for the same inputs.

**DAG Model**

A resource block with the `forEach` field creates multiple resources at runtime
while remaining a single DAG node. Each item is treated as a distinct resource
for apply/update/delete with per-item readiness checks and error handling.

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

The key insight: in the RGD, `pods` is just one node. At runtime, it creates
resources based on the collection size. This preserves KRO's DAG model while
enabling dynamic resource creation.

**Dependencies**

Because a collection is a single DAG node, dependencies work at the collection
level: if resource B depends on collection A, B waits until *all* of A's
resources are ready (based on A's `readyWhen`). There is no per-item dependency
where B[i] depends only on A[i].

This behavior is intentional for this KREP - it keeps the DAG model simple and
predictable. However, [KREP-006: Propagation Control](https://github.com/kubernetes-sigs/kro/pull/861)
introduces orthogonal mechanisms (rate limiting, gradual rollout, `propagateWhen`)
that may interact with or extend this dependency model in the future.

## API Changes

### `forEach` Field

The `forEach` field is an array of named iterators. Each iterator is a map where
keys are variable names and values are CEL expressions that evaluate to arrays:

```yaml
forEach:
  - variableName: ${expression}
```

Example:

```yaml
kind: ResourceGraphDefinition
# ...
spec:
  resources:
    - id: workers
      forEach:
        - num: ${lists.range(3)}  # Creates 3 Pods
      template:
        apiVersion: v1
        kind: Pod
        metadata:
          name: ${'worker-' + string(num)}
        spec: { ... }
```

> **Why not maps?** Map iteration order is not deterministic in Go, which would
> lead to unpredictable resource ordering and reconciliation churn. If users
> have map data, they should convert it to a sorted array using CEL macros:
> `forEach: [{key: ${schema.spec.labels.map(k, k).sort()}}]`.

### Multiple Iterators (Cartesian Product)

When you specify multiple iterators, kro creates resources for every combination:

```yaml
- id: deployments
  forEach:
    - region: ${schema.spec.regions}  # ["us-east", "us-west"]
    - tier: ${schema.spec.tiers}      # ["web", "api"]
  template:
    kind: Deployment
    metadata:
      # Creates 4 deployments: us-east-web, us-east-api, us-west-web, us-west-api
      name: ${region + '-' + tier}
```

The first iterator is the outer loop, subsequent iterators are nested inside.
With the above example, resources are created in order: (us-east, web),
(us-east, api), (us-west, web), (us-west, api). If any iterator's collection
is empty, zero resources are created (standard cartesian product: 2 × 0 = 0).

### Iterator Variables

The variable name you specify becomes available in the template, holding the
current element:

```yaml
- id: workers
  forEach:
    - name: ${schema.spec.workers}  # ["alice", "bob", "charlie"]
  template:
    kind: Pod
    metadata:
      name: ${'worker-' + name}  # worker-alice, worker-bob, worker-charlie
```

To access the index, iterate over a range instead:

```yaml
- id: workers
  forEach:
    - i: ${lists.range(size(schema.spec.workers))}
  template:
    kind: Pod
    metadata:
      name: ${'worker-' + string(i)}  # worker-0, worker-1, worker-2
    spec:
      workerName: ${schema.spec.workers[i]}  # alice, bob, charlie
```

### `readyWhen` with Collections

For forEach resources, `readyWhen` uses the `each` keyword for per-item readiness checks.
The collection is ready when ALL items pass ALL readyWhen expressions (AND semantics):

```yaml
- id: workers
  forEach:
    - name: ${schema.spec.workers}
  readyWhen:
    - ${each.status.phase == 'Running'}  # Per-item check using `each`
  template:
    kind: Pod
    # ...
```

#### Why `each` instead of self-reference?

Single resources use self-reference: `${database.status.ready}`.

For collections, we use `each` instead of aggregates like `${workers.all(...)}` to align with
KREP-006 `propagateWhen`, where `each` provides per-item access and aggregates are passed explicitly.

### `includeWhen` with Collections

If `includeWhen` evaluates to false, the entire forEach is skipped (all or nothing):

```yaml
- id: backupJobs
  includeWhen:
    - ${schema.spec.backupsEnabled}
  forEach:
    - db: ${schema.spec.databases}
  template:
    kind: CronJob
    metadata:
      name: ${'backup-' + db.name}
    # ...
```

To exclude specific items, filter in the forEach expression itself:

```yaml
- id: backupJobs
  forEach:
    # Only iterate over databases that have backups enabled
    - db: ${schema.spec.databases.filter(d, d.backupEnabled)}
  template:
    kind: CronJob
    metadata:
      name: ${'backup-' + db.name}
    # ...
```

### Status Fields with Collections

Status can reference collections for dynamic aggregation:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
spec:
  schema:
    apiVersion: v1alpha1
    kind: WorkerPool
    spec:
      workers: "[]string"
    status:
      total: ${size(workerPods)}
      running: ${size(workerPods.filter(w, w.status.phase == 'Running'))}
      ready: ${workerPods.all(w, w.status.phase == 'Running')}
  resources:
    - id: workerPods
      forEach:
        - name: ${schema.spec.workers}
      template:
        kind: Pod
        metadata:
          name: ${schema.metadata.name + '-worker-' + name}
        # ...
```

### Referencing Collections in CEL

Collections can be referenced by ID from other resources. The collection exposes
all created resources as an array:

```yaml
kind: ResourceGraphDefinition
spec:
  resources:
    - id: microservices
      forEach:
        - svc: ${schema.spec.services}
      template:
        kind: Service
        metadata:
          name: ${svc.name}
        # ...

    - id: gateway
      template:
        kind: Ingress
        spec:
          rules:
            - host: ${schema.spec.domain}
              http:
                # collection resource can be referenced by its ID
                paths:
                  | # kro will do its magic making sure to replace this with the proper type.
                  ${microservices.map(s, {
                    'path': '/' + s.metadata.name,
                    'pathType': 'Prefix',
                    'backend': {
                      'service': {
                        'name': s.metadata.name,
                        'port': {'number': 80}
                      }
                    }
                  })}
```

Collection chaining - one collection can iterate over another:

```yaml
- id: databases
  forEach:
    - db: ${schema.spec.databases}
  template:
    apiVersion: db.example.com/v1
    kind: Database
    metadata:
      name: ${db.name}
    # ...

- id: backupJobs
  forEach:
    - db: ${databases}  # iterates over the databases collection
  template:
    kind: CronJob
    metadata:
      name: ${'backup-' + db.metadata.name}
    spec:
      schedule: "0 2 * * *"
      # ...
```

## Scope

### In Scope

- The `forEach` field for iterating over arrays
- Named iterator variables
- Multiple iterators for cartesian product
- Resource references in CEL to enable collection chaining
- Status field computations using collections
- Integration with existing KRO fields (`readyWhen`, `includeWhen`)
- Updates to resources when collection items change

### Out of Scope

The following are explicitly out of scope for this KREP and are addressed by
other proposals:

- **Enhanced externalRef for collection watching** - Discovering collections of
  external resources via label selectors is covered by
  [KREP-003: Decorators](https://github.com/kubernetes-sigs/kro/pull/738)
- **Controlled rollout strategies** - Rate limiting, time-based controls, and
  gradual propagation of changes through collections is covered by
  [KREP-006: Propagation Control](https://github.com/kubernetes-sigs/kro/pull/861)

The following are out of scope for this KREP but may be addressed in future:

- Per-item dependencies - where resource B[i] depends only on A[i] being ready,
  rather than waiting for all of collection A (see **Dependencies** in Core
  Concepts). This would require splitting collections into individual DAG nodes.

The following are out of scope and not planned:

- Nested forEach loops or recursive expansion
- Custom CEL functions beyond standard library
- Ensuring all resources in a forEach succeed or fail together (each resource is
  reconciled independently)
- Custom reconciliation ordering within a forEach
- Dealing with cross-namespace resource creation

### Non-Goals

- Turn KRO into a general purpose programming language
- Replace specialized controllers that need complex state machines
- Provide workflow orchestration with conditional branching
- Enable unbounded computation or recursion

## Design Details

### RGD Processing and Validation

KRO's existing validation framework extends to handle `forEach`:

- **Type checking**: Validates that forEach expressions resolve to arrays. If
  `schema.spec.workers` is `[]string`, KRO knows `forEach: [{name: ${schema.spec.workers}}]`
  iterates over strings and types `name` accordingly.
- **Scope validation**: Ensures iterator variables (e.g., `name`, `region`) are
  only accessible within the template block of their collection resource.
- **Iterator name validation**: Ensures iterator names are valid identifiers and
  don't conflict with reserved names like `schema`.

Dependency analysis requires no changes - collections are single DAG nodes (see
**Dependencies** in Core Concepts).

### Runtime Execution

During graph traversal, when the reconciler reaches a forEach node:

1. Evaluates the forEach CEL expression (same security constraints as all CEL)
2. Creates multiple resources - one per collection item
3. Processes all resources before moving to the next DAG node
4. Evaluates `readyWhen` across all collection resources

**Resource naming**: Each resource in a collection must have a unique name.
Templates should include the iterator variable in `metadata.name` to ensure
uniqueness within the collection. Additionally, names must be unique across
instances - best practice is to include `schema.metadata.name` (the instance
name) to avoid collisions when multiple instances of the same RGD exist.

**Resource tracking**: Each resource receives standard KRO ownership labels plus
collection labels like `kro.run/node-id`, `kro.run/collection-index`, and
`kro.run/collection-size` to identify collection membership and position.

**Reconciliation**: KRO uses ApplySet (KEP-3659) to manage resources during
normal reconciliation - handling create, update, and prune operations.
Collection labels are used during instance deletion to discover and clean up
resources. Errors on individual resources don't block others in the collection.

### Status Type Inference

KRO's type inference extends to collection expressions. When analyzing
`totalWorkers: ${size(workerPods)}`, KRO recognizes `workerPods` as a collection
and infers the expression returns an integer. This enables proper OpenAPI schema
generation even when collection size is only known at runtime.

## Backward Compatibility

The `forEach` field is purely additive. Existing RGDs continue to function
unchanged, no API version bump is required, and users can adopt `forEach`
incrementally.

## Future Work

### Multi-Key Maps for Iterator Grouping

The `forEach` field is an array (not a map) because array order is deterministic
while map key order is not. This lets users control the nesting order of
cartesian products - the first array element is the outermost loop:

```yaml
forEach:
  - region: ${schema.spec.regions}  # outermost
  - zone: ${schema.spec.zones}
  - tier: ${schema.spec.tiers}      # innermost
```

The current design requires each iterator to be a single-key map. A future
enhancement could allow multiple keys in a single map to group iterators where
the user doesn't care about relative ordering:

```yaml
forEach:
  - tier: ${schema.spec.tiers}           # level 0 (outermost)
  - region: ${schema.spec.regions}       # level 1 (kro determines order)
    zone: ${schema.spec.zones}           # level 1 (kro determines order)
```

In this model:
- Array position determines the nesting level (user controls coarse ordering)
- Within a multi-key map, kro hashes variable names to determine consistent
  sub-ordering (user doesn't care about fine ordering)

This would let users express "tier should be outermost, but I don't care whether
region or zone comes next" without verbose single-key syntax. The hash ensures
deterministic behavior while reducing user cognitive load for cases where
ordering doesn't matter.

## Alternatives Considered

The `forEach` syntax went through several iterations before arriving at the
current design. The key constraints were:

1. **Named variables**: Users need descriptive names for iterator variables, not
   implicit names like `item` or positional references.
2. **Cartesian product**: Multiple iterators should compose naturally without
   requiring users to manually flatten nested structures.
3. **Static analysis**: kro must be able to extract variable names and types at
   validation time, before any CEL expressions are evaluated.
4. **Kubernetes idioms**: The syntax should feel natural to Kubernetes users and
   work within CRD/OpenAPI constraints.

The following alternatives were considered.

### Simple String Expression

```yaml
- id: workers
  forEach: ${schema.spec.workers}
```

A plain CEL expression with implicit variable name (e.g., always `item`).

**Trade-offs**: No way to name the iterator variable, making templates harder
to read. Cartesian products would require complex nested CEL:

```yaml
forEach: >
  ${schema.spec.regions.map(r,
    schema.spec.tiers.map(t,
      {'region': r, 'tier': t}
    )
  ).flatten()}
```

This pushes complexity onto users and makes simple cases verbose.

### Explicit Struct with var/in Keywords

```yaml
forEach:
  - var: region
    in: ${schema.spec.regions}
  - loop: tier
    over: ${schema.spec.tiers}
```

Explicit fields for variable name and expression.

**Trade-offs**: More verbose than necessary. The map key syntax (`region: ${expr}`)
is more concise and equally clear. The explicit struct adds no expressiveness
while requiring more typing.

### CEL Iterator Function

```yaml
forEach:
  - ${iterator('region', schema.spec.regions)}
  - ${iterator('tier', schema.spec.tiers)}
```

A special CEL function that returns iterator configuration.

**Trade-offs**: Requires AST analysis to extract variable names from CEL
expressions. The expression could use other variables in scope, making static
analysis difficult. Blurs the line between configuration and computation.

### CEL Array of Iterator Tuples

```yaml
forEach: >
  ${[
    {'region': schema.spec.regions},
    {'tier': schema.spec.tiers}
  ]}
```

A single CEL expression returning an array of maps, where each map has the
variable name as key and the iteration array as value.

**Trade-offs**: Similar to the CEL iterator function approach - variable names
are embedded in CEL strings, preventing static analysis at validation time.

### Dynamic Field Names (forEach\<var\>)

```yaml
forEach<region>: ${schema.spec.regions}
forEach<tier>: ${schema.spec.tiers}
```

Variable name embedded in the field name itself.

**Trade-offs**:
- CRD/OpenAPI cannot express dynamic field names - no schema validation
- No IDE support (autocomplete, hover docs, error highlighting)
- Requires custom unmarshaling logic
- Not idiomatic Kubernetes - no other K8s API uses this pattern
- YAML parsers see these as separate unrelated keys
- Harder to iterate programmatically (must parse field names)

### Iteration Context Variables (.item, .index, .key, .value)

An earlier design provided structured access to iteration state:

```yaml
forEach:
  - name: ${schema.spec.workers}
template:
  metadata:
    name: ${'worker-' + name.item}
    labels:
      index: ${string(name.index)}
```

**Why dropped**:
- Added complexity for marginal benefit
- Index access is rare - when needed, iterate over `lists.range(size(array))`
- Map iteration was dropped entirely (non-deterministic ordering)
- Direct variable access (`${name}` vs `${name.item}`) is simpler and covers
  the common case
- Reduces cognitive overhead - the variable IS the item, not a wrapper
