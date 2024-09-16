---
sidebar_position: 1
---

# DeploymentService

```yaml title="deploymentservice-rg.yaml"
apiVersion: x.symphony.k8s.aws/v1alpha1
kind: ResourceGroup
metadata:
  name: deploymentservice.x.symphony.k8s.aws
spec:
  apiVersion: v1alpha1
  kind: DeploymentService
  definition:
    spec:
      name: string
  resources:
  - name: deployment
    definition:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${spec.name}
      spec:
        replicas: 1
        selector:
          matchLabels:
            app: deployment
        template:
          metadata:
            labels:
              app: deployment
          spec:
            containers:
            - name: ${spec.name}-deployment
              image: nginx
              ports:
              - containerPort: 80
  - name: service
    definition:
      apiVersion: v1
      kind: Service
      metadata:
        name: ${spec.name}
      spec:
        selector:
          app: deployment
        ports:
        - protocol: TCP
          port: 80
          targetPort: 80
```