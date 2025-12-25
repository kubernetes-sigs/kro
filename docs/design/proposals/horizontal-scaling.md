# Horizontal scaling in KRO

This proposal complements [KREP-002] and defines:
- An interface to control `ResourceGraphDefinition` (RGD) instance scale using the Kubernetes “scale subresource”.
- An opt-in mechanism to replicate RGD instances and declaratively control rollout across replicas.

It is inspired by Kubernetes Set controllers (for example, `ReplicaSet`) to achieve safe, observable, and controllable rollouts.

## Goals

- Integrate KRO with the Kubernetes horizontal scaling ecosystem (for example, `kubectl scale`, [HPA], and [keda]) via the [scale-subresources].
- Keep a single RGD instance as a “unit of deployment”; KRO’s graph resolution ensures correct ordering and propagation within that unit.
- Provide an optional “Set” abstraction to replicate RGD instances and control rollout across replicas.

## Non-goals

- Implement all possible scaling or rollout strategies.
- Replace or reimplement HPA/VPA behavior.
- Manage progressive rollout within a single RGD instance. The Set abstraction controls rollout across replicas only.

## Background and problem statement

In [KREP-002] we introduced looping/collections. Collections may be required for:
- Different environments (for example, private vs. public ingresses).
- Different customers (duplicated stacks, different hostnames).
- Horizontal scale based on load (request rate, queue depth, number of replicas, and so on).

Required replica counts can vary rapidly. Kubernetes already provides a rich ecosystem (`kubectl scale`, [HPA], [keda]). KRO should integrate with this ecosystem.

Replicas can be:
- A single resource (for example, a Pod or a Deployment), or
- A set of resources that together form a functional unit (for example, a [cluster-api] [capa-cluster]).

When a replica is a set of resources, we need controlled rollout to limit blast radius if a new version/configuration causes issues.

## Design overview

This proposal proceeds in two phases:

1) Scale subresource on RGD (integration with the Kubernetes scaling ecosystem).
2) RGD Set controller (replicate RGD instances and control rollout across replicas).

### Phase 1: Scale subresource on RGD

Users add explicit schema annotations on RGD fields to expose the Scale subresource:
- `| scale` on integer fields to represent desired/current replicas.
- `| scaleSelector` on a status string field that holds a label selector for Pods (used by HPA/VPA).

Validation rules:
- `| scale` applies to integers only.
- `| scale` must be present on both spec and status (or on neither).
- `| scaleSelector` applies to a string attribute in status. KRO does not validate selector correctness nor enforce that it matches Pods.

Example (minimum to expose Scale):

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
schema:
  spec:
    desiredReplicas: "integer | scale"
  status:
    replicas: "integer | scale"
    podSelector: "string | scaleSelector"
```

Usage:
- `kubectl scale` works against the RGD instances.
- HPA can target the RGD Scale subresource and use `status.podSelector` to select Pods.

Example HPA (illustrative):

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-rgd-hpa
spec:
  scaleTargetRef:
    apiVersion: example.org/v1
    kind: MyObject
    name: some-name
  minReplicas: 1
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70
```

### Phase 2: RGD Set controller (replication and rollout)

An RGD can define a `scaling` section to emit an additional CRD that manages sets of RGD instances.
A dedicated dynamic controller reconciles the Set and performs creation, update, and deletion of the underlying RGD instances.

High-level behavior:
- A new `MyObjectSet` CRD is generated next to `MyObject` (the RGD instance CRD).
- The Set controller lives alongside existing KRO controllers and has one responsibility: manage RGD instance replicas.
- `OwnerReferences` are set from each RGD instance to its Set.
- Update order: oldest to newest.
- Deletion order: newest to oldest.
- Rolling update concurrency is configurable.
- The controller waits for each updated instance to become “Ready” (KRO Ready condition) before proceeding.

Example RGD enabling a Set:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
scaling:
  kind: MyObjectSet
  method: replicas
schema:
  kind: MyObject
  spec:
    desiredReplicas: "integer | scale"
  status:
    replicas: "integer | scale"
    podSelector: "string | scaleSelector"
```

This generates CRDs `MyObject` and `MyObjectSet`.

Users can define a Set to replicate `MyObject`:

```yaml
apiVersion: example.org/v1
kind: MyObjectSet
metadata:
  name: some-name
spec:
  replicas: 1
  rollingUpdate:
    maxConcurrentUpdated: 2
  template:
    spec: ${specThatMatchesResourceGraphDefinitionSchema}
```

The Set controller creates a `MyObject` instance:

```yaml
apiVersion: example.org/v1
kind: MyObject
metadata:
  name: some-name-ldcs4
spec: ${specThatMatchesResourceGraphDefinitionSchema}
```

Scaling via CLI:
- `kubectl scale MyObjectSet/some-name --replicas=10`

Rolling updates:
- When the Set template changes, the controller updates `MyObject` instances (up to `maxConcurrentUpdated` at a time) and waits for each to become Ready before proceeding.

## Controller details (for developers)

Triggers and watch setup:
- Reconcile on Set changes and on underlying RGD instance changes.
- Set and RGD instances share scope (same namespace or cluster-scoped).

Readiness and progression:
- The Set controller uses the RGD instance Ready condition to determine when to continue rolling updates.

Ordering and naming:
- Instances are named with randomized suffixes (ReplicaSet-like).
- Update oldest-to-newest; delete newest-to-oldest.

Error handling and backoff:
- Standard controller-runtime requeue with exponential backoff on errors.

Observability:
- Emit Events on Set and instances for create/update/delete.
- Expose metrics for reconciliation latency and in-progress rollouts.

## Validation summary

- `| scale` only on integer fields.
- `| scale` must appear in both spec and status together.
- `| scaleSelector` only on a status string field; KRO does not validate selector semantics.

## Scoping

### In scope

- “Count/replicas” scaling with randomized names (ReplicaSet-like) and in-place updates of RGD instances.
- Creating instances when scaling up; deleting instances when scaling down.
- Updating instances oldest-to-newest; deleting newest-to-oldest.
- Configurable parallelism for rolling updates.

### Out of scope (future work candidates)

- Replicating RGDs based on other objects (for example, Nodes, Namespaces).
- Customizable update/delete orders beyond the default.
- Alternate naming conventions (for example, ordinal numbering).

## Testing strategy

Unit tests:
- CRD generation with `| scale` and `| scaleSelector` annotations.
- Validation of schema rules (types, presence in spec/status).
- Set controller logic: ordering, concurrency, readiness gates, and error paths.

Integration tests:
- Scale subresource end-to-end: `kubectl scale`, HPA interactions.
- Set controller end-to-end: creation, scaling up/down, rolling update respecting concurrency and readiness.

## Other solutions considered

- Implement Set behavior directly inside the RGD reconciler together with collections.
  This couples graph resolution, failure domains, and rollout strategies, reducing user control and observability over rollouts.
- Implement horizontal scaling in an external controller.
  This could scale any resource but increases user complexity (more controllers to install, configure, and monitor).


[KREP-002]: https://github.com/kubernetes-sigs/kro/pull/679
[cluster-api]: https://cluster-api.sigs.k8s.io/introduction
[capa-cluster]: https://github.com/kubernetes-sigs/cluster-api-provider-aws/blob/main/templates/cluster-template-eks.yaml
[scale-subresources]: https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#scale-subresource
[HPA]: https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/
[keda]: https://keda.sh/