---
sidebar_position: 15
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
| `app.kubernetes.io/managed-by` | Set to `"kro"` (standard Kubernetes recommended label) |
| `applyset.kubernetes.io/id` | Unique ApplySet identifier (hash of name.namespace.kind.group) |

**Annotations:**

| Annotation | Description |
|------------|-------------|
| `applyset.kubernetes.io/tooling` | Identifies kro as the managing tool (format: `kro/<version>`) |
| `applyset.kubernetes.io/contains-group-kinds` | Comma-separated list of GroupKinds managed by this instance |
| `applyset.kubernetes.io/additional-namespaces` | Comma-separated list of namespaces containing managed resources |

</TabItem>
<TabItem value="managed" label="Managed Resource Metadata">

Resources created by kro (Deployments, Services, ConfigMaps, etc.) receive all the instance labels, plus additional labels to trace back to the specific instance:

**Labels (in addition to instance labels):**

| Label | Description |
|-------|-------------|
| `kro.run/instance-id` | UID of the instance that created this resource |
| `kro.run/instance-name` | Name of the instance |
| `kro.run/instance-namespace` | Namespace of the instance |
| `kro.run/node-id` | The resource ID from the RGD (e.g., `workerPods`) |
| `applyset.kubernetes.io/part-of` | Links the resource to its parent instance (matches the instance's `applyset.kubernetes.io/id`) |

**Collection-specific labels** (only on resources created via `forEach`):

| Label | Description |
|-------|-------------|
| `kro.run/collection-index` | Position in the collection (0-indexed) |
| `kro.run/collection-size` | Total number of items in the collection |

These labels allow you to identify exactly which instance owns each managed resource, which is essential when multiple instances of the same RGD exist in a cluster. For collection resources, see [Collection Labels](./rgd/02-resource-definitions/04-collections.md#collection-labels) for more details.

</TabItem>
</Tabs>

:::info ApplySet Specification
kro uses the [Kubernetes ApplySet specification](https://git.k8s.io/enhancements/keps/sig-cli/3659-kubectl-apply-prune) for tracking and pruning managed resources. This enables kro to automatically prune resources that are no longer part of the instance's resource graph and prevents other tools from accidentally modifying kro-managed resources.
:::

### Owner References

kro does not set Kubernetes owner references on managed resources by default. This is intentional for two reasons:

1. **Ordered deletion** - kro deletes resources in reverse topological order, respecting dependencies between resources. Owner references trigger Kubernetes garbage collection, which deletes resources without ordering guarantees.

2. **Cross-scope limitations** - Namespaced resources cannot own cluster-scoped resources. Since kro instances are namespaced but can manage cluster-scoped resources (like ClusterRoles or Namespaces), owner references cannot express this relationship.

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
NAME     STATUS    READY   AGE
my-app   ACTIVE    true    30s
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
- `FAILED` - Instance failed to reconcile properly
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