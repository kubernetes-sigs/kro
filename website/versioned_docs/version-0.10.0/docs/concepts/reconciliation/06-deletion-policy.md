---
sidebar_position: 6
---

# Deletion Policy

By default, deleting an instance deletes every resource kro created for it. Some
resources are too risky to remove that way: a database, a PersistentVolumeClaim,
a bucket. The `kro.run/deletion-policy` annotation on a resource template says
what should happen to that resource when it's removed.

:::note

The deletion policy is alpha and requires the `DeletionPolicy`
[feature gate](../../advanced/02-feature-gates.md#deletionpolicy).

:::

:::note

The deletion policy is currently only available for RGDs, with the
intention of also adding it to Graphs after substantial user experience.

:::

```kro
resources:
  - id: database
    template:
      apiVersion: v1
      kind: PersistentVolumeClaim
      metadata:
        name: ${schema.spec.name}-data
        annotations:
          kro.run/deletion-policy: Orphaned
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: ${schema.spec.storage}
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        annotations:
          kro.run/deletion-policy: Delete
      # ...
```

## Values

| Value      | Behaviour                                                                                           |
|------------|-----------------------------------------------------------------------------------------------------|
| `Delete`   | Delete the resource. This is also what happens when the annotation is absent.                           |
| `Orphaned` | Leave the resource in the cluster and release it.                                                   |

Releasing a resource removes the labels and annotations kro applied to it including
the ApplySet ID. The object itself is left exactly as it was.

Because the labels are what mark a resource as belonging to an instance, a
released resource is no longer tracked by kro at all, and a later instance can
adopt it if needed.

The policy applies whenever kro would remove the resource, not just on instance
deletion:

- the instance is deleted
- the resource is removed from the ResourceGraphDefinition
- its `includeWhen` turns false
- a `forEach` expansion shrinks and drops the item

:::warning Owner references

Releasing does not touch `metadata.ownerReferences`. If the template sets
[owner references](../rgd/06-instances.md#owner-references) pointing at the
instance, Kubernetes garbage collection deletes the resource when the instance
goes away, whatever its deletion policy says. Don't set owner references on
`Orphaned` resources.

:::

## How it works

The annotation is applied to the managed resource like any other template
annotation:

```console
$ kubectl get pvc demo-data -o jsonpath='{.metadata.annotations.kro\.run/deletion-policy}'
Orphaned
```

kro rebuilds the set of resources to clean up from the cluster itself rather
than re-evaluating the graph, so it reads the policy from the live object. That
also means you can confirm from the object whether kro will delete it or not.

A literal value must be `Delete` or `Orphaned`, or the RGD is rejected. The value
can also be a CEL expression, such as `${schema.spec.retainData ? "Orphaned" : "Delete"}`.
An expression that resolves to anything other than `Orphaned` means `Delete`.

:::warning Change the policy before removing the resource

A policy change only takes effect once the instance reconciles and the
annotation reaches the object. If you add `Orphaned` and remove the resource (or
turn its `includeWhen` false) in the same change, the object never gets the
annotation and kro deletes it. Set the policy first, wait for the annotation to
appear, then remove the resource or delete the instance.

:::

## Migrating a resource between graphs

Releasing is what makes it possible to move a resource from one
ResourceGraphDefinition to another without downtime:

1. Add `kro.run/deletion-policy: Orphaned` to the resource in the old RGD and
   let the instance reconcile.
2. Delete the old instance. The resource stays, released.
3. Declare the same resource in the new RGD and create an instance. kro adopts
   the existing object rather than creating a new one.

:::note

The deletion policy is alpha. A broader resource lifecycle field is under
design (KREP-014) and may supersede this annotation.

:::
