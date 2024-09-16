---
sidebar_position: 0
---

# Empty ResourceGroup

```yaml title="no-resources-rg.yaml"
apiVersion: x.symphony.k8s.aws/v1alpha1
kind: ResourceGroup
metadata:
  name: noop.x.symphony.k8s.aws
spec:
  apiVersion: v1alpha1
  kind: Noop
  definition:
    spec:
      name: string | required=true
  resources: []
```