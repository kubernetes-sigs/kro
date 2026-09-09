---
sidebar_position: 204
---

# Singleton Controller

:::warning Alpha Feature
The `Graph` API is alpha and disabled by default. Enabling it requires the
`GraphKind` feature gate and some cluster-level setup; see
[Enabling Graphs](../../docs/concepts/graph/01-overview.md#enabling-graphs).
:::

This example implements a small controller entirely as a Graph. A `Singleton`
custom resource declares a target that should exist exactly once, with a
`priority`. When several Singletons claim the same target (same kind, namespace,
and name), the one with the highest priority wins, ties broken by creation
time. If the winner is deleted, the next claimant takes over without the target
ever being deleted or recreated.

The Graph exercises most of the node kinds together:

- `singletons` is a `ref` with an empty selector, reading every Singleton in the
  cluster.
- `claims` is a `def` with `forEach`, flattening each Singleton into the fields
  the rest of the Graph needs and computing an `identity` string for the target.
- `targets` is a `template` with `forEach` and a **dynamic type**: its
  `apiVersion`, `kind`, `name`, and `namespace` all come from the winning
  claim's template.
- `singletonStatus` is a `patch` with `forEach`, writing `status.active` and
  `status.claim` back onto every Singleton so each claimant can see whether it
  won.

## The CRD

The Singleton CRD is a plain manifest rather than a `template` node, for two
reasons: a `template` would make the Graph the owner of the CRD, so deleting the
Graph would cascade-delete every Singleton; and the `singletons` ref needs the
CRD's schema to be registered before the Graph can compile. Applying the CRD
first, out of band, avoids both.

```yaml title="singleton-crd.yaml"
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: singletons.kro.run
spec:
  group: kro.run
  names:
    plural: singletons
    singular: singleton
    kind: Singleton
    listKind: SingletonList
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                priority:
                  type: integer
                  default: 0
                template:
                  type: object
                  x-kubernetes-preserve-unknown-fields: true
            status:
              type: object
              properties:
                active:
                  type: boolean
                claim:
                  type: string
      additionalPrinterColumns:
        - name: Claim
          type: string
          jsonPath: .status.claim
        - name: Priority
          type: integer
          jsonPath: .spec.priority
        - name: Active
          type: boolean
          jsonPath: .status.active
        - name: Age
          type: date
          jsonPath: .metadata.creationTimestamp
```

## Permissions

The applier reads Singletons cluster-wide, patches their status, and owns the
target resources in whatever namespace each claim names.

```yaml title="singleton-rbac.yaml"
apiVersion: v1
kind: ServiceAccount
metadata:
  name: singleton-controller
  namespace: kro-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: singleton-controller
rules:
  - apiGroups: [kro.run]
    resources: [singletons, singletons/status]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get, list, watch, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: singleton-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: singleton-controller
subjects:
  - kind: ServiceAccount
    name: singleton-controller
    namespace: kro-system
```

## Graph

```kro title="singleton.yaml"
apiVersion: kro.run/v1alpha1
kind: Graph
metadata:
  name: singleton
  namespace: kro-system
spec:
  serviceAccountName: singleton-controller
  nodes:
    # Watch all Singleton CRs (CRD created out-of-band above).
    - id: singletons
      ref:
        apiVersion: kro.run/v1alpha1
        kind: Singleton
        metadata:
          selector: {}

    # Flatten each singleton into {key, name, namespace, priority, creationTimestamp, identity, template}.
    # key = namespace/name composite for cross-namespace uniqueness.
    - id: claims
      forEach:
        - s: ${singletons}
      def:
        key: ${s.metadata.namespace + "/" + s.metadata.name}
        name: ${s.metadata.name}
        namespace: ${s.metadata.namespace}
        priority: ${s.spec.priority}
        creationTimestamp: ${s.metadata.creationTimestamp}
        template: ${s.spec.template}
        identity: >-
          ${s.spec.template.apiVersion
            + (has(s.spec.template.metadata.namespace)
               ? "/namespaces/" + s.spec.template.metadata.namespace
               : "")
            + "/" + s.spec.template.kind
            + "/" + s.spec.template.metadata.name}

    # Apply one target resource per unique identity, using the winner's template.
    # A claim is the winner if its key matches the first entry after sorting
    # by priority desc, creationTimestamp asc.
    - id: targets
      forEach:
        - c: >-
            ${claims.filter(c,
              c.key == claims.filter(x, x.identity == c.identity)
                .sortBy(x, x.creationTimestamp)
                .sortBy(x, -x.priority)[0].key)}
      template:
        apiVersion: ${c.template.apiVersion}
        kind: ${c.template.kind}
        metadata:
          name: ${c.template.metadata.name}
          namespace: ${c.template.metadata.namespace}
        data: ${c.template.data}

    # Status writeback — fan out to EVERY claimant via a forEach patch. Each
    # Singleton CR receives its own status: active=true only for the winner of
    # its target identity, claim=<the contested identity>. An empty claim list
    # renders zero patches (guarded by includeWhen). The patch targets each
    # claimant's own CR by name, so drift re-enqueues via the `singletons` ref.
    - id: singletonStatus
      forEach:
        - c: ${claims}
      includeWhen:
        - ${size(claims) > 0}
      patch:
        apiVersion: kro.run/v1alpha1
        kind: Singleton
        metadata:
          name: ${c.name}
          namespace: ${c.namespace}
        status:
          active: >-
            ${c.key == claims.filter(x, x.identity == c.identity)
              .sortBy(x, x.creationTimestamp)
              .sortBy(x, -x.priority)[0].key}
          claim: "${c.identity}"
```

## Try It

```bash
kubectl apply -f singleton-crd.yaml
kubectl apply -f singleton-rbac.yaml
kubectl apply -f singleton.yaml

cat <<EOF | kubectl apply -f -
apiVersion: kro.run/v1alpha1
kind: Singleton
metadata:
  name: low
  namespace: default
spec:
  priority: 10
  template:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shared
      namespace: default
    data:
      owner: low
---
apiVersion: kro.run/v1alpha1
kind: Singleton
metadata:
  name: high
  namespace: default
spec:
  priority: 20
  template:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shared
      namespace: default
    data:
      owner: high
EOF

kubectl get configmap shared -o jsonpath='{.data.owner}'
# high

kubectl delete singleton high
kubectl get configmap shared -o jsonpath='{.data.owner}'
# low
```
