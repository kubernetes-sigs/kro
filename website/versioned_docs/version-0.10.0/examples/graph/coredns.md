---
sidebar_position: 203
---

# CoreDNS Bundle

:::warning Alpha Feature
The `Graph` API is alpha and disabled by default. Enabling it requires the
`GraphKind` feature gate and some cluster-level setup; see
[Enabling Graphs](../../docs/concepts/graph/01-overview.md#enabling-graphs).
:::

This Graph installs CoreDNS as a single object: a ServiceAccount, ClusterRole,
ClusterRoleBinding, ConfigMap, Deployment, and Service. This is the
**static-bundle** pattern: a fixed set of manifests managed as one object.
Nothing here uses `forEach` or a `ref`; the value is in the ordering, the
health check, and the single lifecycle.

kro infers the apply order from the CEL references between nodes:

```text
sa → role → binding → corefile → deployment → service
```

The `readyWhen` on `deployment` gates the Graph's `Ready` condition on the
Deployment reporting all replicas available. Deleting the Graph removes the whole
stack in reverse order.

:::danger Adopts an existing CoreDNS
The Service (`kube-dns`, `10.96.0.10`) and Deployment (`coredns`) use the
standard cluster-DNS names. On a cluster that already runs CoreDNS, applying this
Graph adopts and overwrites it. Change the names and namespace to install a
second, independent instance instead.
:::

## Permissions

The Graph creates a ClusterRole and ClusterRoleBinding, so its applier
ServiceAccount must be allowed to grant those permissions. That requires the
`escalate` and `bind` verbs, or already holding every permission being granted.
For brevity this example binds the installer to `cluster-admin`; scope it down
in production.

```yaml title="coredns-rbac.yaml"
apiVersion: v1
kind: ServiceAccount
metadata:
  name: coredns-installer
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: coredns-installer
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: coredns-installer
    namespace: kube-system
```

## Graph

```kro title="coredns.yaml"
apiVersion: kro.run/v1alpha1
kind: Graph
metadata:
  name: coredns
  namespace: kube-system
spec:
  serviceAccountName: coredns-installer
  nodes:
    - id: sa
      template:
        apiVersion: v1
        kind: ServiceAccount
        metadata:
          name: coredns

    - id: role
      template:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: system:coredns
        rules:
          - apiGroups: [""]
            resources: [endpoints, services, pods, namespaces]
            verbs: [list, watch]
          - apiGroups: [discovery.k8s.io]
            resources: [endpointslices]
            verbs: [list, watch]

    - id: binding
      template:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRoleBinding
        metadata:
          name: system:coredns
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: ${role.metadata.name}
        subjects:
          - kind: ServiceAccount
            name: ${sa.metadata.name}
            namespace: kube-system

    - id: corefile
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: coredns
        data:
          Corefile: |
            .:53 {
                errors
                health {
                    lameduck 5s
                }
                ready
                kubernetes cluster.local in-addr.arpa ip6.arpa {
                    pods insecure
                    fallthrough in-addr.arpa ip6.arpa
                    ttl 30
                }
                prometheus :9153
                forward . /etc/resolv.conf {
                    max_concurrent 1000
                }
                cache 30
                loop
                reload
                loadbalance
            }

    - id: deployment
      readyWhen:
        - ${deployment.status.availableReplicas == deployment.spec.replicas}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: coredns
          labels:
            k8s-app: kube-dns
        spec:
          replicas: 2
          selector:
            matchLabels:
              k8s-app: kube-dns
          template:
            metadata:
              labels:
                k8s-app: kube-dns
            spec:
              serviceAccountName: ${sa.metadata.name}
              containers:
                - name: coredns
                  image: registry.k8s.io/coredns/coredns:v1.11.1
                  args: ["-conf", "/etc/coredns/Corefile"]
                  ports:
                    - containerPort: 53
                      name: dns
                      protocol: UDP
                    - containerPort: 53
                      name: dns-tcp
                      protocol: TCP
                    - containerPort: 9153
                      name: metrics
                      protocol: TCP
                  volumeMounts:
                    - name: config
                      mountPath: /etc/coredns
                      readOnly: true
                  readinessProbe:
                    httpGet:
                      path: /ready
                      port: 8181
                  livenessProbe:
                    httpGet:
                      path: /health
                      port: 8080
              volumes:
                - name: config
                  configMap:
                    name: ${corefile.metadata.name}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: kube-dns
          labels:
            k8s-app: kube-dns
        spec:
          clusterIP: 10.96.0.10
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - name: dns
              port: 53
              protocol: UDP
            - name: dns-tcp
              port: 53
              protocol: TCP
            - name: metrics
              port: 9153
              protocol: TCP
```
