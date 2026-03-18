# KREP-023: Instance State Nodes

**Author:** Rafael Panazzo ([@pnz1990](https://github.com/pnz1990))
**Status:** Draft
**Filed:** 2026-03-17

---

## Problem statement

kro's CEL expressions are strictly read-only projections. They evaluate against
a snapshot of the parent instance CR and produce child resources. The output of
one reconcile cycle cannot feed as input to the next. This makes it impossible
to express stateful transitions — patterns where the computed result of a
reconcile depends on the previous value of a field.

Concrete patterns that are blocked today:

- **Accumulation**: `retryCount = retryCount + 1`, `usedQuota = usedQuota + delta`
- **Mutation**: `monsterHP[i] = monsterHP[i] - damage`, `inventory = inventory.filter(x, x != usedItem)`
- **Lifecycle transitions**: `phase: pending → reviewing → approved → provisioned`, each gated on the previous phase value
- **Counters with TTL**: `backstabCooldown = backstabCooldown - 1` per turn, stop at zero

These patterns are common in platform engineering — cert rotation scheduling,
progressive delivery step tracking, autoscaling cooldown windows, quota
enforcement, retry budgets with backoff.

The missing ingredient in every case is the same: a way for kro to write a
computed value back into the instance CR so that subsequent reconcile cycles
(or subsequent nodes within the same cycle) can read it as input.

The closest existing primitives do not cover this:

- `schema.status` projections — read-only, no self-reference, no accumulation
- KREP-011 data nodes — ephemeral within one reconcile cycle, not persisted to the API server
- KREP-016 patch nodes — writes to resources kro does not own, not to self

## Proposal

Add a new virtual node type — `state:` — that evaluates CEL expressions and
writes the results to a named storage scope under `status` on the instance CR.
The written values persist across reconcile cycles and are readable by
subsequent CEL expressions in the same or future cycles.

```yaml
resources:
  - id: rotationJob
    template:
      apiVersion: batch/v1
      kind: Job
      # ...

  - id: recordRotation
    state:
      storeName: rotation
      fields:
        lastRotatedAt:    "${rotationJob.status.completionTime}"
        rotationCount:    "${has(schema.status.rotation.rotationCount) ? schema.status.rotation.rotationCount + 1 : 1}"
    includeWhen:
      - "${rotationJob.status.succeeded == 1}"
```

The written values are available to subsequent nodes in the same cycle and to the next reconcile cycle:

```yaml
  - id: notifyOnThirdRotation
    template:
      # ...
    includeWhen:
      - "${has(schema.status.rotation.rotationCount) && schema.status.rotation.rotationCount >= 3}"
```

#### Overview

A `state:` node is a virtual node — it creates no Kubernetes resource. It
participates in the DAG normally, can depend on child resource statuses, and
can be depended upon by other nodes. Its reconcile action is a status patch to
the instance CR rather than an Apply to a child resource.

After a state node writes its values, the controller refreshes the in-memory
CEL context with the new `status.<storeName>` values. This means that state
nodes later in the DAG within the same reconcile cycle can read the values just
written — they are not deferred to the next cycle. Only values that have not
yet been written in the current cycle require a requeue.

This within-cycle context refresh is what makes multi-step state machine
patterns efficient. N state nodes in one RGD can advance N fields in one
reconcile cycle rather than requiring N consecutive cycles.

#### Design details

##### Node structure

A `state:` block is mutually exclusive with `template:` and `externalRef:`.
It has two fields:

- `storeName` (string, required) — the name of the field written under
  `status`. Must be a valid Go identifier. kro auto-injects this field into the
  generated CRD schema with `x-kubernetes-preserve-unknown-fields: true` when
  any state node targeting it is present in the RGD. The name must not collide
  with `state`, `conditions`, or `managedResources`, which are reserved by kro's
  own status management; any attempt to use a reserved name is rejected at
  admission time with a descriptive validation error.

- `fields` (map[string]CEL expression, required) — key-value pairs where every
  value is a `${...}` CEL expression. Bare strings (no `${}` wrapper) are a
  validation error.

`includeWhen` is supported. `readyWhen` is not applicable — a
state node is considered ready as soon as its expressions evaluate and the
status patch succeeds. `forEach` is not supported on state nodes in v1 — the
semantics of writing multiple `storeName`-scoped entries from a single node
are non-trivial and left for a follow-on proposal once the base primitive is
established. For example: does a `forEach` with three iterations produce
`status.migration.items = [{...}, {...}, {...}]` (one array field) or merge
all three iterations into a flat `status.migration = {step0: ..., step1: ...,
step2: ...}` (one map field per iteration)? Neither answer is obvious, and
the right design depends on use cases that are not yet established.

##### Within-cycle context refresh

After a state node successfully writes to `status.<storeName>`, the controller
calls an equivalent of `RefreshInstance` to update the in-memory representation
of the instance used by the CEL context builder. Any state node later in
topological order within the same reconcile cycle will read the updated values
from `schema.status.<storeName>.*` directly.

This is analogous to how Server-Side Apply field managers immediately expose
written values to subsequent operations: the write succeeds, the in-memory
state is refreshed, and the next operation reads the updated values without
requiring a round-trip to the API server.

The implication: for a chain of N state nodes where node B reads a field written
by node A, both fire in the same reconcile cycle. The chain advances N steps per
cycle rather than requiring N requeue cycles. The only case that requires a
requeue is when the same state node must read its own prior output (i.e., it
depends on itself across cycles), which is the bootstrap pattern described below.

##### CRD schema auto-injection

The Kubernetes API server silently discards fields not declared in the CRD
schema on `UpdateStatus` calls. For each distinct `storeName` referenced by any
state node in an RGD, kro's build phase automatically injects:

```yaml
status:
  properties:
    <storeName>:
      x-kubernetes-preserve-unknown-fields: true
```

into the generated CRD. This injection happens at build time so that the first
`UpdateStatus` call does not silently drop the written values. Multiple state
nodes targeting the same `storeName` result in exactly one injection.

##### Bootstrap guard

On the first reconcile, `status.<storeName>` does not exist. Any expression
that self-references a state field must guard with `has()`:

```yaml
- id: trackStep
  state:
    storeName: migration
    fields:
      step: "${has(schema.status.migration.step) ? schema.status.migration.step + 1 : 1}"
  includeWhen:
    - "${!has(schema.status.migration.step) || schema.status.migration.step < schema.spec.totalSteps}"
```

This pattern is consistent with existing kro practice for optional fields in
CEL expressions. The `has()` guard is required only for expressions that read a
field they themselves write. Expressions that only read from `spec` or from
sibling resources do not need it.

On first reconcile, the controller ensures `status: {<storeName>: {}}` is
injected as a baseline into the CEL context before evaluating state node
expressions, preventing "no such key" errors.

##### Concurrent writes to the same `storeName`

When two or more state nodes target the same `storeName`, the within-cycle
context refresh ensures they are not independent overwriting operations. The
sequence is:

1. The DAG processes state nodes in topological order.
2. Node A evaluates its `fields` expressions, producing `{step: 1}`. It reads
   the current `status.<storeName>` from the in-memory instance (e.g., `{}`),
   merges its computed values into a copy of that map, and issues an
   `UpdateStatus` call with the merged result: `{step: 1}`.
3. After the write succeeds, the controller refreshes the in-memory instance
   with the returned updated object. The instance now reflects
   `status.<storeName> = {step: 1}`.
4. Node B evaluates its `fields` expressions, producing `{phase: "reviewing"}`.
   It reads the current in-memory `status.<storeName>` — which is now `{step: 1}`
   — merges its values in, and issues `UpdateStatus` with
   `{step: 1, phase: "reviewing"}`. Node A's fields are preserved because the
   merge starts from the current live state, not from an empty map.

The key invariant: each state node's `UpdateStatus` call contains the full
current contents of `status.<storeName>` plus the node's own computed fields.
No prior node's writes are dropped.

If the `UpdateStatus` call encounters a resource version conflict (a concurrent
external write to the same instance), the controller performs a full retry:
re-fetch the live CR, re-evaluate all CEL expressions in the node's `fields`
map against the freshly-fetched instance, merge the new results into the
current `status.<storeName>`, and retry the call. Re-evaluation is required,
not optional. If the retry only re-merges the originally computed values, a
stale result can be written silently: consider a node computing
`step = schema.status.migration.step + 1` where `step` was `0` at first
evaluation. If a concurrent reconcile wrote `step = 1` between the read and
the write, a merge-only retry would write `step = 1` again instead of `step = 2`.
The counter is off by one with no error. Re-evaluating against the fresh
instance produces `step = 2` — the correct result.

##### Idempotency

Before issuing an `UpdateStatus` call, the controller compares the computed
field values against the current `status.<storeName>.*` values read from the
live CR. If all values already match, no patch is issued and no requeue is
scheduled. This prevents redundant API calls when the RGD re-reconciles due to
unrelated watch events.

##### Reserved `storeName` validation

The following names are reserved and cannot be used as `storeName` values:

- `state` — used by kro's own instance state tracking
- `conditions` — standard Kubernetes status conditions
- `managedResources` — kro's managed resource list

Attempting to use a reserved name produces an admission webhook rejection with
the message:
`state.storeName "<name>" is reserved by kro; choose a different name`.

This is enforced at admission time (kubebuilder validation rule), not at
runtime.

##### CEL type checking

State node expressions are compiled in parse-only mode (no type checking) for
v1. The typed CEL environment is built from the RGD's `schema` section, which
has no knowledge of `status.<storeName>.*` fields at compile time. Type errors
in state expressions are caught at runtime rather than at RGD admission time.
See the Discussion section for mitigations available to RGD authors and the v2
path to compile-time type inference.

##### Requeue behavior

When a state node writes values and the within-cycle context refresh is
complete, the controller does not automatically schedule a requeue unless one
of the following conditions holds:

1. A state node's `includeWhen` is true and its values differ from the live CR
   (the normal case on first write — the write succeeds and triggers an
   immediate requeue so the next cycle can evaluate the updated `includeWhen`).
2. Any state node wrote to `status` during the current reconcile — the
   controller schedules a delayed requeue so that cross-cycle self-referencing
   expressions (bootstrap pattern) get a chance to advance.

Status writes do not advance `spec.resourceVersion` and do not automatically
generate a watch event. The explicit requeue is required for the convergence
loop to make progress.

##### CEL evaluation context for state nodes

State nodes use a different CEL environment than all other node types in kro.

kro's standard CEL context deliberately strips `status` from the instance
object before building the context for `template:`, `readyWhen:`, and
`includeWhen:` expressions on ordinary resource nodes. The reasoning: child
resource templates should not depend on `status` (which can be stale or absent
on first reconcile) — they should depend on `spec` fields that the user controls.

State nodes invert this requirement. Their entire purpose is to read prior state
values from `status.<storeName>.*` and decide whether to fire based on them.
If the standard stripped context were used:

- `has(schema.status.migration.step)` would always return `false` (status is
  absent from the context), so `includeWhen` expressions that guard on prior
  state would always evaluate as if no state had ever been written.
- A state node with `includeWhen: "${!has(schema.status.foo.counter) || ...}"`
  would fire on every reconcile regardless of whether `counter` had already
  reached its target value — producing infinite requeue loops.

To prevent this, state nodes use a status-aware context variant for both their
`includeWhen` expressions and their `fields` expressions. This context includes
`schema.status.*` populated from the live instance CR, with all declared
`storeName` scopes pre-initialized to `{}` if absent (to avoid "no such key"
errors on first reconcile).

The implication for implementers: the `buildContext()` call site in the DAG
evaluation loop must branch on node type. State nodes call
`buildContextWithStatus()` (or equivalent); all other node types continue to
use the standard `buildContext()` that strips `status`. The two code paths share
everything except the status injection step — they are not separate environments,
just different context construction calls.

## Other solutions considered

**External bespoke controller (status quo)**

Write a separate controller that watches for a triggering condition, reads the
parent CR, computes the next state, and patches. This works and is what every
production kro user who needs stateful transitions is doing today. The cost: kro
stops being a complete solution for those workflows. For each new transition
rule, a separate controller binary must be written, deployed, and operated.

**KREP-011 data nodes**

Data nodes solve within-cycle reuse but are ephemeral — their values are not
persisted to the API server and are not visible in the next reconcile cycle.
They cannot express accumulation or any pattern that requires reading the result
of a prior reconcile.

**KREP-016 patch nodes targeting self**

KREP-016 patch nodes write to resources kro does not own. Extending them to
support self-targeting is possible but conflates two distinct semantics:
contributing fields to a peer resource vs. persisting computed state on self.
Keeping them separate maintains a cleaner mental model and avoids complexity in
the field manager assignment logic.

**External ConfigMap as scratch pad**

A kro-managed ConfigMap can hold arbitrary key-value data and would survive
across reconcile cycles. However, this pollutes the cluster with
controller-private implementation details, requires consumers to know about a
second resource, and makes the ownership model harder to reason about. State
belongs on the instance CR itself.

**Write to `spec` instead of `status`**

Writing to `spec` triggers a watch event which re-drives the controller
immediately — enabling a multi-step chain to resolve in a single logical
transaction without an explicit requeue. However, `spec` is the GitOps source
of truth. kro writing to `spec` creates a conflict with Argo CD and Flux, which
detect the mutation as drift and attempt to revert it. `ignoreDifferences` can
paper over this, but it requires per-field configuration and is fragile.
Writing to `status` avoids this entirely. The within-cycle context refresh
described in this proposal recovers most of the convergence speed benefit of
spec writes without the GitOps conflict.

## Scoping

#### What is in scope

- `state:` virtual node type with `storeName` and `fields`
- Within-cycle CEL context refresh after each state node write
- CRD schema auto-injection (`x-kubernetes-preserve-unknown-fields: true`) for each declared `storeName`
- `includeWhen` support on state nodes (with status-aware CEL context)
- Bootstrap `has()` guard pattern (documented; enforcement via `has()` is by convention)
- Idempotency: no patch when computed values already match live CR
- Reserved `storeName` validation at admission time
- Requeue-on-write for cross-cycle bootstrap cases
- DAG participation: state nodes can depend on child resource statuses and can be depended upon

#### What is not in scope

- Writing to `spec` — GitOps conflict; tracked separately
- Writing to other instance CRs or resources kro does not own — covered by KREP-016
- Transactional atomicity across multiple state nodes in a single `UpdateStatus` call
- Time-triggered transitions (no timer/TTL primitive)
- Compile-time type checking for `status.<storeName>.*` fields (v2 improvement)
- `forEach` on state nodes — the semantics of iterating a state write (one entry per iteration vs. merged result) are non-trivial and deferred to a follow-on proposal
- Cleanup of orphaned `storeName` data after a state node is removed from an RGD update — existing instances retain the stale `status.<storeName>` data; pruning is left for a follow-on proposal
- Admission-time warning for non-convergent expressions (expression reads and writes the same field without a terminating `includeWhen` gate)

## Testing strategy

#### Requirements

- A running kro-enabled Kubernetes cluster with the updated controller image
- RGD YAML fixtures for each validation scenario

#### Test plan

**Unit tests:**

- State node CEL evaluation: `EvaluateStateWrite()` produces correct output for arithmetic, array mutation, and conditional expressions
- Within-cycle context refresh: after a state node fires, subsequent nodes in the same reconcile see the updated `status.<storeName>.*` values
- `includeWhen` uses status-aware CEL context: `has(schema.status.<storeName>.field)` evaluates correctly
- CRD schema injection: `InjectStateField()` adds `x-kubernetes-preserve-unknown-fields: true` for each declared `storeName`, exactly once per name
- Reserved `storeName` rejection: kubebuilder validation rule fires for `state`, `conditions`, `managedResources`
- Idempotency: second call with identical computed values issues no `UpdateStatus` call

**Integration tests (Chainsaw):**

1. **Bootstrap**: first reconcile with empty `status.<storeName>` writes initial values correctly; `has()` guard evaluates false, expression uses default value
2. **Cross-cycle accumulation**: a counter field increments correctly across N reconcile cycles; after reaching `totalSteps`, `includeWhen` becomes false and no further patches are issued
3. **Within-cycle chaining**: two state nodes A → B in DAG order, where B reads a field written by A; both fire in the same reconcile cycle; the instance reaches the expected state in one cycle
4. **`includeWhen: false`**: state node is skipped; existing `status.<storeName>` values are preserved unchanged
5. **Concurrent `storeName` writes**: two state nodes targeting the same `storeName` write disjoint fields; the retry-on-conflict path ensures both sets of fields are present in the final status without one node's write dropping the other's fields
6. **Reserved `storeName` rejection**: RGD with `storeName: conditions` is rejected at admission with the expected error message
7. **Multi-`storeName`**: two state nodes targeting different `storeNames` coexist without collision; CRD schema contains both injected fields
8. **Requeue correctness**: a self-referencing counter (bootstrap pattern) advances once per cycle and converges to the target value

## Discussion and notes

**On reconcile amplification**

The within-cycle context refresh addresses the primary latency concern for
chained state transitions. For a chain of N state nodes where the DAG can be
resolved in topological order within a single reconcile, the chain converges
in one cycle regardless of N. The amplification concern only applies to
self-referencing (cross-cycle) patterns, where each step requires one requeue.
For platform use cases — cert rotation scheduling, retry counters, progressive
delivery step tracking — the cross-cycle case is typical, and the latency
(~100–500ms per step on a lightly loaded cluster) is well within the acceptable
range. These transitions happen over minutes or hours, not milliseconds.

**On the `has()` bootstrap verbosity**

The required `has(schema.status.<storeName>.fieldName) ? ... : initialValue`
pattern is verbose. A future improvement could provide a helper function:
`kstate(storeName, fieldName, defaultValue)`. This would handle the
absent-on-first-reconcile case transparently and improve readability. It is out
of scope for this proposal.

**On CEL type safety**

The parse-only compilation for state expressions is a real trade-off. In v1,
a typo in a state field name — `schema.status.migration.stpe` instead of
`schema.status.migration.step` — passes admission without error and produces
a silent `nil` value at runtime. The expression evaluates, the write succeeds,
and the instance reaches a broken state with no indication of what went wrong
at the RGD level.

Two mitigations available to RGD authors in v1:

1. **Test with a unit test fixture** that applies the RGD to a known instance
   state and asserts the expected `status.<storeName>.*` values after
   reconciliation. This is the same pattern used for testing `readyWhen` and
   `includeWhen` expressions and catches typos before production deployment.

2. **Use `has()` guards defensively**: if an expression references a
   `schema.status.<storeName>.*` field, guarding with `has()` prevents a nil
   dereference from propagating and makes the failure mode visible as a missing
   value rather than a panic.

The v2 path to compile-time type checking is feasible: at RGD admission time,
derive a partial type schema from the declared `fields` map across all state
nodes targeting the same `storeName`. The `fields` map is static (declared in
the RGD YAML, not computed at runtime), so inferred types from the declared
expressions (integer arithmetic → `int`, string concatenation → `string`) can
populate a supplementary CEL type environment entry for
`schema.status.<storeName>.*`. This would not require changes to the RGD API
surface and could be implemented as a follow-on without breaking existing RGDs.

**On the `status` write mechanics**

kro's `updateStatus()` builds a completely fresh status map from scratch
(conditions + instance state + CEL-resolved fields) and replaces the entire
`status` object. Without explicit preservation, this overwrites any
`<storeName>` values written by state nodes earlier in the same reconcile.

Responsibility for preservation belongs to `updateStatus()` itself, not to
the state node processor. The RGD build phase already knows which `storeName`
values are declared across all state nodes — it uses this information for CRD
schema injection. That same set must be threaded through to the reconciler
context so that `updateStatus()` can read the current values of each declared
`storeName` from the in-memory instance and merge them into the new status map
before issuing the final `UpdateStatus` call. This applies to both the normal
`updateStatus` code path and the `RetryOnConflict` fetch-before-write path
inside it. No changes to the RGD API are required — the set of declared
`storeName` values is already derivable at build time.

**Precedent**

The `state:` node pattern is analogous to Crossplane's `status.atProvider` — a
controller-owned sub-object within `status` that accumulates observed state
without conflicting with the user-owned `spec`. The named `storeName` scoping
is consistent with how Crossplane isolates provider-managed state from
user-visible conditions.

## Open questions

1. **`storeName` required or optional?** This proposal requires it explicitly.
   Defaulting to a kro-chosen name (e.g., `kstate`) would reduce verbosity for
   simple cases but creates a shared namespace collision risk when multiple
   independent features in one RGD each need their own state. Explicit names
   are self-documenting and prevent accidental cross-node field clobbering.

2. **Type-checked `fields` at admission time?** This proposal uses parse-only
   for v1 with a documented v2 path (see Discussion). If maintainers prefer to
   require type checking before the feature ships, the partial type inference
   approach described in Discussion is the recommended path — it does not
   require changes to the RGD API surface.
