---
sidebar_position: 104
---

# Optional Values & External References


```yaml title="config-map.yaml"
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  ECHO_VALUE: "Hello, World!"
```


```yaml title="deploymentservice-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: deploymentservice
spec:
  schema:
    apiVersion: v1alpha1
    kind: DeploymentService
    spec:
      name: string
  resources:
    - id: input
      externalRef:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: demo
          namespace: default
    - id: deployment
      template:
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
                - name: ${schema.spec.name}-busybox
                  image: busybox
                  command: ["sh", "-c", "echo $MY_VALUE && sleep 3600"]
                  env:
                    - name: MY_VALUE
                      value: ${input.data.?ECHO_VALUE}
```