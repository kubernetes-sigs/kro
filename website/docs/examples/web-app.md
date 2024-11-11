---
sidebar_position: 10
---

# Web Application

```yaml title="deploymentservice-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGroup
metadata:
  name: deploymentservice
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
          name: ${schema.spec.name}
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
                - name: ${schema.spec.name}-deployment
                  image: nginx
                  ports:
                    - containerPort: 80
    - name: service
      definition:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
        spec:
          selector:
            app: deployment
          ports:
            - protocol: TCP
              port: 80
              targetPort: 80
```
