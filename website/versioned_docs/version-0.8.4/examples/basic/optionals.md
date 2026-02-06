---
sidebar_position: 104
---

# Optional Values & External References

## Config Map

This example shows how to reference an existing ConfigMap and use the optional accessor `?` to safely extract values.

```kro title="config-map.yaml"
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo
data:
  ECHO_VALUE: "Hello, World!"
```


```kro title="deploymentservice-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: deploymentservice
spec:
  schema:
    apiVersion: kro.run/v1alpha1
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

## Secret

This example demonstrates referencing an existing Secret and transforming its base64-encoded data using CEL expressions with the optional accessor and base64 decoding functions.

```kro title="secret.yaml"
apiVersion: v1
kind: Secret
metadata:
  name: test
stringData:
  uri: api.test.com
```

```kro title="secret-transformation-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: secret-transformation
spec:
  schema:
    apiVersion: kro.run/v1alpha1
    kind: test
    spec:
      name: string     
  resources:
    - id: test
      externalRef:
        apiVersion: v1
        kind: Secret
        metadata:
          name: test
          namespace: ""
    - id: secret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: "${schema.spec.name}"          
        stringData:
          token: "${ string(base64.decode(string(test.data.uri))) }/oauth/token"
```
