---
sidebar_position: 1
---

# Access Control

There are currently two modes of access control supported by **kro**, if you
[install through the Helm chart](../getting-started/01-Installation.md#installation):

- `unrestricted`

- `aggregation`

The mode is selected with a `values` property `rbac.mode`, and defaults to `unrestricted`.

## `unrestricted` Access

In the `unrestricted` access mode, the chart includes a `ClusterRole` granting
**kro** _full control to every resource type in your cluster_. This can be
useful for experimenting in a test environment, where access control is not
necessary, but is not recommended in a production environment.

In this mode, anyone with access to create `ResourceGraphDefinition` resources,
effectively also has admin access to the cluster.

## `aggregation` Access

In the `aggregation` access mode, the chart includes an [_aggregated_ `ClusterRole`](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#aggregated-clusterroles)
which dynamically includes all rules from all `ClusterRoles` that have the label
`rbac.kro.run/aggregate-to-controller: "true"`.

There is a very minimal set of permissions provisioned by the chart itself, just
enough to let **kro** run at all: full permissions for `ResourceGraphDefinition`s
and its subresources, and full permissions for `CustomResourceDefinitions` as
**kro** will create them in response to the existence of an RGD.

However, this does _not_ automatically set up permissions for **kro** to actually
reconcile those generated CRDs! In other words, when using this mode, you will
need to provision additional access for **kro** for every new resource type you
define.

### Example

If you want to create a `ResourceGraphDefinition` that specifies a new resource
type with `kind: Foo`, and where the graph includes an `apps/v1/Deployment` and
a `v1/ConfigMap`, you will need to create the following `ClusterRole` to ensure
**kro** has enough access to reconcile your resources:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  labels:
    rbac.kro.run/aggregate-to-controller: "true"
  name: kro:controller:foos
rules:
  - apiGroups:
      - kro.run
    resources:
      - foos
    verbs:
      - "*"
  - apiGroups:
      - apps
    resources:
      - deployments
    verbs:
      - "*"
  - apiGroups:
      - ""
    resources:
      - configmaps
    verbs:
      - "*"
```

## `Graph` and ServiceAccount impersonation

:::warning
Granting a user permission to create or update `Graph` resources in a namespace
effectively lets them act as **any ServiceAccount in that namespace that kro is
allowed to impersonate**. Read this section before enabling `Graph` in a
multi-tenant or shared cluster.
:::

Unlike a cluster-scoped `ResourceGraphDefinition`, a `Graph` is a **namespaced,
user-creatable** kind that directly describes cluster resources. To keep that
power from running as kro's own (broad) controller identity, kro applies a
Graph's resources while **impersonating a ServiceAccount**:

- The identity is `system:serviceaccount:<graph-namespace>:<name>`. The
  ServiceAccount is always resolved in the **Graph's own namespace** — a Graph
  cannot name a ServiceAccount in another namespace.
- `spec.serviceAccountName` selects which ServiceAccount in that namespace to
  use. When it is unset, kro impersonates the namespace's `default`
  ServiceAccount.
- Every apply, patch, and delete the Graph performs is authorized against that
  ServiceAccount's RBAC. A Graph can therefore never do more than a
  ServiceAccount in its namespace is already granted.

### This is the same trust model as `create pod`

This mirrors how Pods work in Kubernetes: anyone who can create a Pod (directly,
or via a Deployment/Job/etc.) can set `spec.serviceAccountName` to any
ServiceAccount in the same namespace and run as it. Being able to create or
update a `Graph` grants the equivalent capability.

So, concretely:

> **Permission to mutate `Graph` in a namespace ⇒ permission to act as any
> ServiceAccount in that namespace that kro can impersonate.**

The **namespace is the trust boundary**. Delete is included, since deleting a
Graph tears its resources down under the same impersonated identity.

### Restricting which ServiceAccounts kro may impersonate

Impersonation only works for ServiceAccounts kro itself is permitted to
impersonate. You control that with kro's **own** RBAC — grant the `impersonate`
verb narrowly instead of cluster-wide:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kro-impersonate-applier
  namespace: team-payments
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    verbs: ["impersonate"]
    # Only these ServiceAccounts in this namespace can back a Graph.
    resourceNames: ["kro-applier"]
```

Bind that `Role` to the kro controller's ServiceAccount per namespace. Any
ServiceAccount kro is _not_ granted `impersonate` on simply cannot be used by a
Graph, so a Graph naming it fails to apply rather than escalating. If kro has no
`impersonate` permission for a namespace at all, Graphs there cannot apply
anything.

### The controller's own identity is refused

The one identity namespace confinement does not naturally protect is kro's own
ServiceAccount. A `Graph` created in kro's namespace that resolves to the
controller's ServiceAccount (by name, or because its `default` is the controller
SA) would otherwise run under kro's broad identity. kro **refuses** such a Graph,
marking it `Accepted=False` (reason `InvalidGraph`) before it applies anything.
Any _other_ privileged ServiceAccount reachable in a namespace remains yours to
scope via the `impersonate` RBAC above.

### Recommendations

- Treat `create`/`update` on `Graph` as a **privileged grant**, on par with
  `create pod` — especially in shared namespaces like `kube-system`.
- Grant kro the `impersonate` verb narrowly (per namespace, with
  `resourceNames`) rather than cluster-wide.
- Keep the namespace `default` ServiceAccount minimally privileged; a Graph that
  does not set `spec.serviceAccountName` runs as it.
