---
sidebar_position: 1
sidebar_label: Overview
---

import CompositionFlow from '@site/src/components/CompositionFlow';

# Composition in kro

kro composes Kubernetes resources: you describe a set of resources and how data
flows between them with CEL expressions, and kro creates them in dependency
order, keeps them in sync, and tears them down together. kro offers two APIs for
this, and both run on the same engine.

<CompositionFlow />

## Two APIs, One Engine

| | ResourceGraphDefinition | Graph |
| --- | --- | --- |
| **What you get** | A new Kubernetes API (a CRD); each instance produces its own set of resources | A single set of resources, reconciled directly |
| **Scope** | Cluster-scoped definition; instances live in namespaces | Namespaced |
| **Input** | Instance `spec`, validated against a [SimpleSchema](./rgd/02-schema.md) | None; nodes reference each other and existing cluster objects |
| **Status** | Written back to each instance | Controller-managed conditions only |
| **Identity used to apply** | The kro controller | A ServiceAccount in the Graph's namespace |
| **Availability** | Stable | Alpha, behind the `GraphKind` feature gate |

Use a **ResourceGraphDefinition** when the same composition needs to be
created many times with different inputs, or when the composition should be
exposed as a Kubernetes API: a `WebApplication`, a `Database`, an `EKSCluster`.
Whoever creates an instance works with the schema and does not need to know
about the underlying resources.

Use a **Graph** when the composition is a single thing and does not need an API
of its own: install a bundle of manifests with health checks and ordering,
react to existing resources with a decorator, or aggregate many objects into
one.

## What Is Shared

Because both APIs compile to the same engine, the authoring model is the same
and is documented once:

- **[Expressions](./expressions/01-cel-expressions.md)** - CEL syntax, the
  available function libraries, and how expressions imply dependencies and
  ordering.
- **[Reconciliation](./reconciliation/01-conditional-creation.md)** - the
  behaviors you attach to a resource: `includeWhen`, `readyWhen`, `forEach`, and
  reading existing resources.

Where the two APIs differ, those pages call it out. The largest difference is
[when dependents run relative to readiness](./reconciliation/02-readiness.md#dependencies-and-readiness).

## What Is Specific

- **[ResourceGraphDefinitions](./rgd/01-overview.md)** - the schema, resource
  templates, generated CRD, instance lifecycle, and the static analysis kro runs
  when you create an RGD.
- **[Graphs](./graph/01-overview.md)** - node kinds (`template`, `ref`, `def`,
  `patch`, `graph`), scopes and nesting, ServiceAccount impersonation, and Graph
  status.

## Next Steps

- **[ResourceGraphDefinition Overview](./rgd/01-overview.md)** - Build a custom API
- **[Graph Overview](./graph/01-overview.md)** - Compose resources directly
- **[CEL Expressions](./expressions/01-cel-expressions.md)** - The language both APIs share
