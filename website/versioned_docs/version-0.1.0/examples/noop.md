---
sidebar_position: 5
---

# Empty ResourceGroup

```yaml title="noop.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGroup
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
