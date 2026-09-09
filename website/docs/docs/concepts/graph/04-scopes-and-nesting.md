---
sidebar_position: 4
---

import ScopeFrames from '@site/src/components/ScopeFrames';

# Scopes and Nesting

A Graph is a **scope**: its node IDs are visible only within it, and nothing
outside can reference them except through the Kubernetes resources it creates.
Graphs compose in two ways: a `graph` node nests a child Graph inline, and a
`template` node whose `kind` is `Graph` stamps out a separate Graph object.

## Inline Subgraphs

A [`graph` node](./02-nodes.md#graph) embeds a child `nodes` list. The child is
compiled and reconciled as part of the parent, in the same reconcile, under the
same identity.

```kro
spec:
  nodes:
    - id: shared
      def:
        app: my-app

    - id: backend
      graph:
        nodes:
          - id: deployment
            template:
              apiVersion: apps/v1
              kind: Deployment
              metadata:
                name: ${shared.app}-backend        # captures the parent's node
              # ...
          - id: service
            template:
              apiVersion: v1
              kind: Service
              metadata:
                name: ${deployment.metadata.name}   # sibling within the child
              # ...

    - id: ingress
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: ${shared.app}
        spec:
          defaultBackend:
            service:
              name: ${backend.service.metadata.name}  # reaches into the child
              port:
                number: 80
```

<ScopeFrames />

### Scoping Rules

The child forms a lexical frame with these rules:

- **Capture.** A child node may reference any node of the enclosing Graph
  (`${shared.app}` above). That reference becomes a dependency of the `graph`
  node as a whole: the child does not start until the captured nodes are in
  scope.
- **Shadowing.** A child node may reuse an ID from the enclosing Graph. Inside
  the child, the nearest definition wins.
- **Addressing outward.** The enclosing Graph reaches a child's nodes through
  the `graph` node's `id`: `${backend.service.metadata.name}`. Siblings of the
  `graph` node cannot reference the child's nodes directly.
- **No mixing.** A single CEL expression may reference nodes from only one
  frame. `${deployment.metadata.name + shared.app}` inside the child, mixing a
  child node with a captured parent node, is rejected. Compute the combined
  value in a `def` in one frame and reference that instead.
- **Depth.** Nesting has no depth limit.

A `graph` node accepts no [modifiers](./03-modifiers.md). Put `includeWhen`,
`readyWhen`, or `forEach` on the nodes inside it.

Inline subgraphs share the parent's status. There is one set of conditions and
one `managedResources` inventory for the whole Graph, and the resources a child
creates are labeled with the qualified node path (for example
`backend.deployment`).

## Stamped Graphs

A `template` node can create a `Graph` like any other Kubernetes resource. This
is useful with `forEach`: one parent Graph produces one child Graph per item.

```kro
spec:
  nodes:
    - id: namespaces
      ref:
        apiVersion: v1
        kind: Namespace
        metadata:
          selector:
            matchLabels:
              baseline: "true"

    - id: baselines
      forEach:
        - ns: ${namespaces}
      template:
        apiVersion: kro.run/v1alpha1
        kind: Graph
        metadata:
          name: baseline
          namespace: ${ns.metadata.name}
        spec:
          serviceAccountName: baseline-applier
          nodes:
            - id: quota
              template:
                apiVersion: v1
                kind: ResourceQuota
                metadata:
                  name: baseline
                spec:
                  hard:
                    pods: "50"
```

A stamped child is an independent object. The parent's apply completes as soon
as the child Graph exists; the child then compiles and converges on its own
reconcile loop, under its own `spec.serviceAccountName`, with its own status.

### Parent and Child Readiness Are Independent

The parent's `ResourcesConverged` and `Ready` conditions describe the parent's
own nodes. The parent can be `Ready` while a stamped child is still compiling,
invalid, or not yet converged. Nothing gates the parent on the child
automatically.

To make a parent wait on a child, read the child back with a `ref` node and put
its `Ready` condition in a `readyWhen`:

```kro
    - id: childStatus
      ref:
        apiVersion: kro.run/v1alpha1
        kind: Graph
        metadata:
          name: baseline
          namespace: team-a
      readyWhen:
        - ${childStatus.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')}
```

### Deferring Expressions to the Child

A child Graph's spec is data inside the parent's template, so every `${...}` in
it is, by default, evaluated by the **parent**. To write an expression the
**child** should evaluate, wrap it in a quoted CEL string literal. The parent
evaluates the string literal and emits the text `${...}` into the child's spec;
the child then evaluates it in its own scope.

```kro
# Evaluated by the parent: the child's namespace is fixed at stamp time
namespace: ${ns.metadata.name}

# Evaluated by the child: the parent emits the literal text ${quota.metadata.name}
name: ${'${quota.metadata.name}'}-copy
```

Each extra level of nesting is one more layer of quoting:
`${"${'${...}'}"}` is evaluated by the grandchild. A bare nested `${` that is not
immediately inside a quote, such as `${outer(${inner})}`, does not parse.

The parent type-checks only the expressions it evaluates itself. An error in a
deferred expression is reported on the child Graph's `Accepted` condition, not
on the parent.

## Choosing Between the Two

| | Inline subgraph (`graph` node) | Stamped Graph (`template` with `kind: Graph`) |
| --- | --- | --- |
| Reconciled | With the parent, synchronously | Independently, asynchronously |
| Identity | The parent's ServiceAccount | The child's own `spec.serviceAccountName` |
| Status | Rolled into the parent | Separate object with its own conditions |
| Parent can reference child nodes | Yes, as `${nodeId.childId...}` | Only by reading the child Graph object with a `ref` |
| `forEach` | On nodes inside the child | On the stamping `template` node |
| Use for | Grouping and reuse within one unit of work | Fan-out across namespaces, delegated identities, or multi-stage bootstrapping |

## Next Steps

- **[Nodes](./02-nodes.md)** - The `graph` node and dynamic types
- **[Status and Lifecycle](./05-status-and-lifecycle.md)** - What the parent's status does and does not include
- **[Dependencies & Ordering](../expressions/03-dependencies-ordering.md)** - How captured references become dependencies
