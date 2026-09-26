# KREP-019: Deferred Fields and Soft Dependencies

## Problem statement

Today KRO treats data dependencies as hard edges. If resource `A` references
resource `B`, then `A` must wait for `B`, and dependency cycles are rejected.
That is the right default, but it rules out graphs where both resources can be
created first and only need each other's data later.

The missing capability is an explicit way to say "this field may be resolved
after initial creation." Without that, ordinary resource templates fail with
`ErrDataPending` as soon as a referenced value is absent, so a graph that could
converge across reconciles is rejected as cyclic up front.

KRO already has a narrow precedent for this behavior: instance status can be
partially resolved even when some inputs are still missing. This proposal
extends that idea to ordinary resource templates, but only when the author
explicitly marks a field as deferred.

## Proposal

Add a KRO-specific expression form `${defer(expr)}` for standalone template
fields.

In one sentence: `defer()` turns the dependencies of `expr` into soft
dependencies, so the field can be resolved later and its effective value is
`null` while the referenced data is still pending.

`defer(expr)` means:

- the compiler parses and type-checks `expr` as an ordinary CEL expression
- the compiler records the dependencies of `expr` as soft rather than hard
- the runtime does not block initial creation of the resource on those soft
  dependencies being ready
- when `expr` cannot yet be evaluated because data is pending, the effective
  value of the deferred field for that reconcile is `null`
- reconciliation continues until the deferred field either resolves or reaches
  a terminal failure

For v1, `defer()` is only valid as the outermost wrapper of a standalone field
expression, optionally composed with `omitWhen(null)` from [KREP-018].

The key graph rule is: cycles are checked on the hard-dependency graph. If the
graph formed by hard dependencies is acyclic, the resource graph is valid even
if the full graph still contains cycles that pass through soft dependencies. A
cycle made entirely of hard dependencies remains invalid.

If the author wants omission rather than `null`, that should be explicit:

- `${defer(expr)}` means "treat pending as `null`"
- `${defer(expr).omitWhen(null)}` using `omitWhen(...)` from [KREP-018] means
  "treat pending as `null`, then omit the field when the effective value is
  `null`"

Example using `omitWhen(null)` from
[KREP-018]:

```yaml
resources:
  - id: service
    template:
      apiVersion: v1
      kind: Service
      metadata:
        name: ${schema.metadata.name}
      spec:
        type: LoadBalancer

  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.metadata.name}
      spec:
        template:
          spec:
            containers:
              - name: app
                image: ${schema.spec.image}
                env:
                  - name: PUBLIC_ENDPOINT
                    value: ${defer(service.status.loadBalancer.ingress[0].hostname).omitWhen(null)}
```

On the first reconcile, the deployment can be created without
`PUBLIC_ENDPOINT`. Once the service publishes an external hostname, the next
reconcile resolves the deferred expression and updates the deployment with the
field present.

The same mechanism applies to true cycles, again using `omitWhen(null)` from
[KREP-018]. A more concrete
example is two ACK EC2 `SecurityGroup` resources that need to allow traffic
from one another by AWS security group ID:

```yaml
resources:
  - id: leftSG
    template:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: SecurityGroup
      metadata:
        name: left-sg
      spec:
        name: left-sg
        description: left security group
        vpcRef:
          from:
            name: shared-vpc
        ingressRules:
          - ipProtocol: tcp
            fromPort: 443
            toPort: 443
            userIDGroupPairs:
              - description: allow right SG
                groupID: ${defer(rightSG.status.id).omitWhen(null)}

  - id: rightSG
    template:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: SecurityGroup
      metadata:
        name: right-sg
      spec:
        name: right-sg
        description: right security group
        vpcRef:
          from:
            name: shared-vpc
        ingressRules:
          - ipProtocol: tcp
            fromPort: 443
            toPort: 443
            userIDGroupPairs:
              - description: allow left SG
                groupID: ${defer(leftSG.status.id).omitWhen(null)}
```

Without `defer()`, this graph is cyclic and would be rejected because each node
depends on the other's `status.id` before it can be fully resolved. With
`defer()`, both security groups can be created first with the peer rule omitted.
Once ACK reports each group's `status.id`, later reconciles fill in the missing
`groupID` values and the graph converges.

## Semantics

`defer()` is not a general-purpose CEL error suppression mechanism. It is a
compiler-recognized wrapper around an ordinary CEL expression.

The compiler still type-checks the inner expression normally and keeps it as the
expression that will be evaluated at runtime. `defer()` changes how KRO treats
that field when evaluating the inner expression returns `ErrDataPending`.

The contract is:

- if `expr` evaluates successfully, use its value normally
- if evaluating `expr` returns `ErrDataPending`, the field stays pending and
  the effective result of `defer(expr)` is `null` for that reconcile
- if `expr` fails with any other CEL/type/runtime error, surface that error
  normally

That means `defer()` only softens data-pending behavior. It does not hide real
errors.

## Soft Dependencies and Cycle Admission

`defer()` introduces soft dependencies into the graph.

Ordinary dependencies remain hard:

- the runtime waits for them
- readiness and exclusion propagate through them normally

Deferred dependencies are different:

- the compiler still records them
- the runtime does not block initial resolution on them
- they do not prevent initial resolution of the node

Cycle validation should therefore operate on the hard-dependency graph, not on
the full graph including soft dependencies. If removing soft dependencies makes
the graph acyclic, the graph is valid. If a cycle remains among hard
dependencies, the graph is invalid.

This is what allows `defer()` to break otherwise-hard dependency cycles
temporally across reconciles instead of rejecting them outright.

## Relationship to KREP-018

KREP-018 (PR #1125) covers partial dependencies in branching expressions. That
proposal is about determining which dependencies are actually required on the
selected path through an expression.

This KREP solves a different problem. Here, the dependencies are known, but the
author wants some of them to be soft so the resource can be created first and
the missing values can be resolved in later reconciles.

## Null and omission

Bare `defer()` should not omit a field by itself. Its default behavior is to
produce an effective `null` while the deferred value is still pending.

That keeps the feature aligned with its real purpose: dependency timing. The
job of `defer()` is to turn a hard edge into a soft edge and let the graph make
progress. Whether a `null` result should be written or omitted is a separate
field-resolution decision.

When omission is desired, the author should compose `defer()` with
`omitWhen(null)` from
[KREP-018]:

- `${defer(expr)}` -> pending becomes `null`
- `${defer(expr).omitWhen(null)}` using `omitWhen(...)` from [KREP-018] ->
  pending becomes `null`, then the field is omitted because the effective
  value is `null`

This keeps the two features distinct:

- `defer()` controls dependency timing and cycle admission
- `omitWhen(...)` from [KREP-018] controls whether a successfully produced
  value should be written to the output

For v1, this composition should only be valid in standalone field expressions
where field omission is well-defined.

In scope:

- `${defer(expr)}` as the whole field value

Out of scope:

- string interpolation fragments like `"x-${defer(expr)}"`
- list-element omission
- map-entry omission

## Convergence

Deferred expressions are re-evaluated every time KRO walks the graph during
reconciliation. If a deferred expression still returns `ErrDataPending`, KRO
does not fail the resource. It uses the field's pending value for that graph
walk (`null`, or omission if composed with `omitWhen(null)` from [KREP-018]).

`defer()` does not by itself cause the controller to keep reconciling forever.
It only changes what happens when reconciliation is already running and a field
still has pending data.

That means deferred expressions are only retried while the instance continues
to be reconciled. If the instance reaches readiness and the controller stops
reconciling, deferred expressions stop being retried.

This is why readiness matters for resources that use deferred fields. If a
deferred value must be present before the resource or instance is truly done,
that expectation needs to be reflected in readiness.

That gives KRO this behavior:

1. nodes with deferred fields can be created without waiting on every deferred
   input
2. every graph walk re-evaluates deferred expressions
3. deferred fields resolve either to `null` or, if composed with
   `omitWhen(null)` from [KREP-018], to omission while their data is pending
4. once deferred inputs exist during a later graph walk, the real values are
   resolved and applied
5. if evaluating the deferred expression starts returning a normal
   non-pending error, reconciliation fails normally
6. `defer()` alone does not keep reconciliation going after the instance is
   otherwise Ready

## Validation Rules

The compiler should enforce the following:

- `defer()` is only valid in standalone template field expressions
- for v1, `defer()` must be the outermost wrapper of the field expression,
  optionally followed by `omitWhen(null)` from [KREP-018]
- `defer()` is not valid in `includeWhen`, `readyWhen`, `forEach`, or other
  non-template CEL contexts
- the inner expression must still parse and type-check normally
- bare `${defer(expr)}` is only valid when the destination field can accept
  `null` while the expression is pending
- if the destination field cannot accept `null`, the author must provide an
  explicit fallback policy such as `${defer(expr).omitWhen(null)}` using
  `omitWhen(...)` from [KREP-018]

The compiler must also record which dependencies come from deferred
expressions so the graph can distinguish hard edges from soft edges.

## Implementation shape

The current runtime already distinguishes data-pending errors from true CEL
errors. This proposal generalizes that behavior explicitly for user templates.

The implementation should have three pieces.

First, the compiler recognizes `defer(expr)` in a standalone template field,
unwraps the inner expression, type-checks and compiles that inner expression as
ordinary CEL, and records deferred-field metadata alongside it. That metadata
includes at least:

- that the field is deferred
- the pending-value policy (`null`, or omission via composition)
- the dependencies referenced by the inner expression, classified as soft

Second, graph construction uses that metadata to distinguish hard dependencies
from soft dependencies and validates cycles on the hard-dependency graph.

Third, the resolver/runtime changes field resolution for those marked fields:

- evaluate the compiled inner expression normally
- if it succeeds, write the value
- if it returns `ErrDataPending`, use the configured pending-value policy for
  that reconcile
- if it returns any other error, fail the node normally
- on each later graph walk, evaluate the deferred expression again until it
  either succeeds or fails with a normal error

No additional global convergence-tracking mechanism is required by this KREP.

## Relationship to `omitWhen(...)` from [KREP-018]

`defer()` and `omitWhen(...)` from
[KREP-018] are related but different.

- `defer()` changes dependency semantics and turns pending evaluation into an
  effective `null`
- `omitWhen(...)` from [KREP-018] changes field-resolution semantics and omits
  a field based on the value produced by evaluation

They can be used together, and that is the preferred way to express "this field
may resolve later, and while it is pending I want it omitted rather than
written as `null`":

- `${defer(expr).omitWhen(null)}` using `omitWhen(...)` from
  [KREP-018]

The important semantic point is that `omitWhen(null)` from [KREP-018] only
affects what gets written to the manifest for the current reconcile. It does
not change dependency semantics, and it does not stop the deferred expression
from being retried on later reconciles.

## In scope

- explicit deferred template expressions via `${defer(expr)}`
- soft dependency edges in the compiled graph
- `null` as the default pending value for deferred fields
- explicit omission via `${defer(expr).omitWhen(null)}` using `omitWhen(...)`
  from [KREP-018]

## Out of scope

- changing the default strict behavior of ordinary template expressions
- making all `ErrDataPending` behavior soft everywhere
- deferred semantics in conditions or iteration expressions
- deep patch/merge semantics

## Other solutions considered

### Keep strict resolution only

This preserves the current model, but it leaves no explicit way to describe
"this field can arrive later" and keeps some otherwise-valid dependency shapes
unrepresentable.

### Silent partial resolution everywhere

That would make ordinary templates behave more like instance status does today,
but it would be too implicit. Users would lose the ability to distinguish
required fields from deferred ones, and failures would become harder to reason
about.

### Make `defer()` omit fields directly

That would couple dependency timing to one specific field-resolution policy.
Using `null` as the default pending value keeps `defer()` focused on graph
semantics, while `${defer(expr).omitWhen(null)}` using `omitWhen(...)` from
[KREP-018] gives authors explicit control when omission is the right outcome.

[KREP-018]: https://github.com/kubernetes-sigs/kro/pull/1125
