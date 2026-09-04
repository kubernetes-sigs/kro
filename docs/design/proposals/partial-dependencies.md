# KREP-018: Partial Dependencies in Branching Expressions

## Summary

KRO currently infers dependencies by analyzing the CEL AST for a template
expression, collecting every resource reference it finds, and turning each one
into a hard dependency. That works for straight-line expressions, but it is too
coarse for branching expressions such as ternaries, and that creates a
correctness problem we refer to here as contagious exclusion. In an expression
like `${schema.spec.useCache ? cache.status.endpoint :
database.status.endpoint}`, the runtime needs either `cache` or `database`
depending on the branch taken. But KRO infers both as required, so excluding
one referenced resource can prematurely exclude the dependent resource even
when the selected branch could have been satisfied.

This proposal makes the compiler and runtime aware of partial dependencies in
branching expressions. The compiler should preserve the branch structure of the
checked CEL AST and lower it into a branch-aware dependency/control-flow IR,
and the runtime should use that IR while preserving the original CEL program
for final value evaluation.

For ternaries, including nested ternaries, dependency activation should follow
CEL selection semantics: conditions are evaluated in order, and the first
branch whose predicate evaluates to true wins. Only the dependencies on the
selected path should be considered required for the node to proceed.

## Motivation

The core problem is not that the compiler sees too many references; it is that
the current runtime model treats every reference in a branching expression as
unconditionally required.

The canonical example is:

```yaml
spec:
  endpoint: ${schema.spec.useCache
      ? cache.status.endpoint
      : database.status.endpoint}
```

If `cache` is guarded by `includeWhen: ${schema.spec.useCache}` and
`schema.spec.useCache` is `false`, the node above should still be valid. The
expression does not need `cache` in that case. Under the current model, the
compiler still infers dependencies on both `cache` and `database`, and the
runtime treats both as required. The dependent node is skipped even though the
`database` branch was sufficient.

This is a correctness problem, not just an optimization problem. The user wrote
an expression with valid runtime semantics, but the dependency model cannot
represent that semantics precisely enough to execute it correctly.

There are several ways to model this class of problem, including lazy
exclusion, tagged edges, and dependency sets. This proposal takes the most
explicit approach: branching semantics should become a compiler artifact rather
than a runtime guess.

## Proposal

### Overview

This proposal changes dependency compilation for branching expressions. Instead
of flattening all references into one unconditional dependency set, the
compiler should deconstruct the checked CEL AST into a dependency/control-flow
IR that preserves branch structure.

For a simple ternary such as:

```cel
schema.spec.useCache ? cache.status.endpoint : database.status.endpoint
```

the compiler should distinguish:

- dependencies needed to evaluate the condition
- dependencies needed on the true path
- dependencies needed on the false path

The original CEL expression remains the source of truth for final value
evaluation. The new IR exists for dependency reasoning, runtime dependency
handling, and exclusion handling.

### What Counts as a Partial Dependency

For this proposal, a partial dependency is a dependency introduced by a
branching expression where the referenced resource is needed only on some
runtime paths through the expression.

Examples:

```yaml
${condition ? cache.status.endpoint : database.status.endpoint}
${condition ? left.status.value : right.status.value}
${a ? x : b ? y : z}
```

In each case, the expression contains multiple possible dependency paths, but
only one path is active at runtime once the branch conditions are known.

### Compiler Deconstruction

The compiler should continue to parse and type-check the original CEL
expression, but it should emit two artifacts from the checked AST:

- a normal CEL program for final value evaluation
- a dependency/control-flow IR for runtime dependency handling and exclusion

That IR should model branching constructs explicitly. For a ternary, the
compiler should capture:

- the predicate expression and its dependencies
- the true branch and its dependencies
- the false branch and its dependencies

For nested ternaries, the same lowering should apply recursively. Conceptually:

```cel
a ? x : b ? y : z
```

means:

- evaluate `a`
- if `a` is true, choose `x`
- otherwise evaluate `b`
- if `b` is true, choose `y`
- otherwise choose `z`

The first predicate that evaluates to true wins. Only the dependencies on the
selected path should become active requirements for the node.

The compiler should not rewrite user-authored CEL into new user-visible CEL
expressions. The source expression remains intact. The compiler simply produces
a richer internal representation alongside it.

### Runtime Semantics

The runtime should consume the deconstructed dependency IR rather than a flat
dependency set.

For a branching dependency expression, runtime behavior should become:

1. Evaluate the branch predicates whose inputs are available.
2. Use those predicate results to determine the active path through the
   dependency IR.
3. Wait only on dependencies that are active on the selected path.
4. Ignore dependencies that belong only to unselected paths.
5. Evaluate the original CEL value expression once the selected path is
   satisfiable.
6. Skip the node only when the selected path requires a dependency that is
   excluded or otherwise impossible to satisfy.

This makes branch selection a first-class part of dependency handling rather
than something inferred indirectly from evaluation failure.

### Graph Representation

The graph model should become branch-aware for dependency purposes, but it does
not need to become a separate graph per branch combination. Instead, branching
CEL expressions should preserve their original compiled program and carry
branch-aware dependency information that the runtime can use without
flattening all referenced resources into unconditional dependencies.

That avoids the main failure modes of naïve deconstruction:

- exploding one expression into many rewritten CEL strings
- losing source-level error locality
- duplicating shared subexpressions
- eagerly enumerating every branch combination

The graph representation should preserve enough information for the runtime to
reason over the dependency IR rather than a single flattened dependency set. In
practice, that means the runtime can determine:

- what dependencies are needed to decide the next branch?
- which dependencies are active on the selected path?
- can this expression proceed with the currently available data?

### Relationship to Soft Dependencies and Partial Resolution

Soft dependencies and partial resolution are the broader idea of allowing a
resource to proceed even when some dependencies are unresolved or unavailable,
with the expectation that reconciliation can converge later as those
dependencies become satisfiable. A separate KREP is being developed to describe
that model.

This proposal solves a different problem. It does not allow unresolved
dependencies to be deferred; it makes KRO identify which dependencies are
actually required in a branching expression so that only the selected path can
block or exclude the resource.

## In Scope

This proposal includes:

- branching expressions whose statically inferred dependencies are only
  partially required at runtime
- compiler deconstruction of branching CEL into dependency/control-flow IR
- preservation of the original CEL expression for final value evaluation
- branch-aware runtime dependency activation
- preservation of current behavior for non-branching expressions
- compiler and runtime tests that demonstrate correct behavior for conditional
  dependencies

## Out of Scope

This proposal does not include:

- new CEL syntax
- rewriting user-authored CEL into new user-visible CEL expressions
- soft dependencies, soft edges, or cycle breaking
- solving every form of partial resolution
- changing non-branching expression semantics

## Other Solutions Considered

### Lazy Exclusion

One alternative is to keep the current compiler behavior and fix correctness
purely in the runtime by attempting resolution before exclusion propagates.

That works as a safety net, but it still leaves the compiler blind to the
actual dependency structure of the expression. If the goal is the most correct
architecture, dependency control flow should be made explicit in compiler
output rather than inferred indirectly at runtime.

### Tagged Edges

One alternative is to keep the graph static but annotate edges with activation
predicates derived from branch conditions. In the cache/database example, the
edge to `cache` would only be active when `schema.spec.useCache == true`, and
the edge to `database` would only be active when it is `false`.

This is a viable representation of branch-aware dependency IR, but it is not
the only one. The proposal here stays one level more general: the compiler
should deconstruct branching expressions into dependency/control-flow IR, and
tagged edges are one possible encoding of that IR.

### Dependency Sets

Another alternative is to have the compiler derive multiple satisfiable
dependency sets for a node and let the runtime use any set that is fully
satisfied at runtime.

This also models the problem directly, but it tends toward eager expansion of
branch combinations. A branch-aware IR preserves the same information without
requiring the compiler to enumerate every combination up front.

### Soft Dependencies

Soft dependencies solve a related but different problem. They are useful when a
node should be allowed to proceed without some data and then converge over
later reconciliations. The problem in this KREP is narrower: the selected path
determines which dependencies are actually required. Soft-edge semantics are
therefore not a substitute for deconstructing branching expressions correctly.
