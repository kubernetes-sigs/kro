# KREP-013 Resource Contribution

## Summary

Today, KRO can observe a resource it doesn't own via `externalRef`, but it can never write to it.
The only way KRO can mutate a resource is to own it -- to have created it via a resource template.
This KREP introduces a new node type, `patch`, which points to a resource KRO does not own and
contributes fields to it via server-side apply. `externalRef` remains read-only. `patch` is the
write counterpart.

## Motivation

It is common in Kubernetes for multiple independent control loops to cooperate on a single object's
lifecycle. A Pod, for example, is not moved through its phases by a single controller. The scheduler
assigns it to a node. The controller manager ensures it exists. The kubelet runs it and writes
status back. Each component manages a distinct slice of the object's fields, and the object's state
is the composition of their contributions.

Today, KRO cannot participate in this model. A single RGD is responsible for every field it touches,
and the only resources KRO can write to are resources it created. This means a single RGD must
contain the full implementation of every behavior it wants to express. There is no way to have
multiple independent KRO graphs cooperate on the same object -- no way for one RGD to handle
networking, another to handle scaling, and a third to aggregate status, all contributing to the same
workload object.

`patch` closes this gap. Any number of RGDs can contribute different fields to the same resource.
Server-side apply enforces ownership at the field level, so each contributor manages only what it
declares, and conflicts are surfaced explicitly. KRO becomes a flexible building block in a broader
composition model, not a monolithic owner.

## Proposal

### Patch

A `patch` node identifies a resource by its primary key -- `apiVersion`, `kind`, `metadata.name`,
and `metadata.namespace` -- and contributes all other declared fields to it via server-side apply.
Fields may be static values or `${...}` CEL expressions.

```yaml
# Read-only -- observe a Deployment, import its state into the graph
- id: target
  externalRef:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-deployment

# Patch -- contribute a status condition to a Deployment we don't own
- id: target
  patch:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: my-deployment
    status:
      conditions:
        - type: PlatformManaged
          status: "True"
```

KRO issues an SSA Apply with a partial object containing only the declared fields. The Deployment's
other fields -- spec, existing conditions, labels -- are untouched. The field manager is derived
from the RGD name (e.g. `KRO.run/rgd/<rgd-name>`), so multiple RGDs can contribute different fields
to the same Deployment without conflict. When two RGDs claim the same field, SSA returns a conflict
-- this is the enforcement boundary.

### Collection Patch

To patch a collection, observe it with a selector-based `externalRef`, then `forEach` over the
result into a named `patch`. One node observes, one patches.

```yaml
# Observe all Deployments in the cluster
- id: deployments
  externalRef:
    apiVersion: apps/v1
    kind: Deployment

# Patch each one
- id: managedDeployments
  forEach:
    - deployment: ${deployments}
  patch:
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: ${deployment.metadata.name}
      namespace: ${deployment.metadata.namespace}
    status:
      conditions:
        - type: PlatformManaged
          status: "True"
```

KRO issues a separate SSA Apply per item.

## Example: Sidecar Injector

A platform team wants every Deployment in the cluster to run a log collector sidecar. The
Deployments are owned by many different teams and RGDs -- the platform team cannot modify them
directly. A `SidecarInjector` instance patches the sidecar container into each Deployment via SSA,
without taking ownership of any other field.

```yaml
apiVersion: KRO.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: sidecar-injector
spec:
  schema:
    apiVersion: v1alpha1
    group: platform.example.com
    kind: SidecarInjector
    spec:
      logCollectorImage: string
  resources:
    # Observe all Deployments in the cluster
    - id: deployments
      externalRef:
        apiVersion: apps/v1
        kind: Deployment

    # Patch the sidecar container into each Deployment
    - id: injectedDeployments
      forEach:
        - deployment: ${deployments}
      patch:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${deployment.metadata.name}
          namespace: ${deployment.metadata.namespace}
        spec:
          template:
            spec:
              containers:
                - name: log-collector
                  image: ${schema.spec.logCollectorImage}
```

Creating a `SidecarInjector` instance causes KRO to SSA-patch every Deployment with the
`log-collector` container entry. Kubernetes tracks container list items by `name` as the merge key,
so `log-collector` is a distinct ownership slice -- the application's own containers are untouched.
Updating `logCollectorImage` propagates to every Deployment and triggers a rolling update on each.

This is strictly better than a mutating admission webhook for this use case: ownership is explicit
and tracked, updates propagate to existing Deployments, and cleanup is automatic. The tradeoff is
that injection and image updates trigger rolling updates -- the same behavior a webhook would
produce if it patched existing Deployments.

## Example: Task State Machine

Multiple independent RGDs can cooperate to move an object through a lifecycle -- the same model as
the Kubernetes scheduler, controller manager, and kubelet cooperating on a Pod. Here, three RGDs
implement the phases of a `Task` object: `Pending`, `Working`, and `Complete`. No single RGD owns
the full lifecycle; each owns exactly one transition.

The `Task` type is defined once:

```yaml
apiVersion: KRO.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: task
spec:
  schema:
    apiVersion: v1alpha1
    group: example.com
    kind: Task
    spec:
      image: string
    status:
      phase: "..."
```

`status.phase` is intentionally left without a CEL expression -- it is owned entirely by the
patching RGDs below. The Task RGD defines the field; it does not compute it.

The **scheduler** RGD observes all Tasks, filters to Pending ones in CEL, creates a Job for each,
and transitions the phase to Working once the Job exists:

```yaml
apiVersion: KRO.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: task-scheduler
spec:
  schema:
    apiVersion: v1alpha1
    group: example.com
    kind: TaskScheduler
    spec: {}
  resources:
    # Observe all Tasks
    - id: tasks
      externalRef:
        apiVersion: example.com/v1alpha1
        kind: Task

    # Create a Job for each Pending task
    - id: jobs
      forEach:
        - task: ${tasks.filter(t, t.status.phase == "Pending")}
      template:
        apiVersion: batch/v1
        kind: Job
        metadata:
          name: ${task.metadata.name}
          namespace: ${task.metadata.namespace}
        spec:
          template:
            spec:
              containers:
                - name: worker
                  image: ${task.spec.image}

    # Transition to Working once the Job exists
    # Referencing `jobs` creates a DAG dependency -- this node evaluates after jobs are applied
    - id: workingTasks
      forEach:
        - task: ${tasks.filter(t, t.status.phase == "Pending")}
      patch:
        apiVersion: example.com/v1alpha1
        kind: Task
        metadata:
          name: ${task.metadata.name}
          namespace: ${task.metadata.namespace}
        status:
          phase: ${jobs.filter(j, j.metadata.name == task.metadata.name).size() > 0 ? "Working" : "Pending"}
```

The **completion** RGD observes all Tasks and Jobs, filters to Working tasks, and transitions each
to Complete once its Job has succeeded:

```yaml
apiVersion: KRO.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: task-completion
spec:
  schema:
    apiVersion: v1alpha1
    group: example.com
    kind: TaskCompletion
    spec: {}
  resources:
    # Observe all Tasks
    - id: tasks
      externalRef:
        apiVersion: example.com/v1alpha1
        kind: Task

    # Observe all Jobs
    - id: jobs
      externalRef:
        apiVersion: batch/v1
        kind: Job

    # Transition Working tasks to Complete when their Job has succeeded
    - id: completedTasks
      forEach:
        - task: ${tasks.filter(t, t.status.phase == "Working")}
      patch:
        apiVersion: example.com/v1alpha1
        kind: Task
        metadata:
          name: ${task.metadata.name}
          namespace: ${task.metadata.namespace}
        status:
          phase: ${jobs.filter(j, j.metadata.name == task.metadata.name)[0].status.succeeded > 0 ? "Complete" : "Working"}
```

Each RGD owns exactly one slice of the Task's lifecycle. They compose without coordination -- adding
a new phase, swapping the Job implementation, or introducing a parallel completion strategy requires
only a new RGD, not a change to the others.

## Discussion

- **Patch naming** -- `patch` is the working name for the contributing node type. The reference
  semantics analogy is useful: a reference points to an object you did not construct and are not
  responsible for destroying, but can read and mutate. `patch` captures the mutation intent clearly,
  though `reference` is a reasonable alternative if the goal is to emphasize the ownership model
  over the operation.

- **Unifying `externalRef` and `patch`** -- both node types refer to resources KRO does not own. One
  option is to rename `externalRef` to `watch` and keep the two types distinct, giving a clean
  three-way vocabulary: `template` (own), `watch` (observe), `patch` (mutate without owning). The
  alternative is a single `reference` node type where the presence or absence of fields beyond the
  primary key determines read-only vs. contributing behavior -- collapsing the two concepts at the
  cost of explicitness.
