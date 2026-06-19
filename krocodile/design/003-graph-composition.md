# Graph Composition

A Graph is one primitive: a **definition** parameterized by a typed input
interface, **instantiated** by binding that interface to a context. An
instance CR and a nested-graph node are the same operation — bind the
inputs, specialize the definition — differing only in where the bindings
come from. Compilation is therefore staged: the definition is checked
once against its interface *types*; each instantiation specializes it
against a *context* of bound values. This is the mechanism behind three
features that looked separate — CEL in `apiVersion`/`kind`, nested
graphs, and RGDs — and it is one mechanism.

This document describes the definition/binding split, the compilation
context, the staging rules, the module boundary for nesting, and how an
RGD falls out as a top-level promotion.

## Why

The current compiler is a pure total function `Compile(g) → Program`
([compiler.go](../pkg/compiler/compiler.go)). It assumes three facts are
statically knowable per node: the GVK is a literal, the target schema is
resolvable now, and the node namespace is one flat closed set. Three
features each break a different assumption:

1. **CEL in `apiVersion`/`kind`.** The GVK depends on bound values, so
   the schema can't be resolved at first compile.
2. **Nested graphs.** A node introduces a child namespace with its own
   nodes; the child's output type depends on what the parent binds in.
3. **RGDs.** A reusable abstraction with a typed `spec` interface,
   instantiated by a CR — which is exactly a parameterized graph.

Handling these as three features yields three mechanisms. Handling them
as one — *generic definition, specialize per call site* — yields one.

## The two forms

| Form           | What it is                                  | Bindings come from        |
| -------------- | ------------------------------------------- | ------------------------- |
| **Definition** | Parameterized graph. Has an input interface (schema) and output interface (status). Holes where inputs go. | n/a — compiled against *types* |
| **Binding**    | A definition + concrete inputs filling the holes. | an instance CR, or a parent node's input map |

Today's `Graph` is only the *concrete* form (values inline; the Graph is
its own instance). Composition adds the *abstract* form: a definition
with a declared input schema. The split is deliberate — definition and
instance are genuinely different things (generic vs. concrete), and
collapsing them was premature merging.

## Compilation context

A context is a **frame on a parent-linked chain**. Bindings (`def`s,
nodes, inputs) live in frames; name resolution walks outward,
innermost-wins. Instantiating a definition pushes a frame seeded with the
bound inputs; resolving a name walks the chain.

The same chain structure carries different payloads in different phases:

- **Compile time:** frames carry *types* (the input schema, each node's
  published schema).
- **Reconcile time:** frames carry *values* (the runtime scope).

Non-negotiable: **both phases share one resolution rule.** If the type
chain and the value chain resolved names differently, an expression could
type-check against one binding and evaluate against another — silent
meaning skew. Shadowing makes this a hard constraint, not a nicety: the
compile context and the runtime scope are the same structure, seeded
differently.

(This is a change from today's flat `celSchemas` map at compile and flat
`scope map[string]any` at runtime — both become chains.)

## Staging: compile once, specialize per call site

Two kinds of dependency, conflated today because they never diverged:

- **Value dependency** — need the upstream value to *render* a field.
- **Type-determining dependency** — need the upstream value to know the
  *type/schema* of this node (`apiVersion: ${cfg.flavor}`; a nested
  graph's output type as a function of its inputs).

Only type-determining deps force staging. A node is fully type-checked
the instant its type-determining inputs resolve — even if its value-deps
are still pending.

The lever: krocodile has no separate instance object, so `def`s and
literals are present at compile time. A type-determining expression over
static scope is **constant-foldable during compile** — fold it, fetch the
resolved schema, type-check fully now. Deferral is forced *only* when a
type-determining input transitively depends on a resource's observed
status (genuinely unknown until applied). That frontier is usually small.

```
seed context = input interface types (+ static def/literal scope)
for node in topo order:
    fold type-determining exprs against current context
    fully resolved? → resolve schema, type-check NOW (compile error on fail)
    waits on runtime status? → emit a deferred check; re-run at reconcile
                               once status enriches the context
```

A deferred node carries a **reified pending check** (node + waiting-on set
+ the schema-fetch-and-check described as data), *not* a closure — so the
registry can still cache partial programs. The deferred check runs the
*same* typecheck code path, fed the enriched context. It is staged static
checking, never a downgrade to runtime errors.

## Nesting is a sealed module boundary

A nested graph is a **module**: typed inputs in, typed outputs out,
internals private. The boundary does **not** capture the enclosing scope
— a child sees only its declared inputs. This is forced by the RGD use
case: an RGD must be reusable, and cross-boundary capture would make a
child mean different things in different parents.

Shadowing still exists, but **intra-module only**: a forEach iterator, a
local `def`, or a node may shadow an input name within the same graph.
The module boundary itself is sealed and typed. (This closes the
let-block / lexical-capture direction: capture is incompatible with
reusable definitions.)

Consequences of sealing:

- **DAG stays per-definition.** A nested graph is a *single vertex* in
  the parent DAG, with edges only through declared inputs/outputs. No
  flattened cross-level graph.
- **Cache key = definition spec + bound input *types*.** A child compiles
  and caches as a unit; the same definition with the same input types is
  reused.
- **Recursion is a compile error.** A definition transitively nesting
  itself is an invariant violation (cycle guard + depth cap on the
  context chain), surfaced loudly — not a recoverable edge.

## RGD = a definition promoted to a CRD-backed type

An RGD is not a separate concept. It is a **graph definition promoted to
a Kubernetes type**: reflect its input interface to a generated CRD so
users get a `kubectl`-able kind; each CR of that kind is a binding that
instantiates the definition. The output interface becomes the instance's
`status`.

This must stay a **thin top-level layer**. CRD generation, status
subresource, printer columns, versioning — these are concerns of
*promotion*, applied only at the top. They must never leak into the core
`definition + context → specialized program` machinery. If RGD-specific
concerns appear in the nesting code, the unification was forced and
should be backed out.

The payoff over original kro's `builder.go`: abstractions compose
natively. An RGD using another RGD is just a nested-graph node, not the
awkward "create an instance of another generated CRD as a resource"
chaining the original requires.

## What this deliberately does not do

- **No cross-module capture.** A child reads only declared inputs. No
  implicit visibility of the parent's nodes.
- **No flattened global DAG.** Nesting composes per-definition DAGs; it
  does not splice child internals into the parent's topological order.
- **No closures in the Program.** Deferred checks are reified data so the
  compile cache survives.
- **No downgrade of dynamic-GVK checking to runtime.** Dynamic nodes are
  staged-checked against their resolved schema; the type-check is delayed,
  not skipped.
- **No new instance engine.** Instance CRs and nested-graph nodes go
  through the same bind-and-specialize path.

## Boundaries

- **Compiler** gains a context parameter and a recursion/scope frame; it
  compiles a definition against its input interface and emits a Program
  that may contain reified deferred checks.
- **Reconciler / executor** finalizes deferred checks against the
  enriched (status-bearing) context before apply, surfacing deferred type
  failures on status distinctly from `DataPending`.
- **Registry** keys cached programs on definition spec + bound input
  types; nested children compose their hashes into the parent's key.
- **CRD promotion** is a separate top-level consumer of a definition's
  input interface. It does not touch the core compile/nest path.

## Open questions

1. **Input interface syntax.** simpleschema for `spec`? Where does it
   live on the abstract graph form?
2. **Output interface.** CEL-over-internal-nodes for `status` — same as
   original kro's status schema, or richer?
3. **Deferred-check status semantics.** Keep `Accepted` for the static
   frontier and route deferred type errors to `Ready=False` with a
   distinct reason, or widen `Accepted` to report mid-reconcile failures?
4. **Cache key for deferred programs.** Input *types* are clear; do
   partially-folded contexts participate in the key, or only the
   definition + interface?
5. **One context type or two.** Compile (types) and runtime (values)
   share the resolution rule — do they share the Go type, seeded
   differently, or stay distinct types with a shared interface?
