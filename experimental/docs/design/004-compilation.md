# Compilation

Graphs are compiled like source code: parse expressions, check types, resolve dependencies, emit
executable artifacts. Not all type information is available at first compilation. CRDs may not be
installed yet. The compiler uses gradual typing, compiling nodes without schemas permissively, and
narrows types progressively as schemas become available.

For nodes with complete schemas, every type error is caught before a resource is applied. Dynamic
GVK nodes (`kind: ${expr}`) are compiled permissively until their concrete type is known, which
triggers recompilation with the resolved schema (see [Deferred Types](#deferred-types)). The only gap
is [unsafe types](#unsafe-types) -- CRDs with incomplete schemas where the type information doesn't
exist.

The compiler has two objectives:

- **Correctness** -- catch every type error before a resource is applied. Type coverage improves
  with each compilation as schemas arrive. This extends across composition boundaries: when a parent
  Graph stamps child Graphs via forEach, the compiler validates child expressions at the parent's
  compile time.
- **Performance** -- pay the cost of parsing, type-checking, and dependency resolution once. Each
  compiled artifact is immutable and shared across reconciles and structurally identical graphs.
  Compilation fires only when inputs change.

<img src="004-compilation.svg" width="300" />

The Watcher runs metadata-only informers against the API server and triggers the reconciler on
resource or schema changes. The reconciler declares interest in new types it discovers during the DAG
walk (fire-and-forget). When triggered, the reconciler requests compilation, which returns a cached
artifact or recompiles if the type cache generation has advanced. The compiler is passive -- it
resolves schemas and compiles expressions only when called.

## Type System

To type-check expressions, the compiler needs to know each node's type. Each node type determines
how its type is resolved:

- **`template:`, `patch:`, `ref:`, `watch:`** -- resolve OpenAPI schemas from the API server by
  apiVersion/kind, cached in the [type cache](#type-cache).
- **`def:`** -- infer types from template structure and expression return types. Literal fields are
  typed from their values. Expression fields (`${expr}`) are typed from the return type of the
  compiled expression, resolved during topological compilation (see [Algorithm](#algorithm)). The
  inferred type is written to the type cache so downstream nodes see it.

When a node's type cannot be resolved (unresolved CRDs, dynamic GVKs), the compiler declares it
untyped and proceeds permissively. Any field access on an untyped node compiles without error. Types
only get more specific, never less. Unresolved nodes start permissive and narrow as schemas become
available.

The compiler validates assignment compatibility: each expression's return type must match the
destination field's schema. `spec.replicas: ${someString}` is rejected because the schema expects an
integer.

### Type Cache

All resolved types live in a type cache. Resource nodes populate the cache from the API server's
OpenAPI schemas, resolved during compilation. Def nodes populate it during compilation (node ID to
inferred type from structure and expression return types). forEach element types are derived from
collection expressions during compilation. The cache is part of the compiled artifact.
Reconciliation reads it -- it never re-resolves types.

The cache tracks a single global generation counter. Any schema change (CRD installed, updated,
removed) advances it. Staleness is one integer comparison: current generation exceeds the artifact's
recorded generation. This over-invalidates -- a schema change to an unrelated CRD marks all artifacts
stale -- but CRD changes are rare and recompilation is cheap relative to the alternative (per-entry
versioning with dependency tracking). The hot path pays one comparison. Spec mutations invalidate the
entire artifact unconditionally.

For dynamic GVK nodes, the resolved GVK is also recorded per-node in the artifact. When the
reconciler evaluates a dynamic GVK expression and gets a different type than what was compiled
against, that node's compilation is stale regardless of the cache generation.

### Deferred Types

Dynamic GVK nodes (`kind: ${expr}`) can't be typed at first compilation because the schema depends
on runtime data. The node is declared untyped and compiled permissively.

When the reconciler evaluates the GVK expression and resolves a concrete type, it checks whether the
resolved GVK matches what the artifact was compiled against. If the GVK is new or different, the
artifact is stale and recompilation produces a fully-typed result. This time the type cache has the
concrete schema, so the node types fully and downstream narrows through the topological walk.

If the GVK changes on a subsequent reconcile, the artifact is stale again and compilation fires with
the new schema. This is rare -- dynamic GVKs typically stabilize -- but the system handles it the
same way every time.

This is the same invalidation model as CRD changes. The difference is the detection path: CRD
changes are detected by watches, dynamic GVK changes are detected at reconcile time. Both resolve
schemas from the type cache. Both invalidate the artifact when the schema differs from what was
compiled against.

### Unsafe Types

CRDs with unstructured portions -- `x-kubernetes-preserve-unknown-fields`, or `type: object` with no
properties -- are **unsafe** in those portions. The compiler has no schema for the unstructured
subtree. Expressions accessing it compile permissively and may fail at runtime. Like unsafe memory
access in systems languages, the compiler cannot help -- the type information doesn't exist.

## Algorithm

**Build the DAG.** Scan expressions for node references to build the dependency graph. Edges from
dependency to dependent, both forward and reverse adjacency. Topological order is stable with
respect to `spec.nodes` ordering. Cycles rejected.

**Compile in topological order.** For each node: resolve its type from the type cache, compile its
expressions against all upstream types (already resolved), then write the narrowed type back to the
cache so the next node sees it.

During expression compilation, the compiler extracts field paths from the AST and validates
assignment compatibility. Two node types require special handling:

- **forEach nodes** produce two type bindings from a single collection expression. The iterator
  variable is typed as the collection's element type (e.g., `list(Namespace)` yields a `Namespace`
  iterator). The node ID is typed as the list type for downstream references. readyWhen expressions
  compile against the element type; all other expressions compile against the list type.
- **Dynamic GVK nodes** are marked as deferred -- their type will be resolved when the reconciler
  evaluates the GVK expression (see [Deferred Types](#deferred-types)).

Field paths are (node, field chain) pairs: `${deploy.status.replicas}` yields
`(deploy, status.replicas)`. When a chain contains a dynamic operation, the path terminates at the
last static select. These paths drive the hash mechanics in
[005-reconciliation](005-reconciliation.md#hash-mechanics).

The result is an immutable artifact: compiled programs, field paths, DAG topology, type cache, and
the cache generation it was compiled against.

Compilation runs when its inputs change. **Spec mutation** is detected when `metadata.generation`
changes -- the reconciler requests compilation, which compiles the new spec and produces a revision
(see [002-revisions](002-revisions.md)). If compilation fails, no revision is created and `Compiled`
is set to `False` on the Graph (see [001-graph](001-graph.md#conditions)); reconciliation continues
on the previous revision if one exists. **Cache invalidation** is detected when the type cache
generation advances -- at least one schema changed (CRD installed, updated, or removed) and the
artifact is stale.

## Optimizations

### Recursive Compilation

When a Graph stamps child Graphs (via forEach), the child's expressions are invisible to the
parent's compiler -- they arrive as deferred `$${...}` strings or as rendered output. The child
controller discovers errors only when it compiles the child Graph, which may be arbitrarily later.

The compiler closes this gap through two mechanisms.

**Deferred expression analysis.** After compiling `${...}` expressions, the compiler scans for
`$${...}` patterns. For each, it strips one `$`, builds a best-effort type environment from the
child scope (node IDs from the child's node list, typed permissively), and type-checks the inner
expression. Errors are reported on the parent Graph. The compiler handles arbitrary deferral depth
by recursing.

The child scope is extracted from the template when statically knowable: literal node lists are
fully extractable, expression-valued node lists are partially or not at all. The compiler works with
what it has -- partial scopes catch errors in known references, unknown references compile
permissively.

**Pre-compilation.** When a forEach template produces a child Graph CR (literal apiVersion/kind
identifying a Graph), the compiler extracts the child spec, strips one deferral level (`$${...}` to
`${...}`), and runs the full pipeline. This catches expression errors, type errors, and DAG cycles
at the parent's compile time -- before any child Graph CR exists. When the parent later stamps child
Graphs during reconciliation, the child controller computes the same compilation key and gets a
cache hit. On spec mutation, the parent recompiles and pre-compiles the new child spec, catching
errors in the updated template before children are re-created.

Pre-compilation requires literal apiVersion/kind and a literal node list. When conditions aren't
met, the child compiles independently at reconcile time. Missing CRDs in the child scope are handled
the same way as at the parent level -- typed permissively, narrowed when the cache is populated.

Errors from both mechanisms carry enough context to trace back to the exact location in the parent
spec: parent node, deferral depth, child node, inner expression.

### Structural Caching

forEach stamps N child Graphs from the same template. Each child has different concrete values, but
compilation ignores concrete values -- the compiled programs, DAG topology, and type environment are
identical across all N children.

A compilation key hashes only structural inputs: node IDs, node types, expression strings, literal
apiVersion/kind values, forEach bindings, condition strings. Concrete values are excluded. Two specs
with the same compilation key produce the same compiled output.

The artifact splits into shared and per-instance parts. The shared artifact (programs, field paths,
DAG topology, types) is keyed by compilation key and immutable. Per-instance state (concrete node
bodies, scope, forEach state) is assembled from the shared artifact and the instance's spec.
Instances are isolated -- mutations to one never affect another.

For a Kind with 100 instances: 1 compilation + 99 cache hits. Shared artifacts are retained while
referenced and cleaned up when the last reference is removed.
