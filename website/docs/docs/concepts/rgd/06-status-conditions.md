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
`'False'`, or `'Unknown'`. Each entry must return a condition (or a list of
conditions, see below); anything else is rejected when the RGD is created.
kro stamps `lastTransitionTime` and `observedGeneration` on the written
condition.

## Ownership of the status surface

When an RGD declares a `conditions:` block, only the author's conditions appear
on `.status.conditions[]`. kro's four built-ins are filtered off the wire so the
author owns the status surface. The built-ins are still computed and remain
readable from author CEL through `runtime.condition` (see below).

The instance's `status.state` field stays kro-driven: it reflects kro's
reconciliation lifecycle, so an instance can report `state: ACTIVE` while the
author's own `Ready` condition is `False`.

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

Lookups on `schema` are restricted: `runtime.condition(schema, 'X')` where `X`
is a type the same RGD defines with `runtime.newCondition` is rejected when
the RGD is created (custom conditions cannot reference each other), and any
other literal `X` that isn't a built-in type is rejected as unknown. A
computed type looks up only the built-ins and returns an empty condition when
not found. Conditions on child resources
(`runtime.condition(myresource, 'X')`) can be read freely.

Two `conditions:` entries may not declare the same literal `type`; the RGD is
rejected when created. Computed types that collide at evaluation time are
handled as described under [Degraded evaluation](#degraded-evaluation).

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

If a condition expression fails to evaluate or two computed conditions
resolve to the same `type`, the surviving conditions are still written and
the instance's `state` is set to `Error`. An expression whose data is not yet
available (for example, a referenced resource that hasn't been created) is
skipped for that reconcile without setting `state: Error`.

In all of these cases, conditions previously written for the types that
produced no output stay on `.status.conditions[]` unchanged — keeping their
`lastTransitionTime` and `observedGeneration` — until their expression
evaluates again. A fully successful evaluation replaces the wire, so
condition types that are no longer produced are cleaned up. Removing the
`conditions:` block from an RGD returns ownership of `.status.conditions[]`
to kro's built-ins.
