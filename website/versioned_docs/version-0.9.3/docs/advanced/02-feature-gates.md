---
sidebar_position: 2
---

# Feature Gates

kro uses Kubernetes-style
[feature gates](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/)
to manage alpha and experimental functionality. Feature gates let you enable or
disable specific features at controller startup without code changes.

## Configuring Feature Gates

Enable feature gates in your Helm values:

```yaml
config:
  featureGates:
    CELOmitFunction: true
```

Or pass the flag directly to the controller binary:

```bash
--feature-gates=CELOmitFunction=true,InstanceConditionEvents=true
```

## Available Feature Gates

| Feature Gate              | Default | Stage | Since  | Description                                                                             |
| ------------------------- | ------- | ----- | ------ | --------------------------------------------------------------------------------------- |
| `CELOmitFunction`         | `false` | Alpha | v0.9.0 | Enables the `omit()` CEL function for conditional field omission in resource templates. |
| `InstanceConditionEvents` | `false` | Alpha | v0.9.0 | Emits Kubernetes Events on instance status condition transitions.                       |

### CELOmitFunction

When enabled, CEL expressions in resource templates can return `omit()` to
remove the containing field from the rendered object instead of writing a value.
This is useful for CRDs that distinguish between field absence and an explicit
null or empty value.

```yaml
spec:
  resources:
    - id: deployment
      template:
        spec:
          replicas: ${schema.spec.replicas > 0 ? schema.spec.replicas : omit()}
```

Since kro uses Server-Side Apply, an omitted field is no longer managed by kro.
Any external changes to that field will not be detected or reverted.

When this gate is disabled, any RGD that uses `omit()` is rejected at build
time.

### InstanceConditionEvents

When enabled, kro emits Kubernetes Events on the instance object whenever a
status condition transitions (e.g. `ResourcesReady` from `False` to `True`).
These events are visible via `kubectl describe` and can be used for alerting or
debugging.
