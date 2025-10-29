---
sidebar_position: 20
---

# Access Control

There are currently two modes of access control supported by **kro**, if you
[install through the Helm chart](../getting-started/01-Installation.md#install-kro-using-helm):

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

In the `aggreagation` access mode, the chart includes an [_aggregated_ `ClusterRole`](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#aggregated-clusterroles)
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
