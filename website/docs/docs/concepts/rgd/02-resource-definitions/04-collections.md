---
sidebar_position: 4
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Collections

By default, each resource definition (in `spec.resources`) creates exactly one
Kubernetes resource. This means the number of resources is fixed at design
time - if you need 5 worker Pods, you write 5 resource definitions.

Collections let you declaratively manage multiple similar resources from a
single definition. This is useful when the number of resources depends on
runtime data like availability zones, tenants, or worker counts.

kro provides the `forEach` field to turn any resource into a collection. The
field takes one or more iterators, and kro creates one resource for each element
(or combination of elements), keeping them in sync as the collection changes.

## Basic Example

This RGD uses `forEach` to create multiple Pods from a single resource entry:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: worker-pool
spec:
  schema:
    apiVersion: v1alpha1
    kind: WorkerPool
    spec:
      workers: "[]string"
      image: string

  resources:
    - id: workerPods
      forEach:
        - worker: ${schema.spec.workers}
      template:
        apiVersion: v1
        kind: Pod
        metadata:
          name: ${schema.metadata.name + '-' + worker}
        spec:
          containers:
            - name: app
              image: ${schema.spec.image}
```

When a user creates an instance with `workers: ["alice", "bob"]`, kro creates two
Pods - one for each worker. If the user later updates the list to
`["alice", "bob", "charlie"]`, kro creates a third Pod for "charlie".

## forEach Syntax

The `forEach` field is an array of iterators. Each iterator is a single-entry
map binding a variable name to an expression:

```kro
forEach:
  - region: ${schema.spec.regions}
```

Each map entry must have exactly one key-value pair:
- **Key**: The iterator variable name (available in your template expressions)
- **Value**: A CEL expression that **must evaluate to an array**

Arrays maintain their order, so resources are indexed in the same order as
elements appear in the source array - this ordering is deterministic and
predictable for naming and labeling purposes.

### Iterating Over Maps

If you have map (object) data, convert it to a sorted array first. Maps have
non-deterministic iteration order in Go, so converting to a sorted array ensures
consistent resource creation order:

```kro
# Given schema.spec.labels = {"app": "web", "env": "prod", "team": "platform"}
forEach:
  # Convert map keys to a sorted array
  - key: ${schema.spec.labels.map(k, k).sort()}
```

You can then access the value inside the template using the key.

## Iterator Variables

The variable name you specify becomes available in the template, holding the
current element:

```kro
- id: workerPods
  forEach:
    - worker: ${schema.spec.workers}  # ["alice", "bob", "charlie"]
  template:
    kind: Pod
    metadata:
      # <instance-name>-alice, <instance-name>-bob, <instance-name>-charlie
      name: ${schema.metadata.name + '-' + worker}
```

To access the index, iterate over a range instead:

```kro
- id: workerPods
  forEach:
    - idx: ${lists.range(size(schema.spec.workers))}
  template:
    kind: Pod
    metadata:
      # <instance-name>-0, <instance-name>-1, <instance-name>-2
      name: ${schema.metadata.name + '-' + string(idx)}
    spec:
      containers:
        - name: worker
          env:
            - name: WORKER_NAME
              value: ${schema.spec.workers[idx]}  # alice, bob, charlie
```

:::tip
Use descriptive iterator names that reflect what you're iterating over, like
`worker`, `region`, or `dbSpec` rather than generic names like `item` or `i`.
:::

### Resource Naming

Each resource in a collection must have a unique name. Always include the
iterator variable in `metadata.name` to ensure uniqueness:

```kro
- id: workerPods
  forEach:
    - worker: ${schema.spec.workers}
  template:
    kind: Pod
    metadata:
      # Good: includes iterator variable for uniqueness
      name: ${schema.metadata.name + '-' + worker}
```

:::warning Unique Names Required
Resource names must be unique not just within the collection, but across all
instances and RGDs in the same namespace. Kubernetes identifies resources by
namespace + name + kind + apiVersion - if two resources share these, they
conflict.

Best practice: always include the instance name to avoid cross-instance
collisions:

```kro
# With regions: ["us-east", "us-west"] and tiers: ["web", "api"]
name: ${schema.metadata.name + '-' + region + '-' + tier}
# Creates: myapp-us-east-web, myapp-us-east-api, myapp-us-west-web, myapp-us-west-api
```

If you omit `schema.metadata.name`, two instances with the same iterator values
will overwrite each other's resources.
:::

## Multiple Iterators (Cartesian Product)

When you specify multiple iterators, kro creates resources for every combination
(cartesian product) of values:

```kro
- id: deployments
  forEach:
    - region: ${schema.spec.regions}  # ["us-east", "us-west"]
    - tier: ${schema.spec.tiers}      # ["web", "api"]
  template:
    kind: Deployment
    metadata:
      # Creates 4 deployments: myapp-us-east-web, myapp-us-east-api, etc.
      name: ${schema.metadata.name + '-' + region + '-' + tier}
    spec:
      template:
        spec:
          containers:
            - name: app
              image: ${schema.spec.image}
              env:
                - name: REGION
                  value: ${region}
                - name: TIER
                  value: ${tier}
```

With `regions: ["us-east", "us-west"]` and `tiers: ["web", "api"]`, this creates
4 deployments covering all combinations.

### Combination Order

The first iterator is the outer loop, subsequent iterators are nested inside.
Resources are created in this order:

```yaml
# forEach:
#   - region: ["us-east", "us-west"]
#   - tier: ["web", "api"]

# Creates resources in order:
# 1. region=us-east, tier=web
# 2. region=us-east, tier=api
# 3. region=us-west, tier=web
# 4. region=us-west, tier=api
```

This order is deterministic as long as each iterator's source array has
deterministic order (arrays are ordered, maps are not - see
[Iterating Over Maps](#iterating-over-maps) if you need to iterate over map data).

:::important
If any iterator's collection is empty, zero resources are created. This follows
standard cartesian product behavior: `2 × 0 = 0`.
:::

## Collection Sources

The iterator expression must evaluate to an array. Arrays can come from various sources:

<Tabs>
<TabItem value="instance-spec" label="Instance Spec">

```kro
spec:
  schema:
    spec:
      regions: "[]string"
  resources:
    - id: regionalConfigs
      forEach:
        - region: ${schema.spec.regions}
      template:
        kind: ConfigMap
        metadata:
          name: ${schema.metadata.name + '-config-' + region}
```

</TabItem>
<TabItem value="resource-spec" label="Resource Spec">

```kro
resources:
  - id: database
    template:
      apiVersion: db.example.com/v1
      kind: Database
      metadata:
        name: ${schema.metadata.name + '-db'}
      spec:
        shards:
          - name: users
            region: us-east
          - name: orders
            region: us-west

  - id: shardBackups
    forEach:
      - shard: ${database.spec.shards}
    template:
      kind: CronJob
      metadata:
        name: ${schema.metadata.name + '-backup-' + shard.name}
      spec:
        schedule: "0 2 * * *"
        # ...
```

</TabItem>
<TabItem value="resource-status" label="Resource Status">

```kro
resources:
  - id: cluster
    template:
      apiVersion: kafka.example.com/v1
      kind: KafkaCluster
      metadata:
        name: ${schema.metadata.name + '-kafka'}
      spec:
        brokers: 3

  - id: brokerServices
    forEach:
      - broker: ${cluster.status.brokers}
    template:
      kind: Service
      metadata:
        name: ${schema.metadata.name + '-broker-' + string(broker.id)}
      spec:
        type: ExternalName
        externalName: ${broker.host}
```

</TabItem>
<TabItem value="array" label="Array Literal">

```kro
resources:
  - id: zoneConfigs
    forEach:
      - zone: ${["us-east-1a", "us-east-1b", "us-east-1c"]}
    template:
      kind: ConfigMap
      metadata:
        name: ${schema.metadata.name + '-config-' + zone}

  - id: shards
    forEach:
      - shardNum: ${lists.range(3)}  # CEL function that returns [0, 1, 2]
    template:
      kind: StatefulSet
      metadata:
        name: ${schema.metadata.name + '-shard-' + string(shardNum)}
```

</TabItem>
</Tabs>

## Referencing Collections

Other resources can reference a collection by its ID. The collection exposes
all created resources as an array, which you can use with CEL functions like
`map()`, `filter()`, and `all()`.

### In a Resource

Use CEL functions to aggregate collection data into a single resource:

```kro
- id: workerPods
  forEach:
    - worker: ${schema.spec.workers}
  template:
    kind: Pod
    metadata:
      name: ${schema.metadata.name + '-' + worker}
    # ...

- id: summary
  template:
    kind: ConfigMap
    metadata:
      name: ${schema.metadata.name + '-summary'}
    data:
      podNames: ${workerPods.map(p, p.metadata.name).join(', ')}
      count: ${string(size(workerPods))}
```

### In Another Collection

One collection can iterate over another to create dependent resources:

```kro
- id: databases
  forEach:
    - dbSpec: ${schema.spec.databases}
  template:
    apiVersion: db.example.com/v1
    kind: Database
    metadata:
      name: ${schema.metadata.name + '-' + dbSpec.name}
    spec:
      storage: ${dbSpec.storage}

- id: backupJobs
  forEach:
    - db: ${databases}  # iterate over the collection
  template:
    kind: CronJob
    metadata:
      name: ${schema.metadata.name + '-backup-' + db.metadata.name}
    spec:
      schedule: "0 2 * * *"
      jobTemplate:
        spec:
          template:
            spec:
              containers:
                - name: backup
                  env:
                    - name: DB_HOST
                      value: ${db.status.endpoint}
```

### Dependency Behavior

Like references in templates, resource references in `forEach` expressions
create edges in the dependency graph. Any reference between a resource and a
collection - whether in the `forEach` expression or in the template - causes
kro to wait for the referenced collection to be fully reconciled and ready
(`readyWhen` resolves to true) before proceeding:

```kro
resources:
  # Collection A: creates multiple databases
  - id: databases
    forEach:
      - dbSpec: ${schema.spec.databases}
    template:
      kind: Database
      metadata:
        name: ${schema.metadata.name + '-' + dbSpec.name}
      # ...

  # Collection B: depends on Collection A
  # kro waits for ALL databases to be ready before creating backup jobs
  - id: backupJobs
    forEach:
      - db: ${databases}
    template:
      kind: CronJob
      metadata:
        name: ${schema.metadata.name + '-backup-' + db.metadata.name}
      # ...

  # Resource C: depends on Collection B
  # kro waits for ALL backup jobs to be ready before creating the summary
  - id: summary
    template:
      kind: ConfigMap
      metadata:
        name: ${schema.metadata.name + '-summary'}
      data:
        backupCount: ${string(size(backupJobs))}
```

## Readiness with Collections

For collections, `readyWhen` uses the `each` keyword for per-item readiness
checks. The collection is ready when **all** items pass **all** readyWhen
expressions (AND semantics):

```kro
- id: workerPods
  forEach:
    - worker: ${schema.spec.workers}
  readyWhen:
    - ${each.status.phase == 'Running'}  # Per-item check using `each`
  template:
    kind: Pod
    metadata:
      name: ${schema.metadata.name + '-' + worker}
    spec:
      containers:
        - name: app
          image: ${schema.spec.image}
```

In this example, `workerPods` is only considered ready when **every** pod in the
collection has `status.phase == 'Running'`. Dependent resources wait for this
condition before proceeding.

:::tip
Without `readyWhen`, a collection is considered ready once all resources are
created. Add `readyWhen` when dependent resources need specific conditions to be
true, not just existence.
:::

## Conditional Collections

The `includeWhen` field operates on the collection as a whole. If the condition
evaluates to `false`, the entire collection is skipped - no resources are
created. You cannot use `includeWhen` to exclude individual items:

```kro
- id: backupJobs
  includeWhen:
    - ${schema.spec.backupsEnabled}  # all or nothing
  forEach:
    - dbSpec: ${schema.spec.databases}
  template:
    kind: CronJob
    metadata:
      name: ${schema.metadata.name + '-backup-' + dbSpec.name}
    # ...
```

To filter individual items from a collection, use `filter()` in the `forEach`
expression:

```kro
- id: backupJobs
  forEach:
    # Only iterate over databases that have backups enabled
    - dbSpec: ${schema.spec.databases.filter(d, d.backupEnabled)}
  template:
    kind: CronJob
    metadata:
      name: ${schema.metadata.name + '-backup-' + dbSpec.name}
    # ...
```

## Exposing Collection Data in Instance Status

Status expressions can reference collections to compute aggregate values. This
lets you expose summary information about the collection in your custom
resource's status:

```kro
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
        - worker: ${schema.spec.workers}
      template:
        kind: Pod
        metadata:
          name: ${schema.metadata.name + '-' + worker}
        # ...
```

## Collection Lifecycle

Collections automatically stay in sync with their source data. When the source
array changes, kro creates, updates, or deletes resources to match.

### Scaling Up

When items are added to the source array, kro creates new resources:

```yaml
# Instance spec change:
# workers: ["alice", "bob"] → ["alice", "bob", "charlie"]
#
# Result: kro creates a new pod for "charlie"
```

### Scaling Down

When items are removed from the source array, kro automatically deletes the
corresponding resources:

```yaml
# Instance spec change:
# workers: ["alice", "bob", "charlie"] → ["alice"]
#
# Result: kro deletes pods for "bob" and "charlie"
```

This cleanup happens automatically through kro's applyset mechanism - orphaned
resources are pruned on each reconciliation.

### Empty Collections

An empty array results in zero resources - this is not an error:

```yaml
# workers: []
# Result: zero pods created (collection exists but is empty)
```

This is useful for optional features that can be enabled by adding items to an
initially empty array.

:::important
If any iterator in a cartesian product is empty, zero resources are created.
For example, `regions: ["us-east"]` × `tiers: []` = zero deployments.
:::

### Drift Detection

kro continuously reconciles collection resources to match the desired state. If
a resource is modified externally (drift), kro restores it:

```yaml
# 1. kro creates ConfigMap with data.prefix: "original"
# 2. Someone manually changes data.prefix to "MODIFIED"
# 3. kro detects the drift and restores data.prefix to "original"
```

This applies to all fields that kro manages. Fields added by other controllers
(not in kro's template) are preserved.

---

## How Collections Work

This section explains the internal mechanics for those who want to understand
what happens under the hood. You don't need this to use collections effectively.

When you add `forEach` to a resource, kro treats that resource block as a
template for multiple resources rather than a single one.

**At RGD validation time (static analysis)**, kro analyzes the `forEach`
expressions to determine the element types. If `schema.spec.workers` is
`[]string`, kro knows each iteration yields a string and types the iterator
variable accordingly. This enables [static type checking](../05-static-type-checking.md)
of expressions inside the template before any resources are created.

**At instance reconciliation time (runtime)**, when the reconciler reaches a
collection node in the dependency graph:

1. Evaluates each forEach CEL expression to get the current collections
2. Creates multiple resources - one per element (or combination)
3. Compares against existing resources and creates, updates, or deletes as
   needed
4. Evaluates `readyWhen` (if specified) across all collection resources before
   proceeding to dependent nodes

Each resource in the collection is tracked independently. If one resource fails
to reconcile, the others continue - kro doesn't require all-or-nothing success
within a collection.

:::note
In kro, each entry in `spec.resources` is a node in the dependency graph. A
collection (a resource with `forEach`) is also a single node in the graph - it
creates multiple resources at runtime, but dependencies treat it as one unit.
:::

## Next Steps

- **[External References](./05-external-references.md)** - Reference existing
  resources not managed by kro
- **[CEL Expressions](../03-cel-expressions.md)** - Learn about CEL functions
  like `lists.range()`, `filter()`, and `map()`
- **[Dependencies & Ordering](../04-dependencies-ordering.md)** - Understand
  how collections affect the dependency graph
