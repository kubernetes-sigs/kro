---
sidebar_position: 15
---

# Instances

Once **kro** processes your ResourceGraphDefinition, it creates a new API in your cluster.
Users can then create instances of this API to deploy resources in a consistent,
controlled way.

## Understanding Instances

An instance represents your deployed application. When you create an instance,
you're telling kro "I want this set of resources running in my cluster". The
instance contains your configuration values and serves as the single source of
truth for your application's desired state. Here's an example instance of our
WebApplication API:

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

1. **Observe**: Watches for changes to your instance or its resources
2. **Compare**: Checks if current state matches desired state
3. **Act**: Creates, updates, or deletes resources as needed
4. **Report**: Updates status to reflect current state

This continuous loop ensures your resources stay in sync with your desired
state, providing features like:

- Self-healing
- Automatic updates
- Consistent state management
- Status tracking

### Reactive Reconciliation

kro automatically watches all resources managed by an instance and triggers
reconciliation when any of them change. This means:

- **Child Resource Changes**: When a managed resource (like a Deployment or Service) is
  modified, kro detects the change and reconciles the instance to ensure it matches
  the desired state defined in your ResourceGraphDefinition.

- **Drift Detection**: If a resource is manually modified or deleted, kro will detect
  the drift and automatically restore it to the desired state.

- **Dependency Updates**: Changes to resources propagate through the dependency graph,
  ensuring all dependent resources are updated accordingly.

This reactive behavior ensures your instances maintain consistency without requiring
manual intervention or periodic full reconciliations.

## Monitoring Your Instances

KRO provides rich status information for every instance:

```bash
$ kubectl get webapplication my-app
NAME     STATUS    SYNCED   AGE
my-app   ACTIVE    true     30s
```

For detailed status, check the instance's YAML:

```yaml
status:
  state: ACTIVE # High-level instance state
  availableReplicas: 3 # Status from Deployment
  conditions: # Detailed status conditions
    - lastTransitionTime: "2025-08-08T00:03:46Z"
      message: instance is properly managed with finalizers and labels
      observedGeneration: 1
      reason: Managed
      status: "True"
      type: InstanceManaged
    - lastTransitionTime: "2025-08-08T00:03:46Z"
      message: runtime graph created and all resources resolved
      observedGeneration: 1
      reason: Resolved
      status: "True"
      type: GraphResolved
    - lastTransitionTime: "2025-08-08T00:03:46Z"
      message: all resources are created and ready
      observedGeneration: 1
      reason: AllResourcesReady
      status: "True"
      type: ResourcesReady
    - lastTransitionTime: "2025-08-08T00:03:46Z"
      message: ""
      observedGeneration: 1
      reason: Ready
      status: "True"
      type: Ready
```

### Understanding Status

Every instance includes:

1. **State**: High-level status

   - `ACTIVE`: Indicates that the instance is successfully running and active.
   - `IN_PROGRESS`: Indicates that the instance is currently being processed or reconciled.
   - `FAILED`: Indicates that the instance has failed to be properly reconciled.
   - `DELETING`: Indicates that the instance is in the process of being deleted.
   - `ERROR`: Indicates that an error occurred during instance processing.

2. **Conditions**: Detailed status information structured hierarchically

   kro provides a top-level `Ready` condition that reflects the overall instance health. This condition is supported by three sub-conditions that track different phases of the reconciliation process:

   - `InstanceManaged`: Instance finalizers and labels are properly set
     - Ensures the instance is under kro's management
     - Tracks whether cleanup handlers (finalizers) are configured
     - Confirms instance is labeled with ownership and version information

   - `GraphResolved`: Runtime graph has been created and resources resolved
     - Validates that the resource graph has been successfully parsed
     - Confirms all resource templates have been resolved
     - Ensures dependencies between resources are properly understood

   - `ResourcesReady`: All resources in the graph are created and ready
     - Tracks the creation and readiness of all managed resources
     - Monitors the health of resources in topological order
     - Reports when all resources have reached their ready state

   - `Ready`: Instance is fully operational (top-level condition)
     - Aggregates the state of all sub-conditions
     - Only becomes True when all sub-conditions are True
     - **The primary condition to monitor and wait on for instance health**
     - Use this condition in automation, CI/CD, and health checks

   :::tip
   Always use the `Ready` condition to determine instance health. The sub-conditions (`InstanceManaged`, `GraphResolved`, `ResourcesReady`) are provided for debugging purposes and may change in future versions. kro reserves the right to add, remove, or modify sub-conditions without breaking compatibility as long as the `Ready` condition behavior remains stable.
   :::

   Each condition includes:
   - `observedGeneration`: Tracks which generation of the instance this condition reflects
   - `lastTransitionTime`: When the condition last changed state
   - `reason`: A programmatic identifier for the condition state
   - `message`: A human-readable description of the current state

3. **Resource Status**: Status from your resources
   - Values you defined in your ResourceGraphDefinition's status section
   - Automatically updated as resources change

## Debugging Instance Issues

When an instance is not in the expected state, the condition hierarchy helps you quickly identify where the problem occurred:

1. **Check the Ready condition first**
   ```bash
   kubectl get <your-kind> <instance-name> -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
   ```

2. **If Ready is False, check the sub-conditions** to identify which phase failed:

   - If `InstanceManaged` is False: Check if there are issues with finalizers or instance labels
   - If `GraphResolved` is False: The resource graph could not be created - check the ResourceGraphDefinition for syntax errors or invalid CEL expressions
   - If `ResourcesReady` is False: One or more managed resources failed to become ready - check the error message for which resource failed

3. **Use kubectl describe** to see all conditions and recent events:
   ```bash
   kubectl describe <your-kind> <instance-name>
   ```

4. **Check the observedGeneration** field in conditions:
   - If `observedGeneration` is less than `metadata.generation`, the controller hasn't processed the latest changes yet
   - If they match, the conditions reflect the current state of your instance

## Best Practices

- **Version Control**: Keep your instance definitions in version control
  alongside your application code. This helps track changes, rollback when
  needed, and maintain configuration history.

- **Use Labels Effectively**: Add meaningful labels to your instances for better
  organization, filtering, and integration with other tools. kro propagates
  labels to the sub resources for easy identification.

- **Active Monitoring**: Regularly check instance status beyond just "Running".
  Watch conditions, resource status, and events to catch potential issues early
  and understand your application's health. Focus on the `Ready` condition and
  its sub-conditions to understand the reconciliation state.

- **Monitor observedGeneration**: When making changes to an instance, verify that
  `observedGeneration` in the conditions matches `metadata.generation` to ensure
  kro has processed your changes.

- **Leverage Reactive Reconciliation**: kro automatically detects and corrects drift
  in managed resources. If you need to make manual changes to resources, update the
  instance specification instead to ensure changes persist and align with your
  desired state.

- **Regular Reviews**: Periodically review your instance configurations to
  ensure they reflect current requirements and best practices. Update resource
  requests, limits, and other configurations as your application needs evolve.
