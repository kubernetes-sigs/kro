---
sidebar_position: 6
---

# Instance Deletion

Deleting an instance also deletes the resources managed by that instance. kro
keeps the instance finalizer until all managed resources are gone. Resources
referenced through `externalRef` are read-only and are never deleted by kro.

## Deletion Order

kro deletes dependents before their dependencies. Resources that do not depend
on each other share a deletion wave. A wave must disappear completely before
the next wave starts, so a child finalizer can pause the rest of the deletion.

kro stores each child's wave in the
`internal.kro.run/apply-order` annotation. Children without a valid annotation,
such as resources created before an upgrade, are deleted in a final unordered
wave.

## Resource Discovery

kro discovers children from the instance's persisted ApplySet inventory. It
does not need to evaluate the current graph or CEL expressions during deletion.
Cleanup can therefore continue when an external reference is gone or desired
state can no longer be resolved.

## Troubleshooting

While deletion is in progress, the instance has a `ResourcesReady` condition
with status `Unknown` and reason `UnderDeletion`. Its message reports inventory,
discovery, or delete errors. Also check managed children for finalizers that may
be blocking the active wave.
