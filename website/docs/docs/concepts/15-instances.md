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

### Labels

When reconciling Instances, kro adds certain labels for tracking purposes.

Labels added to instance:
* `kro.run/reconciled-by-rgd` - Name of the RGD that defines the instance CRD and reconciles the instance.

Labels added to resources created by the instance to identify the RGD:
* `kro.run/defined-by-rgd` - Name of the RGD where the resources are defined.

Labels added to resources created by the instance to link back to the instance:
* `kro.run/managed-by-instance-group`: The API group of the instance (e.g., `mygroup.example.com`).
* `kro.run/managed-by-instance-kind`: The kind of the instance (e.g., `MyKind`).
* `kro.run/managed-by-instance-namespace`: The namespace of the instance (e.g., `default`).
* `kro.run/managed-by-instance-name`: The name of the instance (e.g., `my-instance`).

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
