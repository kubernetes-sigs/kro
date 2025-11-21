---
sidebar_position: 104
---

# Optional Values & External References

## Config Map
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

## Secret

```yaml title="secret.yaml"
apiVersion: v1
kind: Secret
metadata:
  name: test
  namespace: default
stringData:
  uri: api.test.com
```

```yaml title="secret-transformation-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: secret-transformation
spec:
  schema:
    apiVersion: v1alpha1
    kind: test
    spec:
      prefix: string | default="test"
      namespace: string | default="default"
    status:
      creationTimestamp: ${secret.metadata.creationTimestamp}
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
          name: "${schema.spec.prefix}-transformed"
          namespace: ${schema.spec.namespace}
        stringData:
          Token-url: "${ string(base64.decode(string(test.data.uri))) }/oauth/token"
```