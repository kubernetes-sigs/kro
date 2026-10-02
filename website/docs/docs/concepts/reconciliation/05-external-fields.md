---
sidebar_position: 5
---

# External Fields

Sometimes a resource kro creates needs to have one of its fields taken over by a different controller after kro creates it — an HPA scaling a Deployment's `replicas`, Argo Rollouts shifting a Service's `selector` during a blue/green rollout, or a Gateway API controller adjusting an HTTPRoute's backend weights. Without `externalFields`, kro resets those fields back to the template value on every reconcile, fighting the other controller.

`externalFields` lets you list specific dotted paths on a resource that kro should hand off once another controller actually starts managing them, while kro keeps creating the resource (with an initial value) and managing every other field normally.

## Basic Example

```kro
resources:
  - id: app
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.spec.name}
      spec:
        replicas: 3
        selector:
          matchLabels:
            app: ${schema.spec.name}
        template:
          spec:
            containers:
              - name: app
                image: ${schema.spec.image}
    externalFields:
      - spec.replicas

  - id: hpa
    template:
      apiVersion: autoscaling/v2
      kind: HorizontalPodAutoscaler
      metadata:
        name: ${schema.spec.name}
      spec:
        scaleTargetRef:
          apiVersion: apps/v1
          kind: Deployment
          name: ${app.metadata.name}
        minReplicas: 2
        maxReplicas: 10
        metrics:
          - type: Resource
            resource:
              name: cpu
              target:
                type: Utilization
                averageUtilization: 70
```

kro creates `app` with `spec.replicas: 3`. Once the HPA scales the Deployment, kro stops resetting `spec.replicas` on later reconciles — every other field on `app` (image, labels, selector, …) is still fully managed by kro.

## How It Works

- **kro sets the initial value on create.** The first apply includes every field in the template, exactly like a resource without `externalFields`.
- **kro keeps reconciling the field normally until something else claims it.** A field listed in `externalFields` behaves like any other field — hand-edit it yourself and kro still reverts it — right up until a *different* controller actually writes to that exact field.
- **Only then does kro stop including it.** kro tracks field ownership the same way Kubernetes does (via [Server-Side Apply field management](https://kubernetes.io/docs/reference/using-api/server-side-apply/#field-management)). Once a write from another field manager is observed on the path, kro omits it from future applies, so the other controller's value is never reset.
- **kro still deletes the whole resource on prune.** `externalFields` only affects reconciliation of that field while the resource exists; deleting the instance deletes the whole resource as usual.

:::note
This sequencing matters: if kro released the field the moment it was created — before any other controller had a chance to touch it — the field would have no owner at all, and Kubernetes would delete it rather than leave the initial value in place. Waiting for an observed external write is what keeps the initial value safe.
:::

## Path Syntax

Paths are dotted, map-key-only references into the template (for example `spec.selector`, `metadata.annotations.someKey`). A path must:

- **Resolve to an existing key** in the literal template. A typo is rejected when the RGD is compiled, not silently ignored.
- **Not cross into a list.** `spec.containers.image` is rejected because `spec.containers` is a list — Server-Side Apply's associative-list merge keys make releasing one list element ambiguous. You can release a list field as a whole (e.g. `spec.ports`), just not address through one.

```kro
# ✓ map key path
externalFields:
  - spec.selector

# ✗ rejected at compile time: containers is a list
externalFields:
  - spec.containers.image
```

`externalFields` is only valid on a resource with a `template` — it has no meaning on `externalRef` (kro never creates or owns that resource) and is rejected if both are set.

## Graph Nodes

A Graph node expresses the same list under `externalFields` on a `template` node:

```kro
nodes:
  - id: app
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: my-app
      spec:
        replicas: 3
        # ...
    externalFields:
      - spec.replicas
```

## Next Steps

- **[External References](./04-external-references.md)** - Reference resources kro doesn't create at all
- **[Readiness](./02-readiness.md)** - `readyWhen` conditions can reference an externally-managed field's live value
- **[Graph Nodes](../graph/02-nodes.md)** - The `template` node kind and its siblings
