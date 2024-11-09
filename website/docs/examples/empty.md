---
sidebar_position: 0
---

# Empty ResourceGroup

```yaml title="no-resources-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGroup
metadata:
  name: kro.run/v1alpha1
spec:
  apiVersion: v1alpha1
  kind: Noop
  definition:
    spec:
      name: string | required=true
  resources: []
```
