---
sidebar_position: 6
---

# Custom Status Conditions

By default, kro adds four built-in conditions to every instance's
`.status.conditions[]` that describe its reconciliation lifecycle:

| Type | Meaning |
|---|---|
| `InstanceManaged` | The instance has kro's finalizers and labels applied. |
| `GraphResolved` | The runtime graph was built and all resources resolved. |
| `ResourcesReady` | All resources were created and reached their ready state. |
| `Ready` | Root condition: true when the three above are true. |

These describe kro's lifecycle ("all child resources were applied"), not
whether your application is actually healthy. Custom status conditions let an
RGD author declare their own conditions, computed from resource state on every
reconcile, and write them to `.status.conditions[]`.

## Declaring conditions

Add a `conditions:` list under the schema's `status`. Each entry is a CEL
expression that returns a condition built with `runtime.newCondition`:

```kro
schema:
  apiVersion: v1alpha1
  kind: WebApp
  spec:
    replicas: integer
  status:
    conditions:
      - ${runtime.newCondition({
          type: 'AppReady',
          status: deployment.status.readyReplicas > 0 ? 'True' : 'False',
          reason: 'ReplicaCount',
          message: ''
        })}
```

`runtime.newCondition` takes a map with four keys: `type`, `status`, `reason`,
and `message`. `type` and `status` are required; `status` must be `'True'`,
`'False'`, or `'Unknown'`. kro stamps `lastTransitionTime` and
`observedGeneration` on the written condition.

## Ownership of the status surface

When an RGD declares a `conditions:` block, only the author's conditions appear
on `.status.conditions[]`. kro's four built-ins are filtered off the wire so the
author owns the status surface. The built-ins are still computed and remain
readable from author CEL through `runtime.condition` (see below).

## Reading kro's built-in conditions

`runtime.condition(schema, 'X')` returns kro's internal value for a built-in
condition type, regardless of what the author writes to the wire. This lets an
author define their own `Ready` that composes kro's lifecycle signal with a
domain check:

```kro
status:
  conditions:
    - ${runtime.newCondition({
        type: 'Ready',
        status: runtime.condition(schema, 'ResourcesReady').status == 'True'
          && deployment.status.readyReplicas > 0 ? 'True' : 'False',
        reason: deployment.status.readyReplicas > 0 ? 'AllHealthy' : 'NotReady',
        message: ''
      })}
```

Custom conditions cannot reference each other: `runtime.condition(schema, 'X')`
where `X` is a type the same RGD defines with `runtime.newCondition` is
rejected when the RGD is created. Only kro's built-in types can be read this
way.

## One condition per collection item

A single `conditions:` entry can return a list of conditions. kro flattens the
result, so the number of conditions on the instance tracks the collection size.
Mapping over a `forEach` resource gives each element's full object:

```kro
schema:
  apiVersion: v1alpha1
  kind: SubnetGroup
  spec:
    name: string
    cidrBlocks: "[]string"
  status:
    conditions:
      - ${subnets.map(s, runtime.newCondition({
          type: 'Subnet-' + s.metadata.name + '-Ready',
          status: s.status.state == 'available' ? 'True' : 'False',
          reason: 'SubnetState',
          message: ''
        }))}
resources:
  - id: subnets
    forEach:
      - cidr: ${schema.spec.cidrBlocks}
    template:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: Subnet
      metadata:
        name: ${schema.spec.name}-${cidr}
      spec:
        cidrBlock: ${cidr}
        vpcID: vpc-123
```

## Degraded evaluation

If a condition expression fails to evaluate or two conditions resolve to the
same `type`, the surviving conditions are still written and the instance's
`state` is set to `Error`. An expression whose data is not yet available is
skipped for that reconcile and reappears once the data resolves.
