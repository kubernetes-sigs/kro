# KREP-020: Dynamic Template Keys

## Problem statement

Today KRO only allows CEL expressions in template field values. Template keys
are always static.

That is a good default for the current compiler model. Static keys let the
compiler walk the manifest shape, derive concrete field paths, validate against
schema, and infer dependencies with clear error locations.

The limitation is that some templating use cases need computed keys rather than
computed values. The easy examples are open map fields such as labels,
annotations, and ConfigMap data. The harder and more interesting examples are
structured objects, where the chosen key determines which child field exists at
all and therefore which schema applies underneath it.

Example:

```yaml
template:
  metadata:
    labels:
      ${schema.spec.region}: "true"
```

The value is static, but the key is dynamic. Today there is no direct way to
express that in KRO's template model.

A more structural example is:

```yaml
template:
  spec:
    transport:
      ${schema.spec.protocol}:
        port: ${schema.spec.port}
        ${schema.spec.optionName}: ${schema.spec.optionValue}
```

Here the key does not just pick a map entry. It picks which child field exists
under `spec.transport`.

This is not just a syntax gap. Once a key becomes dynamic, the compiler can no
longer know the full field path at compile time. That means compile-time schema
descent, path-based validation, and some forms of type reasoning cannot work in
the same way they do for static keys.

The proposal therefore needs to answer two questions:

1. How should KRO represent CEL expressions in key position?
2. What should the compiler validate at compile time versus defer until the
   fully-resolved manifest exists at runtime?

## Proposal

### Overview

This KREP proposes allowing CEL expressions in template key position in
resource templates, including both open maps and structured objects.

Key expressions apply to any template object, not just open maps. They do not
apply to schema declarations or other non-template definitions.

#### Semantics

- A key expression must resolve to a string. If it resolves to `null`, the
  entire entry (key and value) is omitted.
- Dependency inference walks the CEL AST inside both key and value expressions,
  the same way it does for value-only expressions today.

#### Compile-time behavior

- The parser stops descending path-by-path when it encounters a CEL expression
  in key position. It marks the parent object as the boundary.
- The compiler reconstructs the subtree under that boundary as a single CEL
  value and type-checks it against the expected schema, using the same
  structural reasoning it already uses for CEL objects.
- Static siblings outside the dynamic-key subtree continue to use ordinary
  path-based validation.

#### Runtime behavior

- Ordinary apply-time validation checks the fully-resolved manifest after
  template resolution. This is the same validation path that exists today.

#### Common use cases

The simplest use cases are open map fields:

- `metadata.labels`
- `metadata.annotations`
- `ConfigMap.data`

The more structural use case is when the chosen key determines which child
field exists and therefore which schema applies underneath (e.g.
`spec.transport.${protocol}`).

### Syntax

Authors write a normal CEL expression in key position:

```yaml
template:
  spec:
    transport:
      ${schema.spec.protocol}:
        port: ${schema.spec.port}
        ${schema.spec.optionName}: ${schema.spec.optionValue}
```

The compiler must enforce that the key expression is string-valued when
present. If it resolves to `null`, the keyed field or map entry is omitted by
default.

### Compile-Time Validation

The key idea is simple: once the parser encounters a CEL expression in key
position, it can no longer keep walking the object as if every child path were
already known.

With a static key, the compiler can reason about an exact path like:

- `spec.transport.http.port`

With a dynamic key, it only knows the parent object:

- `spec.transport`

That does not mean compile-time validation stops. It means the validation shape
changes.

Instead of continuing path-by-path descent under `spec.transport`, the compiler
should reconstruct the whole nested object as a single CEL value rooted at
`spec.transport`, then validate that value against the expected schema for
`transport`.

That compile-time validation should still include:

- checking that the key expression is valid CEL
- checking that the key expression is compatible with a string key, with `null`
  treated as omission
- inferring dependencies from both key and value expressions
- type-checking the reconstructed CEL object against the expected schema, first
  with normal CEL assignability and then, when needed, with the same recursive
  structural-compatibility checks KRO already uses for CEL objects
- continuing ordinary path-based validation for static siblings outside the
  dynamic-key subtree

The only thing the compiler loses is exact child-path knowledge under the
dynamic key until resolution produces a concrete key string.

Mechanically, the rule should be simple: once the parser encounters a CEL
expression in key position, it stops treating that subtree as ordinary
path-by-path YAML.

At that point, the compiler takes the whole subtree rooted at the parent path,
rebuilds it as one CEL object expression, and validates that expression against
the expected schema for that parent object.

So the parser's job is just to find the CEL boundary. The compiler's job is to
reconstruct the subtree into a typed CEL value and validate it.

Concretely, that validation should work the same way `validateExpressionType`
works today: try ordinary CEL assignability first, and if that is not enough,
fall back to `AreTypesStructurallyCompatible`, which recursively compares lists,
maps, and structs and already knows how to reason about map-to-struct and
struct-to-map compatibility.

Note: the current structural-compatibility checks are intentionally permissive
in some cases. We should improve that behavior in follow-up work so compile-time
validation is stricter when enough type information is available.

Conceptually, that means the compiler can lower:

```yaml
transport:
  ${schema.spec.protocol}:
    port: ${schema.spec.port}
    ${schema.spec.optionName}: ${schema.spec.optionValue}
```

into a single CEL value expression for `transport` equivalent to:

```cel
{schema.spec.protocol: {"port": schema.spec.port, schema.spec.optionName: schema.spec.optionValue}}
```

This is not new user-facing syntax. It is compiler-side AST manipulation and
reconstruction: stop at the first key-position CEL expression, rebuild the
surrounding object as one comprehensible CEL value, then type-check that value
against the expected schema for `transport`.

### Dependency Inference

Dependency inference should continue to work for key expressions.

Example:

```yaml
template:
  spec:
    transport:
      ${config.metadata.name}:
        port: ${config.spec.value}
        ${schema.spec.optionName}: ${schema.spec.optionValue}
```

The compiler should infer dependencies from:

- `config.metadata.name`
- `config.spec.value`

This part is already aligned with the underlying CEL AST model: key expressions
are still CEL expressions, and their referenced resources should contribute to
the node's dependencies the same way value expressions do.

### Enum-Constrained Keys

Some dynamic keys may be constrained to a known finite set at compile time.

Example:

```yaml
template:
  spec:
    transport:
      ${schema.spec.protocol}:
        port: ${schema.spec.port}
        ${schema.spec.optionName}: ${schema.spec.optionValue}
```

If `schema.spec.protocol` is statically known to be one of:

- `http`
- `grpc`
- `tcp`

then the compiler could validate against each variant and require the nested
content to be compatible with all possible selected fields.

That is a useful optimization, but it is not required for v1. The baseline
proposal only needs best-effort compile-time validation plus ordinary
apply-time validation of the resolved manifest.

### Interactions with `omitWhen(...)` and `defer()`

`omitWhen(...)` is value-specific. It controls whether a produced value is
written, not whether a key exists.

For key expressions, `null` already has a default meaning: omit the keyed field
or map entry. There is no key string to materialize, so omission is the natural
result.

That means:

- `omitWhen(...)` should apply to values, not to key expressions
- if a key expression resolves to `null`, the keyed field or map entry is
  omitted by default
- if a value expression resolves to `null`, `omitWhen(...)` still behaves the
  same way it does today

`defer()` follows the same rule for keys. If a deferred key expression is still
pending for a given reconcile and its effective result is `null`, the keyed
field or map entry is omitted for that reconcile by default. No separate
`omitWhen(null)` is needed on the key expression.

### Validation Tradeoff for Structural Keys

For open-map fields like `metadata.labels` or `ConfigMap.data`, the schema
accepts any string key. A dynamic key expression cannot produce an invalid key
in that context, so compile-time safety is preserved.

For structural fields like `spec.transport`, the story is different. The key
selects which child field exists, and therefore which schema applies underneath.
If the key expression produces a value that does not match any known field name
(e.g. `"htpp"` instead of `"http"`), the error is only caught at apply time by
server-side validation. The compiler cannot catch it because it does not know
the concrete key.

This is an intentional tradeoff. Structural dynamic keys degrade validation to
apply-time only, unless enum constraints are implemented (see
Enum-Constrained Keys above). Authors who use structural dynamic keys should
understand that the compiler will validate the shape of the nested content but
not whether the chosen key is a valid field name.

The enum-constrained optimization described above would recover compile-time
safety for the common case where the key expression draws from a known finite
set. That optimization is not required for v1 but is the natural follow-up for
authors who want stronger guarantees on structural keys.

### Scope: Spec-Side Templating Only

Dynamic key expressions apply to resource templates, which define the desired
spec of managed resources. They do not apply to status field extraction or
readiness conditions.

Status paths and readiness checks reference concrete field paths on observed
resources. Those paths are always static: the resource already exists, the field
already has a concrete name, and the compiler can validate the full path.

Readiness expressions read from a known, already-materialized object. There is
no templating step for status extraction, so there is no key-position
expression to evaluate.

If a future proposal needs dynamic status extraction (e.g. reading a field
whose name depends on the spec), that would be a separate feature with its own
design. This KREP does not cover it.

## Other solutions considered

### Keep keys static

This preserves the current compiler model and keeps validation simple, but it
leaves a real and common templating need unexpressed.

### Limit the feature to open maps only

Another option is to support dynamic keys only in easy open-map cases.

That would cover the low-risk wins, but it would leave out the more important
structural use cases where the selected key changes which child field exists.

### Require users to select whole template variants

Another option is to say that any dynamic choice involving keys should be
modeled as a full template branch or selection.

That is appropriate when the choice changes structure, but it is too heavy for
simple open-map cases where the only dynamic part is the key string.

## Scoping

### What is in scope for this proposal?

- CEL expressions in map key position in resource templates
- CEL expressions in structured object key position in resource templates
- requiring dynamic key expressions to produce string keys, with `null` meaning
  omission
- dependency inference from key expressions
- parser stop-point behavior at the first key-position CEL expression
- compiler validation of the nested content as a typed CEL object
- best-effort compile-time validation when the exact child path is not known
- ordinary apply-time validation of the fully-resolved manifest
- support for both easy open-map and structural-object key expressions

### What is not in scope?

- CEL expressions for schema field names or other non-template declarations
- exhaustive compile-time validation across all possible key values
- special-case UX helpers for labels, annotations, ConfigMap data, or similar
  cases
- introducing a new runtime model beyond normal template resolution and
  validation
- dynamic key expressions in status field extraction or readiness conditions
