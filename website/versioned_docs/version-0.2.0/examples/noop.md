---
sidebar_position: 5
---

# Empty ResourceGraphDefinition

```yaml title="noop.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: kro.run/v1alpha1
spec:
  apiVersion: v1alpha1
  kind: NoOp
  definition:
    spec:
      name: string | required=true
  resources: []
```
