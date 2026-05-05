# Optimizations (Future Work — Do Not Implement)

Performance optimizations layered on top of the core
[compilation](004-compilation.md) and [reconciliation](005-reconciliation.md) designs. These are
additive — the core algorithms are correct without them. Implement the core designs first; add these
optimizations once the core is stable and profiled.

## Metadata Informers

The controller uses metadata-only informers — spec and status are not fetched in the watch path.
Full object reads happen only during Resolve, on demand. Identity labels
(see [003-ownership](003-ownership.md#identity-labels)) enable watch event routing to the owning
node without fetching the full object.

The core algorithm relies on Kubernetes informer resync to detect missed watch events — the informer
re-lists periodically and delivers synthetic update events, which the controller detects via
resourceVersion comparison. On top of this, each node has an in-memory resync timer with a jittered
interval (default 30 minutes) — a consistency floor bounding how long any divergence can persist. On
expiry, the node bypasses hash checks — propagateWhen still gates. Between resyncs, the hash layers
eliminate all no-op writes. SSA corrects drift as a side effect. A skipped node does not reset its
timer. Jitter decorrelates across nodes. On restart, timers start fresh — bounded burst (10k nodes
over 30 minutes ≈ 5.5 applies/sec).

The controller's operational inputs are the informer store and the DAG. Revision status is a
write-only observation surface.

## Compilation Cache

The compiler does not run on every reconcile. Compiled artifacts are stored and reused until their
inputs change.

Artifacts are content-addressed by their structural inputs: expressions, node IDs, types, conditions.
Concrete values are excluded. N forEach children with different values but identical structure share a
single compiled artifact — one compilation instead of N.

Two events invalidate a cached artifact:

- **Graph structure change** — detected when `metadata.generation` changes. The reconciler compiles
  the new spec and produces a new revision (see [002-revisions](002-revisions.md)).
- **API structure change** — a referenced CRD's `metadata.generation` advances (see
  [004-compilation](004-compilation.md#algorithm)). Staleness is a per-CRD generation comparison:
  the artifact records the generation of each CRD it compiled against; if any referenced CRD's
  current generation exceeds the recorded value, the artifact is stale. When stale, the next
  reconcile rebuilds the artifact from scratch — types are resolved fresh from the API server and
  the topology is reconstructed.

Dynamic GVK resolution does not invalidate existing artifacts. Instead, the resolution changes the
cache key (which includes resolved GVKs as a suffix), causing a cache miss and producing a new typed
artifact alongside the existing permissive one. Both remain valid for their respective keys.

## Evaluation Caching

The core reconciliation design evaluates every reachable node on every reconcile. Evaluation caching
narrows this to only the nodes affected by a change, and within those nodes, skips work when the
result would be the same.

**Change detection.** A node is triggered when:

- its resourceVersion in the informer store differs from the value recorded at last evaluation.
  Absence counts — a resource deleted since last evaluation (present → absent) or not yet evaluated
  (no recorded value) both trigger. nil != anything.
- its resync timer has expired (see [Metadata Informers](#metadata-informers))
- the node changed in the latest compilation

A node enters the frontier when all its hard dependencies have been processed AND either a
dependency's output changed (propagation trigger) or the node is triggered. Untriggered nodes with
unchanged upstream retain their previous state and scope entry.

**Hash layers.** Three hashes operate at progressively deeper points in each node's evaluation,
mapping to steps in the [reconciliation propagation](005-reconciliation.md#propagation). Each hash
compares the current value against the previous reconcile's value. A match skips the corresponding
work:

- **input-hash** — hashes the field paths the node's expressions depend on (extracted during
  [compilation](004-compilation.md#algorithm)). Match + unchanged resourceVersion → skip template
  evaluation entirely. Match + changed resourceVersion → GET live object, re-evaluate readyWhen,
  check output-hash (template not re-evaluated). For `ref:` and `watch:`, this is the primary path
  — their output depends on cluster state, not template inputs, so input-hash stability doesn't
  imply output stability. Absent paths hash to a sentinel — absent to present is a change, not a
  skip.
- **apply-hash** — hashes the desired state that would be sent via SSA. Match → skip the write.
  Persisted as an annotation (`internal.kro.run/template-hash`) because recomputing it requires an
  SSA round-trip; without persistence, cold start produces an N-write burst. Resync bypasses the
  apply-hash (apply unconditionally) but the output-hash still applies.
- **output-hash** — hashes the node's observed output after apply. Match → dependents don't enter
  the frontier, bounding the propagation walk to the affected subgraph.

Input-hash and output-hash share the same reference paths. Both are ephemeral — recomputed on cold
start. The apply-hash survives restarts via the annotation.

**forEach children.** The evaluation loop applies the same input-hash check per child — unchanged
inputs means the child is already on the latest generation and is skipped.

**Watch nodes.** When a single resource changes, the watch node updates its cached list incrementally
rather than re-listing — O(1) per event, not O(matching).

## Parallel Evaluation

Nodes with no dependency relationship are independent and may be evaluated concurrently. The
correctness invariant is: a node evaluates only after all its hard dependencies have been processed.
The scheduler is free to parallelize within this constraint.

Dependency-driven scheduling seeds level-0 nodes (no dependencies) and evaluates dependents as each
node completes. Independent nodes in the frontier run in parallel. The same applies to prune —
independent nodes in the reverse frontier can be removed concurrently.
