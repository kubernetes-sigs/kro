---
sidebar_position: 201
---

# Namespace Decorator

:::warning Alpha Feature
The `Graph` API is alpha and disabled by default. Enabling it requires the
`GraphKind` feature gate and some cluster-level setup; see
[Enabling Graphs](../../docs/concepts/graph/01-overview.md#enabling-graphs).
:::

This Graph watches every Namespace labeled `policy: enforced` and creates a
default-deny NetworkPolicy in each one. There is no CRD, no schema, and no
instance; the Graph reacts to resources that already exist. Add the label to a
Namespace and the policy appears; remove it and the policy is pruned. Delete the
Graph and every NetworkPolicy it created is cleaned up.

The pattern is a **decorator**: a `ref` node with a `selector` reads a
collection, and a `template` node with `forEach` stamps one resource per item.
The iterator `ns` appears in `metadata.namespace`, which is what gives each
NetworkPolicy a distinct identity.

## Permissions

A Graph applies its resources while impersonating a ServiceAccount in its own
namespace. Namespaces are cluster-scoped and the NetworkPolicies land in many
namespaces, so the applier needs a ClusterRole.

```yaml title="namespace-decorator-rbac.yaml"
apiVersion: v1
kind: ServiceAccount
metadata:
  name: namespace-decorator
  namespace: kro-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: namespace-decorator
rules:
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, list, watch]
  - apiGroups: [networking.k8s.io]
    resources: [networkpolicies]
    verbs: [get, list, watch, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: namespace-decorator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: namespace-decorator
subjects:
  - kind: ServiceAccount
    name: namespace-decorator
    namespace: kro-system
```

## Graph

```kro title="namespace-decorator.yaml"
apiVersion: kro.run/v1alpha1
kind: Graph
metadata:
  name: namespace-decorator
  namespace: kro-system
spec:
  serviceAccountName: namespace-decorator
  nodes:
    - id: namespaces
      ref:
        apiVersion: v1
        kind: Namespace
        metadata:
          selector:
            matchLabels:
              policy: enforced

    - id: policies
      forEach:
        - ns: ${namespaces}
      template:
        apiVersion: networking.k8s.io/v1
        kind: NetworkPolicy
        metadata:
          name: default-deny
          namespace: ${ns.metadata.name}
        spec:
          podSelector: {}
          policyTypes:
            - Ingress
            - Egress
```

## Try It

```bash
kubectl apply -f namespace-decorator-rbac.yaml
kubectl apply -f namespace-decorator.yaml

kubectl create namespace team-a
kubectl label namespace team-a policy=enforced

kubectl get networkpolicy -n team-a
# NAME           POD-SELECTOR   AGE
# default-deny   <none>         5s

kubectl label namespace team-a policy-
kubectl get networkpolicy -n team-a
# No resources found in team-a namespace.
```
