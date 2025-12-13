---
sidebar_position: 101
---

# Empty ResourceGraphDefinition

```yaml title="noop.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: noop
spec:
  schema:
    apiVersion: v1alpha1
    kind: NoOp
    spec:
      name: string | required=true
  resources: []
```
