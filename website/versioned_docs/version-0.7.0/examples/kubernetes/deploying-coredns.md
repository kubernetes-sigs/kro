---
sidebar_position: 10
---

# CoreDNS Deployment

```yaml title="coredns-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: coredns.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: CoreDNSDeployment
    spec:
      name: string | default=mycoredns
      namespace: string | default=default
      values:
        clusterRole:
          labels: 'map[string]string | default={"eks.amazonaws.com/component": "coredns", "k8s-app": "kube-dns", "kubernetes.io/bootstrapping": "rbac-defaults"}'
        clusterRoleBinding:
          annotations: 'map[string]string | default={"rbac.authorization.kubernetes.io/autoupdate": "\"true\""}'
        configMap:
          labels: 'map[string]string | default={"eks.amazonaws.com/component": "coredns", "k8s-app": "kube-dns"}'
        deployment:
          annotations: 'map[string]string | default={"deployment.kubernetes.io/revision": "\"1\""}'
          labels: 'map[string]string | default={"eks.amazonaws.com/component": "coredns", "k8s-app": "kube-dns", "kubernetes.io/name": "CoreDNS"}'
          replicas: integer | default=2
        image:
          repository: string | default=coredns/coredns
          tag: string | default=1.11.3
        resources:
          limits:
            cpu: string | default=100m
            memory: string | default=128Mi
          requests:
            cpu: string | default=100m
            memory: string | default=128Mi
        service:
          annotations: 'map[string]string | default={"prometheus.io/port": "9153", "prometheus.io/scrape": "true"}'
          labels: 'map[string]string | default={"eks.amazonaws.com/component": "kube-dns", "k8s-app": "kube-dns", "kubernetes.io/cluster-service": "true", "kubernetes.io/name": "CoreDNS"}'
          clusterIP: string | default=10.100.123.45
          clusterIPs: '[]string | default=["10.100.123.45"]'
          ipFamilies: '[]string | default=["IPv4"]'
          type: string | default=ClusterIP
        serviceAccount:
          secrets: 'map[string]string | default={"name": "coredns-token-pvcnf"}'
  resources:
  - id: clusterRole
    template:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: ClusterRole
      metadata:
        name: ${schema.spec.name}
        labels: ${schema.spec.values.clusterRole.labels}
      rules:
      - apiGroups:
        - ""
        resources:
        - endpoints
        - services
        - pods
        - namespaces
        verbs:
        - list
        - watch
      - apiGroups:
        - discovery.k8s.io
        resources:
        - endpointslices
        verbs:
        - list
        - watch
      - apiGroups:
        - ""
        resources:
        - nodes
        verbs:
        - get
  - id: clusterRoleBinding
    template:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: ClusterRoleBinding
      metadata:
        name: ${schema.spec.name}
        labels: ${schema.spec.values.clusterRole.labels}
        annotations: ${schema.spec.values.clusterRoleBinding.annotations}
      roleRef:
        apiGroup: rbac.authorization.k8s.io
        kind: ClusterRole
        name: ${clusterRole.metadata.name}
      subjects:
      - kind: ServiceAccount
        name: ${serviceAccount.metadata.name}
        namespace: ${serviceAccount.metadata.namespace}
  - id: configMap
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: ${schema.spec.name}
        labels: ${schema.spec.values.configMap.labels}
      data:
        Corefile: |-
          .:53 {
              errors
              health
              kubernetes cluster.local in-addr.arpa ip6.arpa {
                pods insecure
                fallthrough in-addr.arpa ip6.arpa
              }
              prometheus :9153
              forward . /etc/resolv.conf
              cache 30
              loop
              reload
              loadbalance
          }
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        annotations: ${schema.spec.values.deployment.annotations}
        labels: ${schema.spec.values.deployment.labels}
        name: ${schema.spec.name}
      spec:
        replicas: ${schema.spec.values.deployment.replicas}
        selector:
          matchLabels: ${schema.spec.values.configMap.labels}
        template:
          metadata:
            labels: ${schema.spec.values.configMap.labels}
          spec:
            serviceAccountName: ${serviceAccount.metadata.name}
            containers:
            - name: "coredns"
              image: ${schema.spec.values.image.repository}:${schema.spec.values.image.tag}
              args: ["-conf", "/etc/coredns/Corefile"]
              resources: ${schema.spec.values.resources}
              volumeMounts:
              - name: config-volume
                mountPath: /etc/coredns
            volumes:
              - name: config-volume
                configMap:
                  name: ${schema.spec.name}
                  items:
                  - key: Corefile
                    path: Corefile
  - id: service
    template:
      apiVersion: v1
      kind: Service
      metadata:
        name: ${schema.spec.name}
        labels: ${schema.spec.values.service.labels}
        annotations: ${schema.spec.values.service.annotations}
      spec:
        selector:
          k8s-app: kube-dns
        clusterIP: ${schema.spec.values.service.clusterIP}
        clusterIPs: ${schema.spec.values.service.clusterIPs}
        internalTrafficPolicy: Cluster
        ipFamilies: ${schema.spec.values.service.ipFamilies}
        ports:
        - name: dns
          port: 53
          protocol: UDP
          targetPort: 53
        - name: dns-tcp
          port: 53
          protocol: TCP
          targetPort: 53
        selector:
         k8s-app: kube-dns
        sessionAffinity: None
  - id: serviceAccount
    template:
      apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: ${schema.spec.name}
        namespace: ${schema.spec.namespace}
        labels: ${schema.spec.values.configMap.labels}
      secrets:
      - ${schema.spec.values.serviceAccount.secrets}
```
