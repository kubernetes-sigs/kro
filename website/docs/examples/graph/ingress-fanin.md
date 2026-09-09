---
sidebar_position: 202
---

# Ingress Fan-In

:::warning Alpha Feature
The `Graph` API is alpha and disabled by default. Enabling it requires the
`GraphKind` feature gate and some cluster-level setup; see
[Enabling Graphs](../../docs/concepts/graph/01-overview.md#enabling-graphs).
:::

This Graph watches every Service labeled `expose: "true"` and aggregates them
into a single Ingress with one rule per Service. Label a Service and a route
appears; remove the label and the route disappears. This is the
**aggregated-resource** pattern: many independent contributors, one resulting
object, no coordination between them.

Two details are worth noting:

- The `rules` field is a single CEL expression that maps the `services` list to
  a list of Ingress rules. kro applies the whole list on every reconcile, so the
  Ingress always reflects the current set of labeled Services.
- The `includeWhen` on `ingress` is required. With no matching Service, the
  Graph would render `rules: []`, which the Ingress schema rejects.

## Permissions

The `services` ref lists cluster-wide, so reading Services needs a ClusterRole.
The Ingress is written into the Graph's own namespace, so a namespaced Role is
enough for it.

```yaml title="ingress-fanin-rbac.yaml"
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ingress-fanin
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ingress-fanin
rules:
  - apiGroups: [""]
    resources: [services]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ingress-fanin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ingress-fanin
subjects:
  - kind: ServiceAccount
    name: ingress-fanin
    namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ingress-fanin
  namespace: default
rules:
  - apiGroups: [networking.k8s.io]
    resources: [ingresses]
    verbs: [get, list, watch, create, update, patch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ingress-fanin
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ingress-fanin
subjects:
  - kind: ServiceAccount
    name: ingress-fanin
    namespace: default
```

## Graph

```kro title="ingress-fanin.yaml"
apiVersion: kro.run/v1alpha1
kind: Graph
metadata:
  name: ingress-fanin
  namespace: default
spec:
  serviceAccountName: ingress-fanin
  nodes:
    - id: services
      ref:
        apiVersion: v1
        kind: Service
        metadata:
          selector:
            matchLabels:
              expose: "true"

    - id: ingress
      includeWhen:
        - ${size(services) > 0}
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: shared-ingress
        spec:
          rules: >-
            ${services.map(svc, {
              "host": svc.metadata.name + ".example.com",
              "http": {
                "paths": [{
                  "path": "/",
                  "pathType": "Prefix",
                  "backend": {
                    "service": {
                      "name": svc.metadata.name,
                      "port": {"number": svc.spec.ports[0].port}
                    }
                  }
                }]
              }
            })}
```

## Try It

```bash
kubectl apply -f ingress-fanin-rbac.yaml
kubectl apply -f ingress-fanin.yaml

kubectl create deployment web --image=nginx
kubectl expose deployment web --port=80
kubectl label service web expose=true

kubectl get ingress shared-ingress -o jsonpath='{.spec.rules[*].host}'
# web.example.com
```
