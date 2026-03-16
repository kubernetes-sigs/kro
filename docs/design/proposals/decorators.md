# KREP-003 Decorators (Collection Watching)

## Summary

KREP-003 introduces Decorators (a.k.a Collection Watching), an extension of
[Collections](https://github.com/kubernetes-sigs/kro/pull/679) and
[ExternalRefs](https://kro.run/docs/concepts/resource-group-definitions#using-externalref-to-reference-objects-outside-the-resourcegraphdefinition).
We extend `externalRef` to support watching a collection of objects, rather than just a single
object.

## Motivation

The Kubernetes’ operational model was designed with two tiers of responsibility: cluster
administration and application development. Cluster administrators pre-configure globally scoped
APIs for compute (NodePools), storage (StorageClass), networking (GatewayClass), permissions (RBAC).
This model extends to KRO, where cluster administrators configure RGDs that provide higher-order
interfaces that abstract application definition. Together, these policies enforce a set of
guardrails for application development. Once they are in place, the cluster is considered
“application ready”, and application developers can begin to apply namespace scoped APIs to deploy
their applications. In practice, these guardrails evolve over time, but the mental model is
sufficient for the purposes of this design.

While KRO enables cluster administrators to create RGDs that define new APIs to enforce application
guardrails, it requires application developers to “buy-in” and begin using them. This can be
challenging in large multi-tenant environments that contain large numbers of application teams that
leverage existing and heterogeneous tooling. Further, all policies must be bundled together into a
single monolithic RGD. This requires cluster administrators to centralize all application guardrails
into a single monolithic configuration. Decorators provide a solution to these problems.

## API and Behavior Changes

1. Introduce `Selector` to `ExternalRefMetadata`, mutually exclusive with `Name`
2. If `Name` is set, `ExternalRef` refers to a single resource (scalar)
3. If `Selector` is set, `ExternalRef` refers to a collection of matching resources
4. `Kind` uses the standard resource kind (e.g. `Deployment`, not `DeploymentList`)
5. `NamespaceSelector` is out of scope for this KREP — ordering items across multiple namespaces
   into a single collection is undefined

```
type ExternalRefMetadata struct {
    // Name is the name of the external resource to reference.
    // Mutually exclusive with Selector.
    // +kubebuilder:validation:Optional
    Name string `json:"name,omitempty"`
    // Namespace is the namespace of the external resource.
    // If empty, the instance's namespace will be used.
    // +kubebuilder:validation:Optional
    Namespace string `json:"namespace,omitempty"`
    // Selector is a label selector for collection external references.
    // When set, all resources matching the selector are included.
    // Mutually exclusive with Name.
    // +kubebuilder:validation:Optional
    Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

type ExternalRef struct {
    // +kubebuilder:validation:Required
    APIVersion string `json:"apiVersion"`
    // +kubebuilder:validation:Required
    Kind string `json:"kind"`
    // +kubebuilder:validation:Required
    Metadata ExternalRefMetadata `json:"metadata"`
}
```

## Examples

The power of the decorator pattern is best understood through concrete examples.

### Example 1: Decorating a Kind

As a cluster administrator, I want to configure VPA in recommender mode for existing deployments in
my cluster to see whether or not widespread rollout of VPA would provide significant cost savings.
I'm going to start with an opt-in approach by labeling deployments, though I plan to reduce scoping
as I gain confidence. Eventually, I plan to flip VPA into auto mode.

```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: decorate-deployments-with-vpas
spec:
  schema:
    apiVersion: v1
    group: example.com
    kind: Decorator
  resources:
    - id: deployments
      externalRef:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          selector:
            matchLabels:
              enable-vpa-recommendation: "true"
    - id: vpas
      forEach: ${ deployments.filter(d, d.spec.replicas > 0) } # Optional filtering
      template:
        apiVersion: autoscaling.k8s.io/v1
        kind: VerticalPodAutoscaler
        metadata:
          name: ${each.metadata.name}-vpa
          namespace: ${each.metadata.namespace}
        spec:
          targetRef:
            apiVersion: apps/v1
            kind: Deployment
            name: ${each.metadata.name}
          updatePolicy:
            updateMode: "Off"
          resourcePolicy:
            containerPolicies:
            - containerName: '*'
              minAllowed:
                memory: 50Mi
              maxAllowed:
                memory: 500Mi
              controlledResources: ["memory"]
```

### Example 2: Decorating a Namespace

As a Cluster Administrator, I want to enable application teams to self-service namespaces. However,
I want to provide a default LimitRange to ensure that minimum cpu and memory are defined. I can’t
create these resources until after the namespace is created, so I need a way to dynamically create
these objects when namespaces are created.

```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: decorate-namespaces-with-limitranges
spec:
  schema:
    apiVersion: v1
    group: example.com
    kind: Decorator
  resources:
    - id: namespaces
      externalRef:
        apiVersion: v1
        kind: Namespace
        metadata:
          selector: {} # Empty selector matches all namespaces
    - id: limitranges
      forEach: ${ namespaces }
      template:
        apiVersion: v1
        kind: LimitRange
        metadata:
          name: ${each.metadata.name}-limits
          namespace: ${each.metadata.name}
        spec:
          limits:
          - type: Container
            min:
              cpu: 100m
              memory: 128Mi
```

### Example 3: Decorating Multiple Resources into a single Aggregated Resource

As a ClusterAdministrator, I want to define an Ingress resource that routes to all of the http
Services in my cluster. However, each time a service comes and goes, I need to go update my Ingress
again. I want to dynamically update my ingress each time a service comes and goes in the Cluster.

```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: aggregate-services-to-ingress
spec:
  schema:
    apiVersion: v1
    group: example.com
    kind: Decorator
  resources:
    - id: services
      externalRef:
        apiVersion: v1
        kind: Service
        metadata:
          selector: {} # Empty selector matches all services
    - id: ingress
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: services-ingress
        spec:
          rules:
          - http:
              paths: |
                ${ services.filter(s, s.spec.ports.exists(p, p.name == "http")).map(s, {
                  "path": "/" + s.metadata.namespace + "/" + s.metadata.name,
                  "pathType": "Prefix",
                  "backend": {
                    "service": {
                      "name": s.metadata.name,
                      "port": {
                        "name": "http"
                      }
                    }
                  }
                }).sort(p, p.path) }
```

## The Blast Radius Problem

Decorators have broad blast radius. A small change to a decorator has the potential to impact every
object in the cluster of a given kind. However, this problem is not new to decorators is is
identical to the blast radius problem inherent to collections. We consider this to be an opportunity
for a future KREP.

## The Singleton Problem

ResourceGraphDefinitions result in the creation of a new `kind` defined in the `schema`, which
allows for the creation of multiple instances of the `kind`. We assert that this is an anti-pattern
for most decorators, which should only ever have a single instance. If multiple instances of a
decorator are defined they will take the same actions and cause write conflicts. We refer to this
class of RGD as `Singletons`. Notably singletons don't require any spec or status in the `schema`.
They only contain `apiVersion` and `kind` because those are required to instantiate the RGD. It's
possible to define RGDs that use the decorator pattern AND contain schema (i.e., dynamic
decorators), but use cases that demand this flexibility are unclear.

The singleton pattern is useful beyond decorators. For example, software that uses the Kubernetes
Operator Pattern (i.e. KRO, Karpenter, etc) is intended to be installed a **single** time in a
cluster. These components contain cluster-scoped objects (e.g. CRDs) that can only be installed
once. Customers looking to deploy these components using KRO would need to be careful to only
instantiate the RGD once. Further, they must first create an RGD and then instantiate it without any
schema. This is cumbersome and could be collapsed into a single API without losing functionality.

Singletons are similar to how customers use helm today -- it's possible to deploy a chart multiple
times, and customers self-enforce these singleton properties. For similar reasons, we don't consider
this to be blocking for KREP-003 Decorators. We consider this to be an opportunity for a future
KREP.

Directionally, we posit that `singletons` could take the following form:

1. The `schema` field is no longer required in RGDs
2. An RGD without a `schema` would self-instantiate and create all of its resources as a singleton
3. An RGD without a `schema` becomes the root node of the DAG, rather than the RGD's instance
