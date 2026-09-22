---
sidebar_position: 2
---

# Nodes

A Graph is a list of **nodes**. Each node has an `id`, exactly one field that
determines what kind of node it is, and optionally the
[modifiers](./03-modifiers.md) `includeWhen`, `readyWhen`, and `forEach`.

```kro
spec:
  nodes:
    - id: <name>          # how other nodes reference this one in CEL
      template: {}        # one of: template | ref | def | patch | graph
      includeWhen: []     # optional
      readyWhen: []       # optional
      forEach: []         # optional
```

| Kind | Reads | Writes | Owns | Publishes a value |
| --- | --- | --- | --- | --- |
| `template` | Yes | Yes | Yes | Yes |
| `ref` | Yes | No | No | Yes |
| `def` | No | No | No | Yes |
| `patch` | Yes | Yes (fields only) | No | No |
| `graph` | - | - | - | Yes (its nodes) |

A node **publishes a value** when other nodes can reference it as `${id...}`.

## Node IDs

The `id` is the handle other nodes use in CEL expressions. It must:

- Match `^[A-Za-z][A-Za-z0-9]*$` (letters and digits, starting with a letter).
- Be unique within the Graph. The API server enforces this.
- Not be a reserved word. The following are reserved and rejected when the
  Graph is compiled: `apiVersion`, `kind`, `metadata`, `namespace`, `spec`,
  `status`, `graph`, `graphengine`, `kro`, `each`, `item`, `items`, `object`,
  `self`, `this`, `context`, and every CEL keyword (`true`, `false`, `null`,
  `in`, `as`, `break`, `const`, `continue`, `else`, `for`, `function`, `if`,
  `import`, `let`, `loop`, `package`, `return`, `var`, `void`, `while`).

## `template`

A `template` node creates and manages a Kubernetes resource. kro applies it with
server-side apply on create and on every change, and deletes it when the node is
removed, when its `includeWhen` becomes false, or when the Graph is deleted.

```kro
- id: config
  template:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-config
    data:
      LOG_LEVEL: info
```

The payload is a complete Kubernetes manifest. `apiVersion`, `kind`, and
`metadata.name` are required. `metadata.namespace` defaults to the Graph's
namespace for namespaced kinds and must be omitted for cluster-scoped kinds.
The resource type must exist in the cluster when the Graph is compiled, and CEL
expressions in the template are type-checked against its schema.

Other nodes read the applied resource's observed state. `${config.data.LOG_LEVEL}`
returns the value kro wrote; `${deployment.status.availableReplicas}` returns
whatever the cluster reports.

One restriction applies to `CustomResourceDefinition` templates: CEL
expressions are allowed only under `metadata`. A CRD's schema must be literal.

## `ref`

A `ref` node reads a resource that exists outside the Graph and makes it
available to other nodes. kro never creates, updates, or deletes it.

```kro
# One resource, by name
- id: config
  ref:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-config
      namespace: platform    # optional; defaults to the Graph's namespace

# Many resources, by label selector
- id: services
  ref:
    apiVersion: v1
    kind: Service
    metadata:
      selector:
        matchLabels:
          expose: "true"
```

`metadata` takes either `name` or `selector`, never both. With `selector`, the
node's value is a list, and other nodes use list functions on it:
`${services.map(s, s.metadata.name)}`, `${size(services) > 0}`. An empty
`selector: {}` matches every object of that kind. When `namespace` is omitted on
a selector `ref`, kro lists across all namespaces.

A `ref` whose target does not exist yet is not an error. The Graph's
`ResourcesConverged` condition stays `False` and kro retries with backoff,
so a Graph can be created before the things it reads.

kro watches every `ref` target, so a change to the referenced resource
reconciles the Graph. The full semantics of single and collection references are
documented in [External References](../reconciliation/04-external-references.md).

## `def`

A `def` node introduces a value into the Graph without reading or writing any
Kubernetes resource. Use it to compute something once and reference it from
several places.

```kro
- id: naming
  def:
    prefix: ${deployment.metadata.name + '-' + deployment.metadata.namespace}
    labels:
      app: ${deployment.metadata.labels['app']}

- id: service
  template:
    apiVersion: v1
    kind: Service
    metadata:
      name: ${naming.prefix}-svc
      labels: ${naming.labels}
```

The payload is a free-form object. Literal fields are typed from their values
(a string literal is a string, `3` is an integer, a nested map is an object), so
a reference like `${naming.prefxi}` is caught as an unknown field when the Graph
compiles. Fields whose value is a CEL expression are typed dynamically.

A `def` with `forEach` produces a list, one element per iteration, which is a
common way to reshape a collection read by a `ref` before fanning it out to a
`template`.

## `patch`

A `patch` node contributes fields to a resource that this Graph does **not**
own. The target must exist; until it does, the node is reported as not ready
and kro retries. kro applies only the fields in the payload,
under a field manager dedicated to this node, and never takes ownership of the
whole object. When the node is removed, or the Graph is deleted, kro releases
those fields; the target itself is never deleted.

```kro
- id: appStatus
  patch:
    apiVersion: apps.example.com/v1
    kind: WebApp
    metadata:
      name: my-webapp
    status:
      endpoint: ${service.status.loadBalancer.ingress[0].hostname}
      ready: ${deployment.status.availableReplicas > 0}
```

The payload is authored like a `template`: `apiVersion`, `kind`, and
`metadata.name` are required, `metadata.namespace` is optional, and everything
else is the contribution. kro derives **which endpoint** to write from the
fields present:

- A top-level `status` key writes through the `status` subresource.
- Any other top-level key (`spec`, `data`, ...), or any `metadata` key other
  than `name` and `namespace` (such as `labels` or `annotations`), writes to the
  main resource.
- A single `patch` node may not mix the two. If a resource needs both a `spec`
  and a `status` contribution, use two `patch` nodes.
- The node must contribute at least one field. A payload that is only identity
  is rejected.
- `metadata.ownerReferences`, `metadata.finalizers`, `metadata.deletionTimestamp`,
  and `metadata.uid` are rejected, because a patch must never be able to delete
  or terminate its target.
- The `scale` subresource is not supported.

A `patch` node does not publish a value. Other nodes cannot reference it in CEL
expressions.

`patch` accepts `forEach`. Each iteration renders a separate target, and every
iterator must appear in `metadata.name` or `metadata.namespace` so that the
targets are distinct. This is how one Graph writes status onto many objects at
once.

Status patches are applied with force, so kro's contribution wins a conflict
with another manager on the status subresource. Main-resource patches are not
forced; a conflict with another field manager is reported in the Graph's
conditions rather than overwritten.

## `graph`

A `graph` node nests another Graph inside this one. The payload is a `nodes`
list, exactly as in `spec.nodes`. The child's nodes are addressable under the
parent node's `id`, and the child can reference the parent's nodes.

```kro
- id: backend
  graph:
    nodes:
      - id: deployment
        template:
          apiVersion: apps/v1
          kind: Deployment
          # ...
      - id: service
        template:
          apiVersion: v1
          kind: Service
          metadata:
            name: ${deployment.metadata.name}
          # ...

- id: ingress
  template:
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    spec:
      rules:
        - http:
            paths:
              - path: /
                pathType: Prefix
                backend:
                  service:
                    name: ${backend.service.metadata.name}
                    port:
                      number: 80
```

`graph` nodes do not accept `includeWhen`, `readyWhen`, or `forEach`. Scoping
rules and the alternative of stamping out separate Graph objects are covered in
[Scopes and Nesting](./04-scopes-and-nesting.md).

## Dynamic Types

In `template`, `ref`, and `patch` nodes, `apiVersion` and `kind` may be CEL
expressions. This lets a Graph work with a kind it only learns about from
another node, such as the instances of a CRD it reads.

```kro
- id: crd
  ref:
    apiVersion: apiextensions.k8s.io/v1
    kind: CustomResourceDefinition
    metadata:
      name: widgets.example.com

- id: widgets
  ref:
    apiVersion: ${crd.spec.group}/${crd.spec.versions[0].name}
    kind: ${crd.spec.names.kind}
    metadata:
      selector: {}
```

A node is dynamic when its `apiVersion` or `kind` contains `${`. A CEL
expression in `metadata.name` alone does not make a node dynamic. For a dynamic
node kro cannot resolve a schema at compile time, so:

- CEL expressions in the payload are not type-checked against a schema.
- Other nodes that reference the dynamic node see its fields as untyped, and
  type errors surface at runtime rather than at compile time.
- If the type does not exist in the cluster yet, the node is held as not ready
  and kro reconciles the Graph again when the matching CRD appears.

A node with a literal `apiVersion` and `kind` for a custom resource requires that
CRD to be installed when the Graph is compiled. To bootstrap a CRD and its
instances from one Graph, use a dynamic type for the instances or stamp them
from a child Graph; see [Scopes and Nesting](./04-scopes-and-nesting.md).

## Next Steps

- **[Modifiers](./03-modifiers.md)** - `includeWhen`, `readyWhen`, and `forEach` per node kind
- **[Scopes and Nesting](./04-scopes-and-nesting.md)** - How `graph` nodes and stamped Graphs compose
- **[External References](../reconciliation/04-external-references.md)** - Full semantics shared by `ref` and `externalRef`
- **[Collections](../reconciliation/03-collections.md)** - `forEach` on `template`, `def`, and `patch`
