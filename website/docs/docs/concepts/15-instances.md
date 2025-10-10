---
sidebar_position: 15
---

# Instances

Once **kro** processes your `ResourceGraphDefinition`, it creates a new API in your cluster.
Users can then create *instances* of this API to deploy and manage resources in a consistent and declarative way.

## Understanding Instances

An instance represents your deployed application.
Creating an instance tells **kro**: “ensure this set of resources is present and configured as defined.”

The instance contains all configuration values and acts as the single source of truth for your application’s desired state.

Example:

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

* Creates and configures all resources (e.g. Deployment, Service, Ingress)
* Manages them as a single unit
* Continuously reconciles to keep desired and actual states in sync
* Reports detailed status

---

## Reconciliation Modes

Each `ResourceGraphDefinition` defines how **kro** reconciles resources created by its instances, through the field:

```yaml
spec:
  reconcile:
    mode: ClientSideDelta # or ApplySet
```

:::warning

Once set, `mode` cannot be changed.

:::

### Available Modes

| Mode                               | Description                                                                                                                                                                                                                                                                                                                           |
|------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **ClientSideDelta**                | Client delta–based reconciliation. kro computes differences between desired and observed resources inside the controller and applies updates directly. Lightweight and independent, but less precise and does not support pruning.                                                                                                    |
| **ApplySet**                       | Server-side apply using [ApplySet](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cli/3659-kubectl-apply-prune). All resources are applied and pruned through an ApplySet that tracks ownership and performs SSA through the API server. Provides higher accuracy, conflict resolution, and managed field ownership. |

If not set explicitly in the `ResourceGraphDefinition`, the default mode can be controlled via the flag `--resource-graph-definition-default-reconcile-mode`

:::note

`ApplySet` mode is currently being evaluated for permanent adoption in KRO. Once stabilized, it may become the default, and `ClientSideDelta` could be deprecated.

:::


### Behavior Summary

| Capability               | ClientSideDelta                          | ApplySet                                            |
|--------------------------|------------------------------------------|-----------------------------------------------------|
| **Change detection**     | Client-side diff computed in controller  | Server-side field ownership via SSA                 |
| **Accuracy**             | Approximate                              | Precise                                             |
| **Resource pruning**     | No                                       | Yes                                                 |
| **Update model**         | Standard `UPDATE` calls                  | Server-side `APPLY` operations                      |
| **Performance**          | Faster for small graphs, fewer API calls | More API interactions but consistent cluster state  |
| **Field management**     | None                                     | Uses `FieldManager: kro.run/applyset`               |
| **Ownership tracking**   | Implicit (controller-owned)              | Explicit via ApplySet labels and SSA managed fields |
| **Conflict handling**    | Manual retries on conflict               | Managed automatically by API server                 |
| **Garbage collection**   | Not supported                            | Supported through ApplySet pruning                  |


## How kro Manages Instances

kro continuously reconciles instances using the Kubernetes controller pattern:

1. **Observe**: Watches for instance or resource changes.
2. **Compare**: Evaluates differences between desired and actual state.
3. **Act**: Creates, updates, or deletes resources according to `mode`.
4. **Report**: Updates the instance status.

This ensures self-healing, automatic updates, and declarative state management.


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
    - type: Ready
      status: "True"
      lastTransitionTime: "2024-07-23T01:01:59Z"
      reason: ResourcesAvailable
      message: "All resources are available and configured correctly"
```

### Understanding Status

Every instance includes:

1. **State**: High-level status

   - `ACTIVE`: Indicates that the instance is successfully running and active.
   - `IN_PROGRESS`: Indicates that the instance is currently being processed or reconciled.
   - `FAILED`: Indicates that the instance has failed to be properly reconciled.
   - `DELETING`: Indicates that the instance is in the process of being deleted.
   - `ERROR`: Indicates that an error occurred during instance processing.

2. **Conditions**: Detailed status information

   - `Ready`: Instance is fully operational
   - `Progressing`: Changes are being applied
   - `Degraded`: Operating but not optimal
   - `Error`: Problems detected

3. **Resource Status**: Status from your resources
   - Values you defined in your ResourceGraphDefinition's status section
   - Automatically updated as resources change

## Best Practices

- **Version Control**: Keep your instance definitions in version control
  alongside your application code. This helps track changes, rollback when
  needed, and maintain configuration history.

- **Use Labels Effectively**: Add meaningful labels to your instances for better
  organization, filtering, and integration with other tools. kro propagates
  labels to the sub resources for easy identification.

- **Active Monitoring**: Regularly check instance status beyond just "Running".
  Watch conditions, resource status, and events to catch potential issues early
  and understand your application's health.

- **Regular Reviews**: Periodically review your instance configurations to
  ensure they reflect current requirements and best practices. Update resource
  requests, limits, and other configurations as your application needs evolve.
