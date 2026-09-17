---
sidebar_position: 6
---

# Deletion Policy

By default, deleting an instance deletes every resource kro created for it. Some
resources are too risky to remove that way: a database, a PersistentVolumeClaim,
a bucket. The `deletionPolicy` field on a resource says what should happen to
that resource when kro no longer wants it.

```kro
resources:
  - id: database
    deletionPolicy: Detach
    template:
      apiVersion: v1
      kind: PersistentVolumeClaim
      metadata:
        name: ${schema.spec.name}-data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: ${schema.spec.storage}
  - id: deployment
    deletionPolicy: Delete   # explicit, same as the default
    template:
      apiVersion: apps/v1
      kind: Deployment
      # ...
```

## Values

| Value | Behaviour |
| --- | --- |
| `Delete` | Delete the resource. This is what happens when the field is omitted. |
| `Detach` | Leave the resource in the cluster and release it. |

Releasing a resource removes the labels and annotations kro applied to it. The
object itself, including every field kro set on it, is left exactly as it was.
Because the labels are what mark a resource as belonging to an instance, a
released resource is no longer tracked by kro at all, and a later instance can
adopt it.

The policy applies whenever kro would remove the resource, not just on instance
deletion:

- the instance is deleted
- the resource is removed from the ResourceGraphDefinition
- its `includeWhen` turns false
- a `forEach` expansion shrinks and drops the item

## Only on templates

`deletionPolicy` is only valid on a resource with a `template`. An
`externalRef` is read-only: kro never creates or deletes it, so a policy on it
would mean nothing, and declaring one is rejected when the
ResourceGraphDefinition is applied.

## How it is recorded

kro writes the policy onto the managed resource as the
`kro.run/deletion-policy` annotation:

```console
$ kubectl get pvc demo-data -o jsonpath='{.metadata.annotations.kro\.run/deletion-policy}'
Detach
```

This is how deletion works at all: kro rebuilds the set of resources to clean up
from the cluster itself rather than re-evaluating the graph, so the resource has
to carry its own policy. It also means you can confirm from the object whether
kro will delete it.

Changing `deletionPolicy` on an existing resource takes effect as soon as the
instance next reconciles and the annotation is rewritten. If you are about to
hand a resource over to something else, set `Detach` and wait for the annotation
to appear before deleting the instance.

## Migrating a resource between graphs

Releasing is what makes it possible to move a resource from one
ResourceGraphDefinition to another without downtime:

1. Set `deletionPolicy: Detach` on the resource in the old RGD and let the
   instance reconcile.
2. Delete the old instance. The resource stays, released.
3. Declare the same resource in the new RGD and create an instance. kro adopts
   the existing object rather than creating a new one.

:::note

`deletionPolicy` is alpha. A broader resource lifecycle field is under design
and may supersede it.

:::
