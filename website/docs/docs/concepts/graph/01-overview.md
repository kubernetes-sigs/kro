---
sidebar_position: 1
sidebar_label: Overview
---

import GraphProcessFlow from '@site/src/components/GraphProcessFlow';

# Overview

:::warning Alpha Feature
`Graph` is an alpha API introduced in kro v0.10.0. It is disabled by default
and enabled with the `GraphKind` [feature gate](../../advanced/02-feature-gates.md#graphkind).
The API and its behavior may change between releases without a compatibility
guarantee.
:::

A **Graph** is a namespaced Kubernetes resource that describes a set of
resources and the relationships between them. Create a Graph and kro applies its
resources in dependency order and keeps them in sync; delete it and the resources
it created are removed. Unlike a [ResourceGraphDefinition](../rgd/01-overview.md),
a Graph does not generate a new API, has no schema, and has no instances. It is
the resources.

## When to Use a Graph

A Graph fits when you want kro's composition engine but not a new API:

- **Static bundles.** Install a set of manifests with ordering and health
  checks as a single object.
- **Decorators.** Watch for existing resources (for example every Namespace with
  a label) and create something alongside each one.
- **Aggregation.** Read many resources and fold them into one, such as a single
  Ingress with a rule per matching Service.
- **Status contribution.** Write fields onto resources the Graph does not own,
  through a `patch` node.

If the same composition needs to be created many times with different inputs,
or needs to be exposed as a Kubernetes API, use a ResourceGraphDefinition
instead. See [Composition in kro](../00-overview.md) for a side-by-side
comparison.

## How It Works

1. **You create a Graph** listing the resources you want and how they relate
2. **kro compiles it**, checking every expression and inferring the order
3. **kro applies the resources** in that order, as a ServiceAccount in the Graph's namespace
4. **kro watches** the resources it created and the ones it reads, and reconciles on any change

<GraphProcessFlow />

## Example

```kro
apiVersion: kro.run/v1alpha1
kind: Graph
metadata:
  name: my-app
  namespace: default
spec:
  serviceAccountName: my-app-applier
  nodes:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: my-app
        spec:
          replicas: 3
          selector:
            matchLabels:
              app: my-app
          template:
            metadata:
              labels:
                app: my-app
            spec:
              containers:
                - name: app
                  image: nginx

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${deployment.metadata.name}-svc
        spec:
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - port: 80
```

kro reads the CEL expressions in `service`, sees that they reference
`deployment`, and applies the Deployment first. The order of the `nodes` list
does not matter.

```bash
$ kubectl get graphs
NAME     READY   AGE
my-app   True    30s
```

## Anatomy of a Graph

```kro
apiVersion: kro.run/v1alpha1
kind: Graph
metadata: {}                 # Standard Kubernetes metadata; a Graph is namespaced
spec:
  serviceAccountName: ""     # ServiceAccount in this namespace used to apply resources
  nodes: []                  # The resources and values that make up the Graph
status:                      # Managed by kro
  conditions: []
  managedResources: []
  contributions: []
  appliedServiceAccount: ""
```

- **`spec.nodes`** - An unordered list of [nodes](./02-nodes.md). Each node has
  an `id` and exactly one of `template`, `ref`, `def`, `patch`, or `graph`.
  Node IDs must be unique within the Graph and match `^[A-Za-z][A-Za-z0-9]*$`.
- **`spec.serviceAccountName`** - The ServiceAccount kro impersonates when it
  applies this Graph's resources. It is always resolved in the Graph's own
  namespace. When empty, kro uses the namespace's `default` ServiceAccount. See
  [Identity and Permissions](#identity-and-permissions).
- **`status`** - Conditions and the inventory of resources the Graph has
  applied. See [Status and Lifecycle](./05-status-and-lifecycle.md).

For complete field documentation, see the [Graph API Reference](../../../api/crds/graph.md).

## Evaluation Model

kro compiles a Graph into a dependency graph the same way it does an RGD: every
CEL reference from one node to another is an edge, cycles are rejected, and
nodes are applied in topological order. See
[Dependencies & Ordering](../expressions/03-dependencies-ordering.md).

A node is applied as soon as every field it references is present in the
observed state of the nodes it depends on. It does **not** wait for those nodes
to satisfy their `readyWhen` conditions. `readyWhen` on a Graph node reports
health into the Graph's status; it does not gate downstream nodes. This differs
from an RGD, where dependents wait for readiness. See
[Readiness](../reconciliation/02-readiness.md#dependencies-and-readiness) for
the comparison and for how to gate on health when you need to.

Within one Graph, nodes are applied serially in topological order, and the items
of a `forEach` collection are applied in parallel. Different Graphs reconcile
independently and in parallel.

## Identity and Permissions

A Graph is namespaced and user-creatable, and it describes cluster resources
directly. To keep that from running as the kro controller's own broad identity,
kro applies a Graph's resources while **impersonating a ServiceAccount in the
Graph's namespace**. Every create, update, patch, and delete the Graph performs is
authorized against that ServiceAccount's RBAC, so a Graph can never do more than
a ServiceAccount in its namespace is already allowed to do.

The trust model is the same as `create pod`: whoever can create or update a
Graph in a namespace can act as any ServiceAccount in that namespace that kro is
allowed to impersonate. Read
[Access Control](../../advanced/01-access-control.md#graph-and-serviceaccount-impersonation)
before enabling Graphs in a shared cluster.

The watches kro uses to notice changes to a Graph's resources are not
impersonated. They run under the kro controller's own identity and are
cluster-wide, so the controller itself also needs `list` and `watch` on every
resource type a Graph creates, reads, or patches. In `rbac.mode: unrestricted`
it already has them. In `rbac.mode: aggregation` you grant them yourself, as
described below.

## Enabling Graphs

Graphs are off by default. Enabling them is a cluster-level decision with
several steps.

1. **Install the Graph CRD.** Helm does not install or update CRDs on
   `helm upgrade`. Apply it directly:

   ```bash
   kubectl apply --server-side -f https://raw.githubusercontent.com/kubernetes-sigs/kro/main/helm/crds/kro.run_graphs.yaml
   ```

2. **Enable the feature gate.** In your Helm values:

   ```yaml
   config:
     featureGates:
       GraphKind: true
   ```

   With the gate on, the chart also grants the kro controller the `impersonate`
   verb on ServiceAccounts in `rbac.mode: aggregation`. In `unrestricted` mode
   the controller already holds every verb. Consider narrowing this grant per
   namespace; see
   [Restricting which ServiceAccounts kro may impersonate](../../advanced/01-access-control.md#restricting-which-serviceaccounts-kro-may-impersonate).

   If you deploy kro without the Helm chart, you must also pass
   `--controller-namespace` and `--controller-service-account` to the
   controller. With `GraphKind` enabled and either flag missing, the controller
   refuses to start.

3. **Grant the controller `list` and `watch` on the resource types Graphs will
   manage.** In `rbac.mode: aggregation`, create a ClusterRole labeled
   `rbac.kro.run/aggregate-to-controller: "true"` that grants `list` and
   `watch` on every resource type any Graph creates, reads, or patches. In
   `unrestricted` mode the controller already holds them. See
   [What the kro controller itself needs](../../advanced/01-access-control.md#what-the-kro-controller-itself-needs)
   for an example.

4. **Create the applier ServiceAccount and its RBAC.** The ServiceAccount named
   in `spec.serviceAccountName` (or the namespace's `default` ServiceAccount)
   needs permission for every resource type the Graph creates, reads, patches,
   or deletes, including `get`, `list`, and `watch` on each of them.

5. **Grant users access to the `graphs` resource.** The chart deliberately does
   not add `graphs` to the built-in `edit`, `admin`, or `view` ClusterRoles.
   Create a Role or ClusterRole that grants `kro.run` / `graphs` to the users or
   groups who should author Graphs.

## Next Steps

- **[Nodes](./02-nodes.md)** - The five node kinds and their rules
- **[Modifiers](./03-modifiers.md)** - Which nodes accept `includeWhen`, `readyWhen`, and `forEach`
- **[Scopes and Nesting](./04-scopes-and-nesting.md)** - Composing Graphs from Graphs
- **[Status and Lifecycle](./05-status-and-lifecycle.md)** - Conditions, inventory, and deletion
- **[Graph Examples](../../../examples/graph/namespace-decorator.md)** - Decorator, fan-in, bundle, and singleton patterns
- **[Access Control](../../advanced/01-access-control.md#graph-and-serviceaccount-impersonation)** - The impersonation security model
