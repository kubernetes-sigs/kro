# KREP-017: Template Field Omission with `omit()`

## Summary

Today, once a resource template defines a field, that field is present in the
object kro sends in its apply request. CEL can decide what value is written to
that field, but it cannot express that the field should be omitted entirely.
That becomes a practical limitation for Kubernetes APIs that distinguish
between leaving a field out and sending that field with an empty or placeholder
value. For many APIs, omission is valid, while `null`, `""`, or similar
substitutes are not.

This proposal introduces `omit()`, a zero-argument CEL function that returns a
sentinel value. When the resolver encounters this sentinel as a field's
resolved value, it removes the containing field from the rendered object before
sending the apply request. Users express omission conditions using standard CEL
control flow:

```yaml
policy: ${schema.spec.policy != "" ? schema.spec.policy : omit()}
```

KRO could instead choose a global default and omit certain resolved values
automatically, for example by omitting all resolved `null` values. But no such
default is compatible with all CRDs and all fields. Some fields need omission
to remain valid; other fields are nullable and need to preserve explicit
`null`. This proposal therefore keeps omission explicit and field-local.

The proposal is intentionally narrow. It does not change CEL evaluation, it
does not reinterpret plain `null`, and it does not apply to `includeWhen`,
`readyWhen`, `forEach`, or other non-template expression contexts. The goal is
to add one explicit mechanism for template-driven field omission without
changing the meaning of existing templates.

## Motivation

GitHub issue [#928](https://github.com/kubernetes-sigs/kro/issues/928) asks for
field-level conditional inclusion in templates. The motivating example is an
ACK S3 `Bucket` where `policy: ""` is rejected, but omitting `policy`
entirely is valid.

This is not specific to ACK, and it is not primarily about CEL expressiveness.
KRO already uses CEL extensively. The missing capability is that template
structure is fixed once authored: if a field exists in the template, kro will
include that field in the apply payload. Users can vary the field's value, but
they cannot express that the field itself should be absent.

That matters because, under Kubernetes API validation, "field absent" and
"field present with `null`" are distinct states. For many APIs, especially
third-party CRDs, that distinction is the difference between a valid request
and a rejected one.

In practice, that leaves users with workarounds that either do not preserve
intent or do not scale:

- return `null` and hope the target schema accepts it
- return `""` or `{}` and hope the target API treats that like absence
- split one logical resource into multiple `includeWhen` variants just to
  control one optional field
- move omission logic outside KRO entirely

PR [#1003](https://github.com/kubernetes-sigs/kro/pull/1003) explored implicit
omission of resolved `null` values. The discussion there established the right
boundary for this proposal. The problem is real, but changing the meaning of
plain `null` globally would be a breaking semantic shift. Omission should
remain an explicit authoring decision rather than a side effect of ordinary
expression evaluation.

This matters especially for CRDs because `nullable` is opt-in in CRD schemas. A
field only accepts explicit `null` when the schema marks it as nullable. In the
common Kubebuilder/controller-tools path, fields are non-nullable unless API
authors opt in with `+nullable`, so many third-party CRDs reject explicit
`null` while still accepting the field being absent.

The design goal, then, is not to make `null` behave differently. It is to give
template authors one explicit way to say "omit this field."

## Proposal

### Overview

The proposed user-facing API is a zero-argument CEL function `omit()` that
returns a sentinel value. Users combine it with standard CEL control flow to
express conditional omission:

```yaml
spec:
  minReadySeconds: ${schema.spec.minReadySeconds != null ? schema.spec.minReadySeconds : omit()}
  ingressClassName: ${schema.spec.ingress.className != "" ? schema.spec.ingress.className : omit()}
  policy: ${schema.spec.policy != "" ? schema.spec.policy : omit()}
  scaling: ${schema.spec.autoscaling.enabled ? schema.spec.scaling : omit()}
```

`omit()` is a value, not a wrapper or member-style call. It composes naturally
with any CEL expression — ternaries, nested conditionals, `cel.bind()`, or any
other control flow. Users write the omission condition explicitly using
standard CEL, and the resolver handles field removal when it sees the sentinel.

This is a template-resolution feature, not an evaluation feature. It does not
change how CEL expressions are parsed, checked, or evaluated. It only changes
how kro resolves the containing template field after evaluation has completed.

### Where It Is Valid

`omit()` is only valid within standalone template field expressions.
It may appear at the top level:

```yaml
field: ${omit()}
field: ${condition ? value : omit()}
```

or inside subexpressions that ultimately feed a standalone template field:

```yaml
field: ${condition ? left : omit()}
field: ${cond1 ? val1 : cond2 ? val2 : omit()}
```

It can be used throughout a structured template, as long as each use still
appears as part of a standalone field expression:

```yaml
spec:
  name: ${schema.spec.name}
  policy: ${schema.spec.policy != "" ? schema.spec.policy : omit()}
  encryption:
    type: ${schema.spec.encryption.type}
    kmsKeyID: ${schema.spec.encryption.kmsKeyID != "" ? schema.spec.encryption.kmsKeyID : omit()}
  scaling:
    minReplicas: ${schema.spec.scaling.minReplicas}
    maxReplicas: ${schema.spec.scaling.maxReplicas != null ? schema.spec.scaling.maxReplicas : omit()}
```

In this example, `name`, `encryption.type`, and `scaling.minReplicas` are
always written if their expressions resolve successfully. `policy`,
`encryption.kmsKeyID`, and `scaling.maxReplicas` are omitted only when their
respective conditions route to `omit()`.

It is not valid in:

- `includeWhen`
- `readyWhen`
- `forEach`
- other non-template expression contexts
- string-template fragments such as `"prefix-${expr}-suffix"`

The containing field is the unit of omission. A field is either written or
omitted as a whole.

### ServerSideApply Implications

Because kro uses ServerSideApply, omitting a field means kro no longer manages
that field. If a field is omitted, kro will not track it, and any external
changes to that field will not be detected or corrected by kro. Users should
understand that `omit()` releases field ownership, not just suppresses the
value.

### Why Omission Must Be Explicit

This proposal preserves a distinction between writing `null` and omitting a
field. Some destination fields accept explicit `null`; others only work when
the field is absent. Because kro cannot choose one global default that is
correct for every field, omission must remain an explicit, field-local
authoring decision via `omit()`.

Because `omit()` is a plain CEL value that composes with standard control flow,
users can express arbitrarily complex omission conditions without needing
special syntax.

### Compiler Behavior

At compile time, the compiler should:

- register `omit()` as a zero-argument function in the CEL environment
- reject `omit()` in `includeWhen`, `readyWhen`, `forEach`, and other
  non-template contexts by inspecting function calls in restricted expressions
- reject `omit()` in string-template expressions (see below)
- validate that `omit()` is not used on required fields (e.g., `metadata.name`)
  where omission would always produce an invalid resource

The compiler should not silently rewrite user-authored templates to inject
`omit()`. Omission is the user-visible semantic choice in this proposal.

#### String Template Interaction

String templates like `"prefix-${a}-${b}"` are lowered to CEL concatenation
expressions at parse time (e.g., `"prefix-" + (a) + "-" + (b)`). After
lowering, both standalone and string-template expressions are stored as a
single `Expression`. The parser preserves the original template form in
`Expression.OriginalTemplate`.

If a user writes `"prefix-${omit()}-suffix"`, it gets lowered to
`"prefix-" + (omit()) + "-suffix"`. The `+` operator cannot concatenate a
string with the omit sentinel, so this would fail at runtime rather than
compile time.

The compiler must reject `omit()` in expressions where `OriginalTemplate` is
set — that is the signal that the expression originated from a string template
and omission is not meaningful. This check should happen during expression
validation in the builder, before compilation.

### Runtime Behavior

At runtime, field resolution with `omit()` should behave as follows:

1. Evaluate the expression using existing CEL/runtime semantics.
2. If evaluation returns a pending-data error, preserve existing pending-data
   behavior unchanged.
3. If evaluation returns any other error, preserve existing error behavior
   unchanged.
4. If evaluation succeeds and the resolved value is the omit sentinel, remove
   the containing field from the rendered object.
5. Otherwise, write the resolved value to the field as usual.

Because plain `null` and omission remain different outcomes, the implementation
uses a sentinel type (`OmitSentinel`) internally. Plain `null` remains
representable for nullable fields. Omission only happens when `omit()` is
explicitly used and the control flow routes to it.

The initial implementation omits only the containing field. It does not
recursively prune parent objects that become empty after omission.

Array element omission is not supported. Splicing an element out of an array
shifts subsequent indices, and the resolver does not currently guarantee the
ordering needed to handle multiple omissions in the same array correctly. This
may be addressed in a future proposal.

### Implementation Shape

The implementation treats `omit()` as a CEL library function that returns a
custom `ref.Val` wrapping an `OmitSentinel` struct. The sentinel flows through
CEL's normal type system and control flow. When the resolver encounters the
sentinel as a field's resolved value after `GoNativeType` conversion, it
removes the containing field from the rendered object instead of writing a
value.

This is simpler than a wrapper approach (carrying both a value and an omit
bit) because there is no value to carry — the field is simply absent. The
sentinel composes naturally through branches: in
`${condition ? value : omit()}`, the selected branch evaluates to either an
ordinary value or the sentinel, and the resolver handles each case directly.

On the resolver side, when the resolved value is an `OmitSentinel`, the
resolver navigates to the parent of the target field path and deletes the key
from the parent map.

## In Scope

This proposal includes:

- the CEL function `omit()` returning a sentinel value
- field-level omission for standalone template expressions
- compile-time rejection in restricted contexts (`includeWhen`, `readyWhen`,
  `forEach`)
- preservation of current CEL evaluation, pending-data, and error semantics
- runtime support for distinguishing explicit omission from ordinary values
- ServerSideApply ownership semantics documentation
- unit and integration test coverage

## Out of Scope

This proposal does not include:

- implicit omission of all resolved `null` values
- any change to the meaning of plain `null`
- support in `includeWhen`, `readyWhen`, `forEach`, or other non-template
  expression contexts
- string-fragment omission inside values like `"prefix-${expr}-suffix"`
- omission of individual array elements
- omission of dynamic map entries
- recursive pruning of empty parent objects after a child field is omitted
- silent rewriting of user-authored templates to inject `omit()`
- compile-time validation against destination-field nullability (future
  enhancement)
- convenience sugar for common omission patterns (see Future Work)
- a broader partial-resolution model

## Future Work

### Convenience Functions for Common Patterns

`omit()` as a primitive requires ternary expressions for conditional omission:

```yaml
policy: ${schema.spec.policy != "" ? schema.spec.policy : omit()}
```

For the common case of "omit if equal to some value," this is verbose and
duplicates the expression. If real-world usage confirms that equality-based
omission dominates, we can add convenience functions built on top of `omit()`:

```yaml
# Possible future sugar — omit when the value equals the argument
policy: ${schema.spec.policy.omitWhen("")}
kmsKeyID: ${schema.spec.encryption.kmsKeyID.omitWhen("")}
replicas: ${schema.spec.replicas.omitWhen(null)}
```

These could be implemented as CEL library functions that return the sentinel
directly, or as compiler lowering mechanics that rewrite the sugar into
ternary + `omit()` before compilation — similar to how string templates are
lowered to concatenation today. Either way, the resolver, compiler validation,
and SSA semantics remain unchanged. This is deferred until we see actual usage
patterns and can confirm which forms are worth the added API surface.

## Discussion and Notes

- Issue reference: [#928](https://github.com/kubernetes-sigs/kro/issues/928)
- Prior discussion of implicit null omission:
  [PR #1003](https://github.com/kubernetes-sigs/kro/pull/1003)
- Implementation: [PR #1139](https://github.com/kubernetes-sigs/kro/pull/1139)

## Other Solutions Considered

### Implicit Omission of All Resolved `null` Values

This is the direction explored in PR #1003: if a field resolves to `null`, kro
removes it automatically.

This proposal does not take that route because it changes the meaning of plain
`null`, erases the distinction between explicit `null` and omission, and makes
omission implicit rather than opt-in. It also does not provide a default that
is correct across all schemas: automatically omitting `null` helps with many
non-nullable fields, but it breaks cases where a nullable field must preserve
an explicit `null`.

### `omitWhen(value)` — Member-Style Conditional Omission

An earlier revision of this proposal used a member-style form:

```yaml
spec:
  policy: ${schema.spec.policy.omitWhen("")}
  replicas: ${schema.spec.replicas.omitWhen(null)}
```

`expr.omitWhen(v)` would evaluate `expr`, compare it to `v`, and omit the
field on match. This was rejected in favor of `omit()` for several reasons:

- `omitWhen(value)` is sugar for one specific pattern: equality comparison.
  Users inevitably need non-equality conditions (`> 0`, `has()`, compound
  checks), and then they write ternaries anyway, making `omitWhen` redundant.
- `omit()` is a simpler primitive. It is a value, not a wrapper. It composes
  with any CEL control flow without special handling.
- The implementation is cleaner: a sentinel + delete, rather than a custom
  result type carrying both a value and an omit bit with compose-through-
  branches semantics.
- `omitWhen` required the compiler to understand and validate the omit value
  against both the receiver type and the destination field type. `omit()` needs
  no such coupling.

### The `?` Field Operator

One alternative is a template-level operator such as:

```yaml
spec:
  policy?: ${schema.spec.policy}
```

This proposal does not use that form because it introduces new syntax outside
CEL and pushes omission semantics into template parsing rather than the
expression model users already work in. It remains a possible future
convenience syntax, but it is not the primary proposal here.

### `orOmit()`

Another alternative is a narrower helper such as:

```yaml
spec:
  lifecycleRules: ${schema.spec.lifecycleRules.orOmit()}
```

This proposal does not use `orOmit()` as the primary API because it is
effectively a special case of `omit()` for null values, while the motivating
problem is broader than `null` alone. `omit()` combined with standard CEL
control flow covers all cases without needing specialized variants.
