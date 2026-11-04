# externalFields: Letting External Controllers Own Specific Fields

Authors: @FalcoSuessgott

## Summary

kro resets every field it manages back to the template value on each reconcile, which means an external controller that mutates the same field (an HPA scaling `spec.replicas`, Argo Rollouts shifting a Service `spec.selector`, a Gateway API controller adjusting HTTPRoute backend weights) gets fought on every reconcile. This proposal lays out three different ways to let kro hand a specific field off to whatever else is managing it. One of them (detect then release) is already implemented in [PR #1468](https://github.com/kubernetes-sigs/kro/pull/1468) and working, but review raised real concerns about the general model, so this document compares it honestly against two alternatives rather than treating it as the only answer.

## Problem statement

[Issue #1290](https://github.com/kubernetes-sigs/kro/issues/1290) collects three concrete cases. An HPA needs to scale a Deployment's `spec.replicas`, but kro resets it to the templated value on the next reconcile. Argo Rollouts needs to rewrite a Service's `spec.selector` during a blue/green or canary rollout, and kro resets that too. A Gateway API controller adjusts an HTTPRoute's backend weights for traffic shifting, same problem. In every case kro created the resource, the resource needs an initial value, and some other controller needs to take over a specific field afterward without kro fighting it.

The only workaround today is to leave the field out of the template entirely, which works (kro never touches what it was never told to set) but forecloses ever giving the field an initial value. You cannot template `replicas: 3` and then hand it to an HPA. You can only never template `replicas` at all and let the API server default it.

## Current state

[PR #1468](https://github.com/kubernetes-sigs/kro/pull/1468) implements the first of the three options below (detect then release) end to end:

- `ExternalFields []string` on `Resource` (`api/v1alpha1/resourcegraphdefinition_types.go`) and the equivalent `Node` field in the `Graph` CRD (`api/v1alpha1/graph_types.go`), a list of field paths such as `spec.replicas` or `spec.template.spec.containers[name=nginx].env[name=FOO].value`.
- A field path parser (`pkg/graphengine/compiler/fieldpath.go`, `ParseFieldPath`) supports plain dotted fields and a `[field=value]` selector per segment that addresses one item of an associative list by matching one of its own fields, the same targeting style kustomize uses for list items.
- Compile time validation (`pkg/graphengine/compiler/compiler.go`, `analyzeExternalFields`, line 781) rejects a path whose fields do not exist in the literal template, whose selector matches no item, or that crosses into a list without a selector. The final segment must always be a plain field, addressing a whole named list item rather than one of its fields is not supported.
- Runtime handoff (`pkg/graphengine/executor/simple.go`, `releaseHandedOffFields` and `foreignFieldOwner`, lines 2192 and 2210) parses `metadata.managedFields` using `sigs.k8s.io/structured-merge-diff/v6/fieldpath`, builds the same key element representation Kubernetes itself uses for an associative list item, and omits a listed path from future applies only once a manager other than kro's own claims that exact path.
- Tested with compiler unit tests (`pkg/graphengine/compiler/compiler_test.go`), an integration suite (`test/integration/suites/core/external_fields_test.go`), and a manual run on a kind cluster using `kubectl scale`/`kubectl patch` as a stand in for a real external controller.

Review on the PR raised three concerns: whether kro should auto detect other field managers instead of requiring an explicit list, whether the implementation handles shared field owners, and whether detect then release is the right general model at all, with "only ever apply once on create" suggested as a possibly better fit. See the full discussion on [PR #1468](https://github.com/kubernetes-sigs/kro/pull/1468) for complete context and attribution.

The shared owners question already had an answer: `foreignFieldOwner` releases a field once any manager other than kro claims it, it does not need to pick between multiple foreign claimants. Whether detect then release is the right default at all remains genuinely open, which is why Options B and C exist below.

## Proposal

### Option A: detect then release (implemented)

An explicit per-resource allow-list, `externalFields: [spec.replicas]`. kro applies the field normally on create, then keeps reconciling it exactly like any other field (reverting a hand edit) until `metadata.managedFields` shows a manager other than kro's own claiming that exact path. Only then does kro stop including the field in future applies.

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
    externalFields:
      - spec.replicas
      # a [field=value] selector per segment addresses one item of an
      # associative list by matching one of its own fields, releasing just
      # that field on that item, every other field and every other item
      # stays fully managed by kro
      - spec.template.spec.containers[name=sidecar].env[name=FEATURE_FLAG].value
```

This has to be gated behind detection rather than releasing unconditionally on the first update reconcile, because releasing a field before anyone else owns it means kro was that field's only ever manager, and the next apply does not release it, it deletes it. That ordering constraint is also why the allow-list has to be explicit rather than automatic for every field: an accidental managedFields entry from a one off `kubectl patch` or an unrelated admission webhook could otherwise silently and permanently orphan a field nobody meant to hand off.

Tradeoffs: explicit and safe against accidental release, but sticky, once released a field is never reclaimed even if the external controller is later uninstalled. The selector syntax supports only a single field=value pair, a list item identified by a composite merge key such as a container port keyed by both containerPort and protocol together cannot be addressed yet, and a path cannot release a whole named list item, only a field inside one.

### Option B: create once (not implemented)

Same `externalFields` allow-list, but instead of detecting a foreign owner, the listed paths are applied exactly once, at creation, under a separate field manager identity used only for that one apply (reusing the same partial apply primitive the `Patch` node already uses, `contributeApply` in `pkg/graphengine/executor/simple.go`, line 1759). From the very next reconcile onward kro's main manager never includes those paths in any apply again, unconditionally, with no detection step at all.

This avoids the delete on release problem by construction rather than by gating. The disposable manager's single historical claim on the field persists in `metadata.managedFields` even though that manager never applies again, so the field always has at least one owner of record and the API server never treats it as ownerless.

Tradeoffs: simpler than Option A, no new dependency, no window where a stray write could trigger an unintended release since there is nothing to detect. But it never drift corrects that field again for any reason, including an accidental hand edit with no controller involved at all, and it never automatically reclaims the field if the external controller is removed later. It would use the same field path syntax as Option A, including the `[field=value]` list selector, since both options only differ in when a listed path is released, not in how a path is written or validated.

### Option C: conflict driven, no API change (not implemented)

On the RGD and instance path, kro's apply is always forced (`applyTemplateObject` in `pkg/graphengine/executor/simple.go`, line 2241, returns `s.ssaApply(ctx, obj, FieldManager, true)` whenever `s.ConflictDetection` is false). A forced apply bypasses SSA conflict checking entirely, which is the actual mechanical reason an external controller always loses today, kro never even sees a conflict.

Option C stops forcing by default and reuses the conflict handling the standalone `Graph` path already has: an unforced apply, and on a 409 a decision based on the conflict's own causes (`anyConflictOwnedByForeignGraphTemplate` and `conflictCauseManager`, lines 2116 and 1967) about whether to force reclaim (every conflicting field is kro's own stale identity) or back off (a genuinely foreign manager). This proposal extends that pattern one step further: instead of failing the whole node not ready on a foreign conflict, strip exactly the conflicting field paths named in the 409's causes from the payload and retry immediately, so the rest of the resource still converges in the same reconcile.

No RGD author ever declares anything for this to work, and no author ever has to write a field path at all. Because the trigger is a live conflict rather than a polled snapshot, it self heals in both directions: kro backs off while the field is actively contested and automatically reclaims it the moment the external controller stops writing to it. It also handles any number of foreign managers on the same field without special casing, and it handles any shape of list item, including a composite merge key such as a container port keyed by both containerPort and protocol together, because the conflict causes come from the API server's own structured merge diff machinery rather than an author written selector.

Tradeoffs: this is a global default behavior change affecting every field on every resource kro manages, with no per field opt in, which is a materially bigger blast radius than an allow-list an author must explicitly populate. It would need to ship behind a feature gate (kro already has a feature gate system, `pkg/features`) with a staged rollout rather than flipping the default outright. It also removes an author's ability to say a specific field should always be force corrected no matter who else touches it, which matters for security sensitive fields where "whoever wins the write race" is not an acceptable policy.

### Comparison

| | A, detect then release | B, create once | C, conflict driven |
| --- | --- | --- | --- |
| Author opt in required | yes, explicit list | yes, explicit list | no |
| Reclaims if external controller disappears | no | no | yes |
| Drift corrects before handoff | yes | no | yes, until a real conflict |
| Handles multiple foreign owners | yes, any non kro manager | yes, any non kro manager | yes, enumerated from the 409 causes |
| Handles arrays and named list items | yes, single field=value selector, author written | yes, single field=value selector, author written | yes, any shape including composite merge keys, no authoring needed |
| New dependency | `structured-merge-diff/v6/fieldpath` | none | none |
| Blast radius | one resource, opted in | one resource, opted in | every resource kro manages |
| Author can force correct regardless of contention | yes, just omit the field from the list | yes, just omit the field from the list | no |

## Other solutions considered

Leaving the field out of the template entirely is today's only workaround and is already covered above, it cannot express an initial value plus a later handoff.

Exposing "is this field foreign owned" as a CEL readable signal composed with the existing `omit()` function (KREP-017, `omit-when.md`) was also considered, for example `replicas: ${self.isForeignOwned("spec.replicas") ? omit() : 3}`. This was set aside as a bigger and riskier change than any of the three options above. Every node's template is resolved before that same node's live object is fetched (`Apply` in `pkg/graphengine/executor/simple.go`, line 236, calls `n.Resolve()` before `getLive()` for every node in the walk), so there is no live state available to a node's own template expressions today. The only existing self referential CEL case, `readyWhen`, only works because it runs after the node has already been applied and published into scope (`readyEnv := bc.env` in `pkg/graphengine/compiler/typecheck.go`, line 299, just reuses the node's own already published scope entry). Making a node's own live state available during its own template resolution would require new self schema plumbing in the compiler and reordering `getLive` ahead of `Resolve` for any node that uses it. This may be worth revisiting if that plumbing gets built for other reasons, but it is not a smaller change than Options A through C.

## Scoping

In scope: resource level field handoff for RGD and Graph template nodes, within kro's existing server side apply model.

Out of scope: cross RGD or cross instance shared ownership of a single resource, where two separate RGD instances each want to manage a different subset of fields on the same object. That is [issue #1354](https://github.com/kubernetes-sigs/kro/issues/1354) and is a related but distinct problem that none of the three options here solve.

## Testing strategy

Option A already has test coverage: a parser unit test in `pkg/graphengine/compiler/fieldpath_test.go`, compiler unit tests in `pkg/graphengine/compiler/compiler_test.go` covering the path and selector validation rules, executor unit tests in `pkg/graphengine/executor/external_fields_test.go` covering a selector picking one of several list items that share the same field name, an integration suite in `test/integration/suites/core/external_fields_test.go` covering create, handoff, the regression guard that an unlisted field still reverts, and instance deletion, plus a manual verification on a kind cluster using `kubectl scale` and `kubectl patch` as stand ins for a real field manager.

If Option B is chosen it would need an integration test proving the field never drift corrects after creation, including across a controller restart, and a test proving the disposable manager identity does not collide across repeated create and delete cycles of the same resource name.

If Option C is chosen it would need integration tests for the conflict then partial retry loop itself, a test proving automatic reclaim once the foreign manager stops writing, and a feature gate test since this would ship behind one rather than as a default.

## Discussion and notes

- Issue reference: [#1290](https://github.com/kubernetes-sigs/kro/issues/1290)
- Related but distinct problem: [#1354](https://github.com/kubernetes-sigs/kro/issues/1354)
- Implementation of Option A: [PR #1468](https://github.com/kubernetes-sigs/kro/pull/1468)
- Prior art on server side apply ownership release: [KREP-017](./omit-when.md)
