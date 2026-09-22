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

| Feature Gate                       | Default | Stage | Since   | Description                                                                             |
| ---------------------------------- | ------- | ----- | ------- | --------------------------------------------------------------------------------------- |
| `CELOmitFunction`                  | `false` | Alpha | v0.9.0  | Enables the `omit()` CEL function for conditional field omission in resource templates. |
| `InstanceConditionEvents`          | `false` | Alpha | v0.9.0  | Emits Kubernetes Events on instance status condition transitions.                       |
| `InstanceConditionMetrics`         | `false` | Alpha | v0.9.3  | Exports per-instance condition status as Prometheus metrics.                            |
| `GraphKind`                        | `false` | Alpha | v0.10.0 | Starts the controller for the `Graph` API (`kro.run/v1alpha1`).                         |
| `ConservativeCRDComparison`        | `false` | Alpha | v0.10.0 | Enables conservative checks for additional and unclassified CRD schema changes.         |

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
          replicas: '${schema.spec.replicas > 0 ? schema.spec.replicas : omit()}'
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

### InstanceConditionMetrics

When enabled, kro exports the `instance_condition_current_status_seconds` metric
described in [Controller Metrics](./04-metrics.md#instance-condition-metrics),
which records how long each instance has held its current status for each
condition type. The metric has one series per instance and condition, so it is
gated to avoid unbounded cardinality on clusters with many instances.

### GraphKind

When enabled, kro starts the controller for the
[`Graph`](../concepts/graph/01-overview.md) API. The gate controls only the
Graph controller; ResourceGraphDefinitions and their instances behave the same
whether it is on or off.

Enabling `GraphKind` has prerequisites beyond setting the gate:

- The `graphs.kro.run` CRD must be installed. Helm does not install CRDs on
  upgrade; see [Upgrading](../upgrading/00-overview.md#custom-resource-definitions).
- In `rbac.mode: aggregation`, the chart grants the controller the `impersonate`
  verb on ServiceAccounts when the gate is on. This is a privileged grant; read
  [Access Control](./01-access-control.md#graph-and-serviceaccount-impersonation).
- In `rbac.mode: aggregation`, the controller also needs `list` and `watch` on
  every resource type your Graphs manage, which the chart cannot grant for you.
  See [What the kro controller itself needs](./01-access-control.md#what-the-kro-controller-itself-needs).
- When kro is deployed without the Helm chart, the controller must be started
  with `--controller-namespace` and `--controller-service-account`. If the gate
  is on and either is missing, the controller exits at startup.

See [Enabling Graphs](../concepts/graph/01-overview.md#enabling-graphs) for the
full procedure.

### CRD compatibility checks

By default, kro checks definitive schema changes such as adding or narrowing an
enum constraint, removing nullable support, and removing unknown-field
preservation.

Enable `ConservativeCRDComparison` to check additional schema facets,
including map value schemas, formats, Kubernetes list and map topology, and CEL
validation rules. The gate also treats changes to schema fields without an
explicit compatibility classification as breaking. This can block compatible
changes, but prevents unsupported schema changes from passing unnoticed.

```yaml
config:
  featureGates:
    ConservativeCRDComparison: true
```

For instructions on applying an intentional breaking schema change, see
[Breaking Changes](../concepts/rgd/01-overview.md#breaking-changes).
