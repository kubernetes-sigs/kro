# Graph Controller Performance

Steady-state reconciles make zero API calls per managed resource. The
controller caches compiled expressions, hashes template output to skip
unchanged applies, and uses metadata watches to skip unchanged reads.
The only API call in a fully converged reconcile is the initial GET of
the Graph object that triggered it.

## Expression Compilation

CEL expressions are compiled once when a Graph spec is first seen, not on
every reconcile. The spec determines all the expressions and all the
identifiers they can reference. When the spec changes, everything recompiles.
When it doesn't, the reconcile loop evaluates pre-compiled programs.

A compilation failure halts the reconcile before any resources are touched.
It shows up as a status condition on the Graph.

A cache miss during evaluation is an invariant violation. The set of
compiled programs is closed after compilation — every expression the
reconcile loop can encounter was compiled upfront. A miss means the
extraction is incomplete, not that a fallback should compile on demand.

## Template Hashing

Before applying a resource, the controller hashes the evaluated template and
stores the hash as an annotation on the resource. On the next reconcile, it
hashes the template again and compares. If the hash matches, the apply is
skipped entirely — the desired state hasn't changed, so there's nothing to
send to the API server.

Contributions follow the same pattern. If the contribution's evaluated
output hasn't changed, the write is skipped.

## Change Detection

The watch system runs metadata-only informers for every resource type the
controller touches. These track each object's resourceVersion at negligible
cost. The controller caches the full object alongside its resourceVersion.

On each reconcile, the controller checks whether the resourceVersion in the
metadata informer matches its cached copy. If it matches, the cached object
is used directly — no read from the API server. If it doesn't match (someone
else changed the object, like a status update from another controller), a
full read refreshes the cache.

Combined with template hashing:

- Nothing changed: skip everything. Zero API calls.
- Someone else updated the object but our template is the same: one read.
- Our template changed: one write.
- First time: one write.

The metadata cache can be momentarily stale — a reconcile might fire before
the informer processes the event that triggered it. The next event corrects
this. Every informer-based controller has this property.

## Field Ownership

The controller never forces ownership on resources it creates. When the
template hash doesn't match and the controller applies, it uses server-side
apply without forcing ownership.

If a resource already exists and another field manager owns some of the same
fields, the apply returns a 409. The controller doesn't fight. It marks the
resource as conflicted and moves on.

A conflict on one resource doesn't stop the rest of the Graph. Independent
branches keep working. Only resources that depend on the conflicted one are
blocked. The Graph status reports which resource is conflicted and why.

The controller doesn't poll waiting for conflicts to resolve. The watch on
the conflicted resource fires if it changes, which triggers a new reconcile.
If the other actor releases ownership or the resource is deleted, the next
reconcile succeeds.

If a resource was never successfully applied (it was in conflict from the
start), the controller doesn't delete it when the Graph is deleted. The
template hash annotation is the proof of successful ownership — no
annotation means it was never ours.

Contributions are the exception. A contribution is a partial template that
writes fields on an object someone else manages. They force ownership because
that is their purpose. A 409 on a contribution would defeat the reason it
exists.

## Drift

Drift from edits that don't take SSA ownership (like kubectl edit) is not
restored. The controller's template hash still matches, so it skips the
apply. The edit persists until the Graph's template output changes. At that
point, SSA restores the controller's desired state because it still owns
those fields.

Drift from edits that take SSA ownership (another controller's server-side
apply with force) produces a 409. The controller surfaces the conflict. It
doesn't try to take the field back.

This is the same tradeoff as pod-template-hash in Deployments. The
controller converges on spec change, not continuously.

## Write Elimination

The controller avoids writes to the Graph object itself when nothing has
changed. The `internal.kro.run/applied-resources` annotation is only written
when the set of managed resources changes. The status subresource is only
written when the status content changes.

Without this, the controller doesn't converge. Every reconcile would write
the annotation, bumping the resourceVersion, which would trigger a watch
event, which would trigger another reconcile — an infinite loop of no-op
writes. The comparison breaks the loop.

## Plan States

| State         | Meaning                           | Downstream effect    |
|---------------|-----------------------------------|----------------------|
| Ready         | Applied, readyWhen satisfied      | Unblocked            |
| NotReady      | Applied, readyWhen not satisfied  | Blocked              |
| DataPending   | Upstream data not available yet   | Blocked (contagious) |
| Excluded      | includeWhen false                 | Excluded (contagious)|
| Conflict      | 409, someone else owns the fields | Blocked (contagious) |

## Steady-State Cost

For a Graph with N managed resources where nothing has changed:

| Work                     | Before     | After |
|--------------------------|------------|-------|
| Expression compilations  | ~5N        | 0     |
| Resource writes          | N          | 0     |
| Resource reads           | N          | 0     |
| Graph object reads       | 3          | 1     |
| Graph status writes      | 1          | 0     |
| **Total API calls**      | **2N + 4** | **1** |

When K of N resources change:

| Work                     | Before     | After    |
|--------------------------|------------|----------|
| Expression evaluations   | ~5N        | ~5N      |
| Resource writes          | N          | K        |
| Resource reads           | N          | K        |
| Graph object reads       | 3          | 1        |
| Graph status writes      | 1          | 1        |
| **Total API calls**      | **2N + 4** | **2K+2** |

Unchanged resources (N-K) have matching resourceVersions in the metadata
cache and matching template hashes — zero API calls. Only the K changed
resources require reads (to refresh the cache) and writes (to apply the new
template).

## Why not

**Continuous drift restoration.** SSA can re-apply every reconcile to restore
drift. We skip the apply when the template hash matches instead. Drift from
non-SSA edits persists until the template output changes. This is the
pod-template-hash tradeoff: steady-state cost drops from N writes to zero.
Drift restoration on every reconcile is an N-write tax paid continuously for
an event that rarely happens.

**ForceOwnership on first apply.** First apply without force means a
pre-existing resource with conflicting field owners returns a 409 immediately.
Force on first apply would silently take ownership, hiding the conflict until
the external actor fights back. Surfacing the conflict at first contact is
more useful than discovering it later. Contributions exist for intentional
cross-owner writes.

**Full-object informers.** The watch system uses metadata-only informers.
Full-object informers would eliminate the need for the resource cache and the
GET-on-miss path, but they cache the entire object for every watched resource
in memory. Metadata informers track only resourceVersion, labels, and
annotations. The full object is cached per-resource only after a read,
bounded by what the controller actually touches.

**Client-side SSA diffing.** Instead of hashing, compare the evaluated
template to the live object's managed fields to decide whether to apply. This
is reimplementing SSA client-side. The server already does the diff — the
hash gates whether we talk to the server at all, which is a cheaper check
than a field-level comparison against a cached object.

**Partial DAG walks.** Only walk the subgraph affected by the watch event
that triggered the reconcile. Controller-runtime coalesces events, so
multiple resources can change between reconciles. Tracking every event and
computing the affected subgraph union is equivalent to walking the whole DAG
unless most branches are unaffected. With zero API calls per unchanged
resource, the full walk is already cheap — it's cached CEL evaluation and
hash comparison. The complexity doesn't earn its savings.

**Lazy CEL compilation.** Compile each expression on first use during the
reconcile loop, cache the program for next time. Eager compilation catches
all expression errors before any resources are touched. Lazy compilation
discovers errors mid-reconcile, after some resources have already been
applied, leaving the Graph in a partial state.
