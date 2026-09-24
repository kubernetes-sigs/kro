---
sidebar_position: 6
---

# Instances

Once kro processes your ResourceGraphDefinition, it creates a new API in your cluster. Users create instances of this API to deploy resources in a consistent, controlled way.

## What is an Instance?

An instance represents your deployed application. When you create an instance, you provide configuration values and kro deploys all the resources defined in your ResourceGraphDefinition. The instance serves as the single source of truth for your application's desired state.

Here's an example instance:

```yaml
apiVersion: kro.run/v1alpha1
kind: WebApplication
metadata:
  name: my-app
spec:
  name: web-app
  image: nginx:latest
  ingress:
    enabled: true
```

When you create this instance, kro:
- Creates all required resources (Deployment, Service, Ingress)
- Configures them according to your specification
- Manages them as a single unit
- Keeps their status up to date

## How kro Manages Instances

kro uses the standard Kubernetes reconciliation pattern to manage instances:

1. **Observe** - Watches for changes to your instance or its resources
2. **Compare** - Checks if current state matches desired state
3. **Act** - Creates, updates, or deletes resources as needed
4. **Report** - Updates status to reflect current state

This continuous loop ensures your resources stay in sync with your desired state, providing:
- Self-healing if resources are modified or deleted
- Automatic updates when you change the instance
- Consistent state management
- Rich status tracking

:::tip
To suspend active reconciliation of an instance for debugging purposes, apply an annotation with the key `kro.run/reconcile` and the value `suspended`. See [Debugging Specific Labels](./06-instances.md#labels-and-ownership) for more details
:::

### Reactive Reconciliation

kro automatically watches all resources managed by an instance and triggers reconciliation when any of them change:

- **Child Resource Changes** - When a managed resource (like a Deployment or Service) is modified, kro detects the change and reconciles the instance to ensure it matches the desired state.
- **Drift Detection** - If a resource is manually modified or deleted, kro detects the drift and automatically restores it to the desired state.
- **Dependency Updates** - Changes to resources propagate through the dependency graph, ensuring all dependent resources are updated accordingly.

This reactive behavior ensures your instances maintain consistency without requiring manual intervention.

## Labels and Ownership

kro applies labels and annotations to track ownership and enable resource discovery. There are two sets of metadata: one for instances themselves, and one for the resources kro creates on behalf of instances.

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

<Tabs>
<TabItem value="instance" label="Instance Metadata" default>

When kro manages an instance, it applies these labels and annotations:

**Labels:**

| Label | Description |
|-------|-------------|
| `kro.run/owned` | Set to `"true"` to indicate kro is managing this instance |
| `kro.run/kro-version` | Version of kro managing the instance |
| `kro.run/resource-graph-definition-id` | UID of the ResourceGraphDefinition |
| `kro.run/resource-graph-definition-name` | Name of the ResourceGraphDefinition |
| `applyset.kubernetes.io/id` | Unique ApplySet identifier (hash of name.namespace.kind.group) |

**Annotations:**

| Annotation | Description |
|------------|-------------|
| `applyset.kubernetes.io/tooling` | Identifies kro as the managing tool (format: `kro/<version>`) |
| `applyset.kubernetes.io/contains-group-kinds` | Comma-separated list of GroupKinds managed by this instance |
| `applyset.kubernetes.io/additional-namespaces` | Comma-separated list of namespaces containing managed resources |

**Debugging-specific annotations:** 

| Annotation | Description |
|------|-------------|
| `kro.run/reconcile` | Set the value to `suspended` to suspend reconciliation of the instance. Only to be used when manually debugging |

</TabItem>
<TabItem value="managed" label="Managed Resource Metadata">

Resources created by kro (Deployments, Services, ConfigMaps, etc.) receive labels for ownership tracking and resource discovery. These labels are distinct from the instance's own labels — notably, child resources do **not** carry `resource-graph-definition-*` labels since ownership is tracked via the [ApplySet specification](https://git.k8s.io/enhancements/keps/sig-cli/3659-kubectl-apply-prune).

**Labels:**

| Label | Description |
|-------|-------------|
| `kro.run/owned` | Set to `"true"` to indicate kro manages this resource |
| `kro.run/kro-version` | Version of kro managing the resource |
| `kro.run/instance-id` | UID of the instance that created this resource |
| `kro.run/instance-name` | Name of the instance (see [Long instance names](#long-instance-names)) |
| `kro.run/instance-namespace` | Namespace of the instance (only for namespaced instances) |
| `kro.run/instance-group` | API group of the instance (see [Long instance names](#long-instance-names)) |
| `kro.run/instance-version` | API version of the instance |
| `kro.run/instance-kind` | Kind of the instance |
| `app.kubernetes.io/managed-by` | Set to `"kro"` |
| `kro.run/node-id` | Label-safe token for the resource ID from the RGD (see [The node-id label](#the-node-id-label)) |
| `applyset.kubernetes.io/part-of` | Links the resource to its parent instance (matches the instance's `applyset.kubernetes.io/id`) |

**Collection-specific labels** (only on resources created via `forEach`):

| Label | Description |
|-------|-------------|
| `kro.run/collection-index` | Position in the collection (0-indexed) |
| `kro.run/collection-size` | Total number of items in the collection |

**Annotations:**

| Annotation | Description |
|------------|-------------|
| `internal.kro.run/node-path` | Full resource ID from the RGD, never truncated or hashed |
| `internal.kro.run/instance-name` | Full instance name, only when `kro.run/instance-name` was hashed |
| `internal.kro.run/instance-group` | Full API group, only when `kro.run/instance-group` was hashed |

These labels allow you to identify exactly which instance owns each managed resource, which is essential when multiple instances of the same RGD exist in a cluster. For collection resources, see [Collection Labels](../reconciliation/03-collections.md#collection-labels) for more details.

</TabItem>
</Tabs>

### The node-id label

`kro.run/node-id` identifies which resource in the RGD produced a managed
resource. For a top-level resource it has the resource ID as authored:

```bash
kubectl get pods -l kro.run/node-id=frontend
```

For a resource inside a subgraph it has the qualified path with `.`
separators, so resource `res` in subgraph `subA` is `subA.res`:

```bash
kubectl get pods -l kro.run/node-id=subA.res
```

A label value is limited at 63 characters. When that rendering is longer, kro
substitutes a stable SHA-256 hash of the path prefixed with `h-`, for example
`h-9c1185a5c5e9fc54612808977ee8f548b2258d31`. The hash is deterministic, so a
selector built from it keeps matching across reconciles.

The full path value, `/`-separated and never hashed, is always in the
`internal.kro.run/node-path` annotation:

```bash
kubectl get pod <name> -o jsonpath='{.metadata.annotations.internal\.kro\.run/node-path}'
```

### Long instance names

RGD authors can constrain accepted instance names with
[`schema.metadata.nameValidation`](./02-schema.md#validating-instance-names).

The same 63 character cap applies to the instance identity kro copies onto
every managed resource. Names and API groups are DNS subdomains and may run to
253 characters, so `kro.run/instance-name` and `kro.run/instance-group` hold
the value verbatim at 63 characters or fewer, and an `h-`-prefixed hash beyond
that. The full value is available on the same named annotation:

```bash
kubectl get deployment <name> -o jsonpath='{.metadata.annotations.internal\.kro\.run/instance-name}'
```

:::tip
`kro.run/instance-id` holds the instance UID, which always fits in a label
value and is never hashed. Prefer it to select one specific instance:

```bash
kubectl get <resource> -l kro.run/instance-id=<uid>
```
:::

:::info ApplySet Specification
kro uses the [Kubernetes ApplySet specification](https://git.k8s.io/enhancements/keps/sig-cli/3659-kubectl-apply-prune) for tracking and pruning managed resources. This enables kro to automatically prune resources that are no longer part of the instance's resource graph and prevents other tools from accidentally modifying kro-managed resources.
:::

### Owner References

kro does not set Kubernetes owner references on managed resources by default. This is intentional for two reasons:

1. **Ordered deletion** - kro deletes resources in reverse topological order, respecting dependencies between resources. Owner references trigger Kubernetes garbage collection, which deletes resources without ordering guarantees.

2. **Cross-scope limitations** - Namespaced resources cannot own cluster-scoped resources. When a namespaced kro instance manages cluster-scoped resources (like ClusterRoles or Namespaces), owner references cannot express this relationship.

Instead, kro uses labels and the ApplySet specification for ownership tracking and resource cleanup.

:::warning Manual Owner References
If you need owner references for specific use cases (like integration with Argo CD or other tools that rely on them), you can manually set them in your resource templates:

```kro
resources:
  - id: configmap
    template:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: ${schema.spec.name}-config
        ownerReferences:
          - apiVersion: ${schema.apiVersion}
            kind: ${schema.kind}
            name: ${schema.metadata.name}
            uid: ${schema.metadata.uid}
            controller: true
            blockOwnerDeletion: true
```

**Use at your own risk.** Manual owner references may cause unexpected behavior:
- Resources may be garbage collected out of order
- Cross-namespace or cluster-scoped resources will fail validation
- Deletion timing may conflict with kro's reconciliation
:::

## Monitoring Your Instances

kro provides rich status information for every instance:

```bash
$ kubectl get webapplication my-app
NAME     STATE    READY   AGE
my-app   ACTIVE   True    30s
```

For detailed status, check the instance's YAML:

```yaml
status:
  state: ACTIVE  # High-level instance state
  availableReplicas: 3  # Status from your resources
  conditions:  # Detailed status conditions
    - type: InstanceManaged
      status: "True"
      reason: Managed
      message: instance is properly managed with finalizers and labels
      lastTransitionTime: "2025-08-08T00:03:46Z"
      observedGeneration: 1
    - type: GraphResolved
      status: "True"
      reason: Resolved
      message: runtime graph created and all resources resolved
      lastTransitionTime: "2025-08-08T00:03:46Z"
      observedGeneration: 1
    - type: ResourcesReady
      status: "True"
      reason: AllResourcesReady
      message: all resources are created and ready
      lastTransitionTime: "2025-08-08T00:03:46Z"
      observedGeneration: 1
    - type: Ready
      status: "True"
      reason: Ready
      message: ""
      lastTransitionTime: "2025-08-08T00:03:46Z"
      observedGeneration: 1
```

## Understanding Status

Every instance includes three types of status information:

### 1. State

High-level status showing what the instance is doing:

- `ACTIVE` - Instance is successfully running and active
- `IN_PROGRESS` - Instance is currently being processed or reconciled
- `DELETING` - Instance is being deleted
- `ERROR` - An error occurred during processing

### 2. Conditions

Detailed status information structured hierarchically. kro provides a top-level `Ready` condition that reflects overall instance health, supported by three sub-conditions that track different phases:

- **`InstanceManaged`** - Instance finalizers and labels are properly set
  - Ensures the instance is under kro's management
  - Tracks whether cleanup handlers (finalizers) are configured
  - Confirms instance is labeled with ownership and version information

- **`GraphResolved`** - Runtime graph has been created and resources resolved
  - Validates that the resource graph has been successfully parsed
  - Confirms all resource templates have been resolved
  - Ensures dependencies between resources are properly understood

- **`ResourcesReady`** - All resources in the graph are created and ready
  - Tracks the creation and readiness of all managed resources
  - Monitors the health of resources in topological order
  - Reports when all resources have reached their ready state

- **`Ready`** - Instance is fully operational (top-level condition)
  - Aggregates the state of all sub-conditions
  - Only becomes True when all sub-conditions are True
  - **The primary condition to monitor for instance health**
  - Use this condition in automation, CI/CD, and health checks


:::tip
Always use the `Ready` condition to determine instance health. The sub-conditions (`InstanceManaged`, `GraphResolved`, `ResourcesReady`) are provided for debugging purposes and may change in future versions. kro reserves the right to add, remove, or modify sub-conditions without breaking compatibility as long as the `Ready` condition behavior remains stable.
:::

Each condition includes:
- `observedGeneration` - Tracks which generation of the instance this condition reflects
- `lastTransitionTime` - When the condition last changed state
- `reason` - A programmatic identifier for the condition state
- `message` - A human-readable description of the current state

### 3. Resource Status

Values you defined in your ResourceGraphDefinition's status section, automatically updated as resources change.

## Debugging Instance Issues

When an instance is not in the expected state, the condition hierarchy helps you quickly identify where the problem occurred:

**1. Check the Ready condition first:**

```bash
kubectl get <your-kind> <instance-name> -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```

**2. If Ready is False, check the sub-conditions** to identify which phase failed:

- **InstanceManaged is False** - Check if there are issues with finalizers or instance labels
- **GraphResolved is False** - The resource graph could not be created - check the ResourceGraphDefinition for syntax errors or invalid CEL expressions
- **ResourcesReady is False** - One or more managed resources failed to become ready - check the error message for which resource failed

**3. Use kubectl describe** to see all conditions and recent events:

```bash
kubectl describe <your-kind> <instance-name>
```

**4. Check the observedGeneration field** in conditions:

- If `observedGeneration` is less than `metadata.generation`, the controller hasn't processed the latest changes yet
- If they match, the conditions reflect the current state of your instance

**5. Check that all fields in the instance are specified**

If any of the resources in your ResourceGraphDefinition reference any fields in `schema.spec`, they must have a value. By referencing them, you are declaring a hard dependency that the fields must exist.

For example:
```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: optional
spec:
  schema:
    apiVersion: v1alpha1
    kind: Optional
    spec:
      # Implicit optional field.
      str: string
  resources:
    - id: testConfigMap
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.metadata.name}-config
          namespace: ${schema.metadata.namespace}
        data:
          str: ${schema.spec.str}
---
apiVersion: kro.run/v1alpha1
kind: Optional
metadata:
  name: test-silent-failure
  namespace: default
spec:
  # str unspecified
```

The `Optional` instance here *must* specify `str` in the Instance, otherwise reconciliation will fail.

For truly optional fields you need to use optional field access: `${schema.spec.?<FIELD>.orValue("fallback")}`. For more information, see [The Optional Operator](../reconciliation/02-readiness.md#the-optional-operator-)
