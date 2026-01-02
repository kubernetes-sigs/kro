# Allow `includeWhen` to Reference Upstream Resources

## Problem statement

Today, `includeWhen` expressions can only reference `schema`. This works for
conditions that are purely derived from instance input, but it breaks down as
soon as inclusion needs to depend on observed resource state.

That limitation blocks a common class of workflows:

- gating a resource on data returned by an `externalRef`
- gating a resource on fields exposed by an upstream resource in the same graph
- expressing conditional subgraphs directly in the DAG instead of copying
  external state into `schema.spec`

For example, a user may want to create a bucket only when an externally managed
ConfigMap enables the feature:

```kro
resources:
  - id: featureFlags
    externalRef:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: feature-flags
        namespace: ${schema.spec.namespace}

  - id: bucket
    includeWhen:
      - ${featureFlags.data.enableBucket == "true"}
    template:
      apiVersion: s3.services.k8s.aws/v1alpha1
      kind: Bucket
      metadata:
        name: ${schema.metadata.name}-bucket
```

This is not expressible today because `includeWhen` is treated as a schema-only,
runtime-evaluated boolean gate. The graph builder does not infer dependencies
from `includeWhen`, and the runtime does not have a safe model for waiting on
referenced dependencies before deciding whether to include or skip a resource.

As a result, users are pushed toward weaker alternatives:

- duplicating external state into instance schema
- moving graph-shaping logic into templates instead of the dependency model
- splitting one logical graph into multiple RGDs

That is awkward for users and inconsistent with the rest of KRO's CEL-driven
features, which already participate in graph analysis.

## Proposal

Allow `includeWhen` to reference `schema` and upstream resources, and make those
references participate in dependency graph construction.

This proposal treats `includeWhen` as a first-class graph signal rather than a
special-case runtime filter. Once `includeWhen` can depend on resource state,
KRO must do two things:

1. infer dependency edges from `includeWhen` expressions during graph build
2. evaluate those expressions with dependency-aware runtime semantics

#### Overview

The proposal introduces the following behavior:

- `includeWhen` may reference `schema` and resources in the same graph
- references discovered in `includeWhen` add DAG edges
- `includeWhen` evaluation waits for referenced dependencies to be ready
- exclusion remains contagious to downstream dependents
- a condition change from `true` to `false` triggers normal prune behavior

The end result is that resource inclusion becomes graph-aware without requiring
new syntax.

#### Design details

##### Allowed references

An `includeWhen` expression may reference:

- `schema`
- any resource declared in the same `ResourceGraphDefinition`

This includes `externalRef` resources and template-backed resources.

References to undeclared resources remain invalid.

The proposal does not allow cycles. If `includeWhen` introduces a cycle, graph
construction must fail in the same way as any other dependency cycle.

##### Graph construction

During graph building, KRO should parse `includeWhen` expressions and extract
resource references using the same general dependency-discovery flow already
used for other CEL-driven features.

For each resource:

1. parse every `includeWhen` expression
2. collect referenced resource identifiers
3. add dependency edges from each referenced resource to the current resource
4. run the normal cycle detection and topological sort

For the example above, the graph builder would infer:

```text
featureFlags -> bucket
```

That ensures `bucket` is never evaluated before `featureFlags` is ready as an
upstream dependency.

##### Validation and type checking

`includeWhen` expressions should continue to be validated when the
`ResourceGraphDefinition` is created.

Validation rules:

- the expression must be valid CEL
- the expression must return `bool` or `optional<bool>`
- every identifier must resolve to `schema` or a declared resource
- referenced fields must exist in the corresponding schemas

Type checking should use the same schema information already available to the
builder so that resource-backed `includeWhen` expressions get field-level
validation instead of failing later at runtime.

##### Runtime evaluation model

Allowing resource references changes the runtime semantics of `includeWhen`.
There are now three meaningful outcomes:

- `true`: include the resource
- `false`: exclude the resource
- `pending`: a referenced dependency is not yet ready, or the condition cannot
  yet be decided from available dependency state

`pending` is required. Without it, KRO would have to treat incomplete upstream
state as either a hard error or an implicit `false`, and both are incorrect:

- treating missing data as an error causes noisy reconcile failures for normal
  dependency timing
- treating missing data as `false` risks skipping or pruning resources too early

Runtime behavior should therefore be:

- if a referenced dependency is not yet ready, `includeWhen` remains pending
- if all expressions evaluate to `true`, the resource is reconciled
- if any expression evaluates to `false`, the resource is skipped
- if expression evaluation fails because the expression itself is invalid, the
  reconcile should fail as it does for other invalid CEL usage

This aligns resource-backed `includeWhen` with existing reconciliation
sequencing, where a resource is not reconciled until its dependencies are both
resolved and ready.

##### Contagious exclusion

Existing skip propagation should remain unchanged.

If resource `B` is excluded by `includeWhen`, every downstream resource that
depends on `B` is also excluded, regardless of whether that dependency came
from:

- template interpolation
- `forEach`
- `readyWhen`
- another `includeWhen`

This preserves an important graph invariant: KRO should never reconcile a node
whose prerequisites are absent from the realized graph.

##### Condition changes over time

Once `includeWhen` can reference resource state, inclusion is no longer a
strictly create-time decision. A referenced resource may change, causing an
`includeWhen` condition to flip after the dependent resource already exists.

When a previously included resource becomes excluded:

- the resource should stop being applied
- previously managed objects should be pruned
- downstream resources excluded because of the dependency chain should also be
  pruned

When a condition changes from `pending` to `true`, reconciliation should proceed
normally.

##### Example

The following example shows the intended end-to-end behavior:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: bucket-with-flags
spec:
  schema:
    apiVersion: example.com/v1alpha1
    kind: BucketWithFlags
    spec:
      namespace: string | required=true

  resources:
    - id: featureFlags
      externalRef:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: feature-flags
          namespace: ${schema.spec.namespace}

    - id: bucket
      includeWhen:
        - ${featureFlags.data.enableBucket == "true"}
      template:
        apiVersion: s3.services.k8s.aws/v1alpha1
        kind: Bucket
        metadata:
          name: ${schema.metadata.name}-bucket

    - id: bucketAccessPolicy
      template:
        apiVersion: iam.services.k8s.aws/v1alpha1
        kind: Policy
        metadata:
          name: ${schema.metadata.name}-bucket-access
        spec:
          name: ${schema.metadata.name}-bucket-access
          policyDocument: |
            {
              "Version": "2012-10-17",
              "Statement": [
                {
                  "Effect": "Allow",
                  "Action": ["s3:GetObject"],
                  "Resource": "arn:aws:s3:::${bucket.metadata.name}/*"
                }
              ]
            }
```

Expected behavior:

- the graph builder infers `featureFlags -> bucket`
- `bucketAccessPolicy` depends on `bucket` through
  `arn:aws:s3:::${bucket.metadata.name}/*` in `policyDocument`
- if `featureFlags.data.enableBucket == "true"`, both `bucket` and
  `bucketAccessPolicy` are reconciled
- if the expression evaluates to `false`, `bucket` is skipped and
  `bucketAccessPolicy` is skipped as a downstream dependent
- if the ConfigMap dependency is not yet ready, `bucket` remains pending rather
  than failing or being skipped prematurely

## Scoping

#### What is in scope for this proposal?

- allowing `includeWhen` to reference resources in the same graph
- inferring DAG edges from `includeWhen` expressions
- cycle detection for `includeWhen`-introduced dependencies
- pending-aware runtime evaluation for resource-backed `includeWhen`
- preserving downstream skip propagation
- prune behavior when resource-backed conditions flip from `true` to `false`
- unit and integration test coverage for the new semantics

#### What is not in scope?

- new `includeWhen` syntax
- changing `readyWhen` semantics
- allowing cyclic dependencies
- per-item conditional inclusion for collections
- cross-instance or cross-RGD references

## Discussion and notes

Open questions for review:

- should unresolved optional fields evaluate to `pending`, `false`, or require
  explicit CEL handling by the author?
