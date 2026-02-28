# KREP-013 Polymorphism (Mutable ExternalRef)

## Summary

Today, an externalRef is read-only -- KRO observes a resource it doesn't own but never writes to it.
This KREP makes externalRef mutable: fields declared with expressions become a **partial** -- a
subset of fields that KRO contributes back to the resource via server-side apply, while leaving all
other fields untouched.

This single change closes the loop between observation and contribution. A Decorator (a schemaless
RGD, defined in [KREP-003](https://github.com/kubernetes-sigs/kro/pull/738)) can already watch
resources it doesn't own and create downstream objects in response. With mutable externalRef, it can
also write status back to the resources it watches. Combined with the standalone
[Schema API (#1076)](https://github.com/kubernetes-sigs/kro/issues/1076) -- which separates the CRD
definition from any implementation -- these three capabilities together produce a trait system:
Schemas define contracts, Decorators implement them, and mutable externalRef is the mechanism that
lets implementations report back.

## Motivation

Today, a ResourceGraphDefinition bundles three concerns:

1. **Schema** -- the CRD definition (the contract)
2. **Status** -- computed values written back to the instance (feedback to the user)
3. **Resources** -- the graph of downstream objects (the implementation)

This bundling means every API definition must include its implementation inline, and every
implementation is bound to the API it defines. There is no way to define a behavioral contract
independently, no way to provide different implementations for the same contract, and no way to
compose multiple independent behaviors onto a single type.

## Scope

### In scope

- **Mutable externalRef** -- an externalRef becomes a partial. Fields declared with expressions are
  contributed back via server-side apply. KRO analyzes the target type to discover subresources and
  routes writes to the correct API endpoint.

### Not in scope

- **Schema API** -- Standalone Schema CRD is covered by
  [#1076](https://github.com/kubernetes-sigs/kro/issues/1076).
- **Decorators** -- Schemaless RGDs and collection watching are covered by
  [KREP-003](https://github.com/kubernetes-sigs/kro/pull/738).
- **Cross-GVK label selection** -- Decorators select instances of a specific GVK by label. A
  single Decorator does not watch across multiple kinds. Each GVK requires its own informer.

### Prerequisites

Mutable externalRef can be implemented independently for a single named resource -- an externalRef
that points to one resource by name and contributes fields back to it. This is useful on its own.

However, the trait system described in this document requires all three capabilities:

1. **Schema API** -- to define contracts independently of implementation
2. **Decorators** -- to watch collections of resources by label (selector-based externalRef)
3. **Mutable externalRef** (this KREP) -- to write contributed fields back

These can land in any order. The trait system activates when all three are present.

## Proposal

### Mutable ExternalRef

Today, an externalRef is read-only -- it observes a resource KRO doesn't own but never writes
back. A mutable externalRef adds a write path: any fields declared with `${...}` expressions are
**contributed** back to the resource via server-side apply. Fields without expressions remain
read-only. No expressions means read-only (today's behavior).

```yaml
# Read-only (today) -- observe, no contribution
- id: app
  externalRef:
    apiVersion: example.com/v1alpha1
    kind: Application
    metadata:
      name: my-app

# Mutable -- observe and contribute status back
- id: app
  externalRef:
    apiVersion: example.com/v1alpha1
    kind: Application
    metadata:
      name: my-app
    status:
      ready: ${deployment.status.readyReplicas == deployment.status.replicas}
```

Any RGD can use mutable externalRef -- not just Decorators. A standard RGD with an inline schema
can reference an external resource by name and contribute fields back to it. The Decorator pattern
(schemaless RGD + selector-based watching) is the most powerful application, but the mechanism is
general.

#### `each` in Contributed-Field Expressions

When a selector-based externalRef watches a collection of resources, it contributes fields back to
each instance individually. The `each` variable binds to the current instance being written to.

```yaml
- id: apps
  externalRef:
    apiVersion: example.com/v1alpha1
    kind: Application
    selector:
      matchLabels:
        kro.run/trait-application: "true"
    status:
      ready: ${deployments[each.metadata.name].status.readyReplicas == each.spec.replicas}
```

For each selected Application, KRO evaluates the expression with `each` bound to that instance
and issues a separate SSA Apply call with the result.

This is a new evaluation context for `each`. Today, `each` only exists in `readyWhen` for
per-item readiness checks on collections. Contributed-field expressions extend `each` to a write
context. The runtime must be updated to inject `each` into contributed-field expression evaluation.

Note that `each` is distinct from `forEach` variables. A `forEach` block (e.g.,
`forEach: [{app: ${apps}}]`) uses a user-named variable (`app`) that binds to one item during
template expansion. `each` is a built-in variable that binds to the current instance of a
selector-based externalRef during per-instance expression evaluation. They serve different roles:
`forEach` expands templates, `each` evaluates contributed expressions.

### ExternalRef API Change

The current `ExternalRef` Go struct is a rigid typed object:

```go
type ExternalRef struct {
    APIVersion string              `json:"apiVersion"` // +required
    Kind       string              `json:"kind"`       // +required
    Metadata   ExternalRefMetadata `json:"metadata"`   // +required
}
```

This struct has no room for contributed fields (`status:`) or label selection (`selector:`). To
support mutable externalRef, the struct must change. Three options:

**Option A: Add explicit fields.** Add `Selector *metav1.LabelSelector`, `Spec
*runtime.RawExtension`, and `Status *runtime.RawExtension` to the existing struct. This is the
most conservative change -- the typed fields remain, and contributed fields go into raw extension
blocks that are parsed dynamically. Selector-based externalRef (from the Decorators KREP) makes
`Metadata` optional when `Selector` is present.

**Option B: Replace with RawExtension.** Make `ExternalRef` a `runtime.RawExtension`, like
`template`. The entire block is parsed dynamically. Maximum flexibility, but loses compile-time
type safety for `apiVersion`/`kind`.

**Option C: Hybrid.** Keep `apiVersion`/`kind` as typed fields. Replace `metadata` with a union:
either `metadata` (single resource) or `selector` (collection). Add `spec` and `status` as
`runtime.RawExtension` for contributed fields.

This KREP proposes **Option A** as the initial implementation. It is the smallest API change, it
is backward compatible (all new fields are optional), and it cleanly separates selection
metadata from contributed fields.

### Subresource Routing

KRO must route contributed fields to the correct API endpoint. A contributed `status:` block must
be applied to the `/status` subresource endpoint, not the main resource endpoint. This requires
new infrastructure:

1. **Field analysis** -- at graph build time, the builder inspects which contributed fields belong
   to `.status` vs `.spec` (or other top-level fields).
2. **Dual apply** -- if both `.spec` and `.status` fields are contributed, KRO issues two SSA
   Apply calls: one to the main endpoint, one to the `/status` subresource.
3. **Subresource detection** -- not all CRDs enable the status subresource. The builder checks
   the CRD's `subresources` spec and errors at build time if the externalRef contributes `.status`
   fields to a resource that doesn't have a status subresource.

The RGD author doesn't think about subresources -- they declare the fields they contribute, and
KRO handles the routing.

### Field Ownership

Kubernetes server-side apply is the natural implementation for partial writes. Each Decorator
becomes a distinct field manager, and SSA tracks field-level ownership natively.

**Field manager naming.** Each Decorator's field manager is named `kro.run/<rgd-name>` (e.g.,
`kro.run/application-controller`, `kro.run/networking-controller`). This is distinct from the
primary RGD controller's field manager (`kro.run/applyset`), ensuring that a Decorator's
contributed fields never conflict with the primary controller's owned fields.

**Conflict detection.** When two field managers claim the same field without force-apply, SSA
returns a conflict error. This means two trait implementations are claiming the same field -- a
boundary violation. See [Discussion: Conflict Resolution](#conflict-resolution) for how KRO
should surface and handle these conflicts.

**Decorator deletion.** When a Decorator is removed, its contributed fields remain on the resource
with their last written values -- SSA does not automatically clean up fields when a field manager
stops writing. See [Discussion: Cleanup Semantics](#cleanup-semantics) for options.

**Interaction with the primary controller.** A resource may be owned by one RGD (which creates it
via template) and observed by multiple Decorators (which contribute fields via mutable externalRef).
The primary controller owns `.spec` and `.metadata` through its field manager. Decorators typically
contribute `.status` fields. If a Decorator contributes `.spec` fields, it must not conflict with
the primary controller's field manager -- SSA enforces this boundary.

Note that the primary controller uses an ApplySet field manager (`kro.run/applyset`), which
carries KEP-3659 semantics (membership labels, prune scope). Decorator field managers
(`kro.run/<rgd-name>`) are plain SSA field managers -- they do not participate in ApplySet pruning.
This asymmetry is intentional: the primary controller owns the resource lifecycle, Decorators only
contribute fields.

### A Trait System

The mechanism described above -- mutable externalRef with SSA field ownership -- produces a trait
system when combined with standalone Schemas and Decorators:

- A **Schema** is a **trait definition** -- it declares a behavioral contract (fields, status
  shape) without specifying how that behavior is provided.
- A **Decorator** is a **trait implementation** -- it selects instances of a GVK by label,
  creates downstream resources via templates, and contributes status back through mutable
  externalRef.
- **Labels** are the **adoption mechanism** -- a resource opts into a trait by carrying labels
  that match a Decorator's selector.
- **SSA field ownership** keeps trait implementations independent -- each Decorator is a distinct
  field manager, and Kubernetes enforces boundaries at the field level.

Whether you swap a Decorator to change how a trait is implemented, or compose multiple Decorators
to layer traits onto a type, the machinery is the same.

## Examples

### Single Trait

Consider a bundled RGD that defines an Application API:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: application
spec:
  schema:
    apiVersion: v1alpha1
    group: example.com
    kind: Application
    spec:
      image: string
      replicas: integer | default=1
    status:
      ready: ${deployment.status.readyReplicas == deployment.status.replicas}
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        # ...
```

This is monomorphic -- one API, one implementation, permanently coupled. With standalone Schemas
and mutable externalRef, the same abstraction decomposes into a trait definition and a trait
implementation:

```yaml
# Trait definition -- declares the contract, owns no implementation
apiVersion: kro.run/v1alpha1
kind: Schema
metadata:
  name: application
spec:
  apiVersion: v1alpha1
  group: example.com
  kind: Application
  spec:
    image: string
    replicas: integer | default=1
  status:
    ready: boolean
```

```yaml
# Trait implementation -- watches Applications by label, creates resources,
# writes status back via mutable externalRef
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: application-controller
spec:
  resources:
    # Mutable externalRef -- selects by label, contributes status.ready
    # `each` binds to the current Application instance being written to
    - id: apps
      externalRef:
        apiVersion: example.com/v1alpha1
        kind: Application
        selector:
          matchLabels:
            kro.run/trait-application: "true"
        status:
          ready: ${deployments[each.metadata.name].status.readyReplicas == each.spec.replicas}
    - id: deployments
      forEach:
        - app: ${apps}
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${app.metadata.name}
          namespace: ${app.metadata.namespace}
        spec:
          replicas: ${app.spec.replicas}
          # ...
```

The contract and implementation are now independent. A different Decorator could implement the
same trait by creating Knative Services instead of Deployments.

### Trait Composition

An Application needs compute, but it also needs networking. Rather than bundling both into a
single monolithic RGD, each concern becomes its own trait -- an independent Decorator that
contributes its behavior and writes its status back.

The Application Schema declares status fields for both traits:

```yaml
apiVersion: kro.run/v1alpha1
kind: Schema
metadata:
  name: application
spec:
  apiVersion: v1alpha1
  group: example.com
  kind: Application
  spec:
    image: string
    replicas: integer | default=1
    port: integer | default=80
  status:
    ready: boolean
    endpoint: string
```

A Networking Decorator creates a Service for each Application and writes the endpoint back:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: networking-controller
spec:
  resources:
    - id: apps
      externalRef:
        apiVersion: example.com/v1alpha1
        kind: Application
        selector:
          matchLabels:
            kro.run/trait-networking: "true"
        status:
          endpoint: ${services[each.metadata.name].spec.clusterIP + ":" + string(each.spec.port)}
    - id: services
      forEach:
        - app: ${apps}
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${app.metadata.name}
          namespace: ${app.metadata.namespace}
        spec:
          selector:
            app: ${app.metadata.name}
          ports:
            - port: ${app.spec.port}
```

A user creates an Application that opts into both traits:

```yaml
apiVersion: example.com/v1alpha1
kind: Application
metadata:
  name: my-app
  labels:
    kro.run/trait-application: "true"
    kro.run/trait-networking: "true"
spec:
  image: nginx:latest
  replicas: 3
  port: 8080
```

The Application Decorator fires because `my-app` carries `kro.run/trait-application: "true"` --
it creates a Deployment and writes `status.ready`. The Networking Decorator fires because `my-app`
carries `kro.run/trait-networking: "true"` -- it creates a Service and writes `status.endpoint`.
Each Decorator is a separate field manager (`kro.run/application-controller` and
`kro.run/networking-controller`), manages its own fields via SSA, and knows nothing about the
other. The traits compose without coordination.

## Discussion

### Conflict Resolution

When two Decorators write to the same field, server-side apply returns a conflict error. In the
trait model, this means two trait implementations are claiming the same field -- a boundary
violation. KRO must decide how to surface this: fail the Decorator's reconciliation, log a warning,
or expose the conflict in the Decorator's status conditions. The right behavior likely depends on
whether the conflict is accidental (a misconfiguration) or intentional (two Decorators legitimately
competing). Should KRO allow force-apply to override conflicts, or should conflicts always be hard
errors?

### Backward Compatibility

The ExternalRef API change (Option A) is backward compatible. All new fields (`selector`, `spec`,
`status`) are optional. Existing RGDs with read-only externalRef blocks continue to work unchanged.
Selector-based externalRef is a separate mechanism from forEach and uses a different code path --
forEach expands templates, selector-based externalRef watches existing resources.

### Cleanup Semantics

When a Decorator is deleted, its contributed fields remain on target resources with their last
written values. SSA field manager removal does not zero or delete the fields themselves -- it only
removes ownership tracking. KRO has several options:

1. **Explicit cleanup** -- on Decorator deletion, apply zero/null values for all contributed fields
   using the Decorator's field manager, then remove the field manager. This actively clears the
   contributed state.
2. **Ownership removal only** -- remove the field manager's ownership claims (via an empty SSA
   apply), leaving field values as-is. The fields become unmanaged and can be claimed by another
   writer.
3. **No action** -- leave the fields and ownership as-is. The stale field manager remains in
   `managedFields` until another writer claims those fields.

Option 1 is the safest default for status fields, as stale status is worse than absent status.

### Evaluation Order

The examples in this document show a pattern where a mutable externalRef's contributed expressions
reference a forEach-expanded resource (e.g., `deployments[each.metadata.name]`), and that
forEach resource depends on the externalRef collection (e.g., `forEach: [{app: ${apps}}]`). This
appears circular but is not -- it is a two-phase evaluation:

1. **Observe and expand** -- KRO reads the selected Application instances (`apps`), then expands
   the forEach resources (`deployments`) based on the observed collection.
2. **Contribute** -- after downstream resources are reconciled and their state is observed, KRO
   evaluates the contributed-field expressions on the externalRef and applies the results back.

The contribution is a second pass, not a DAG edge in the traditional sense. The graph builder must
distinguish between dependency edges (resource A needs resource B to exist) and contribution edges
(resource A writes back to resource B after reconciliation). Contribution edges point "backwards"
in the DAG and are evaluated after the forward pass completes.

### Naming

`externalRef` is overspecified. The natural concept is a **reference** -- you don't own the object,
but you point to it and can contribute through it. `ref` is sufficient and more consistent with
the mutable semantics introduced here. A rename is a follow-up concern, not in scope for this KREP.

## Pending

The following sections are deferred until the design is further along:

- Other solutions considered
- Testing strategy
